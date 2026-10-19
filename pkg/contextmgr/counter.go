package contextmgr

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/pkoukk/tiktoken-go"
)

// ============================================================
// Token 计数器
// ============================================================
//
// 精度分两档，按可用性自动降级：
//
//  1. tiktoken（o200k_base）—— 精确 BPE 分词。对 OpenAI/Azure 系模型是
//     厂商同源算法，计数与计费完全一致；对混元/GLM/Gemini 属于「同族近似」，
//     中文场景误差可达 ±20%，这也是 TriggerRatio 取 0.8 的原因。
//  2. 启发式 —— 按字符类型分档估算。仅在 tiktoken 初始化失败时兜底。
//
// 为什么需要兜底：tiktoken-go 首次使用某编码时要从 OpenAI 公网 blob
// 下载 BPE 词典。本服务跑在内网，下载可能直接失败。届时若让计数器返回
// error 并中断调用链，等于让「上下文管理」这个增强特性反过来搞挂主流程。
// 所以降级成估算 —— 精度下降但链路不断。
//
// 同一套兜底也覆盖「词典正在后台下载」的启动窗口，见 GetCounter / WarmCounter。

// tiktokenEncoding 统一用 o200k_base。
//
// 它是当前 OpenAI 系（gpt-4o 及以后）的编码，对中文的压缩率明显优于
// 老的 cl100k_base（后者常把一个汉字切成 2-3 个 token，用来估中文
// 上下文会严重高估）。既然要用一套编码近似所有 provider，选 o200k。
const tiktokenEncoding = "o200k_base"

// Counter 统计消息与工具定义的 token 数。
//
// 并发安全：tiktoken-go 的 Encode 不是并发安全的（内部有可变状态），
// 而本计数器会被多个 task 的调度协程同时调用，因此所有 Encode
// 都在 mu 保护下进行。
type Counter struct {
	mu  sync.Mutex
	enc *tiktoken.Tiktoken
	// degraded 标记已降级为启发式估算（tiktoken 初始化失败）。
	degraded bool
}

var (
	globalCounter     atomic.Pointer[Counter]
	globalCounterOnce sync.Once
)

// GetCounter 返回进程内共享的计数器。
//
// 编码对象构建一次（要解析几十万条 BPE 规则，不可每次新建），
// 之后所有调用复用。初始化失败时返回已降级的计数器，而非 nil ——
// 调用方不必处理 error，拿到的永远是个可用对象。
//
// 首次调用**立即返回**：先挂上启发式估算的兜底计数器，精确编码在后台加载，
// 完成后原子替换。原因是词典加载要下载 o200k_base（约 3.4MB）并解析几十万条
// BPE 规则，慢网下实测可耗时近一分钟（见 WarmCounter）—— 若同步等待，这段
// 时间会落在「用户首个任务」的上下文组装上，用户看到的是几十秒无响应。
// 降级窗口内计数走启发式估算，与本包既有的失败兜底口径一致。
func GetCounter() *Counter {
	globalCounterOnce.Do(func() {
		globalCounter.Store(&Counter{degraded: true})
		go loadEncoder()
	})
	return globalCounter.Load()
}

// WarmCounter 预热计数器：在服务启动期提前触发词典下载与 BPE 规则解析，
// 把首次使用的昂贵开销从「用户首个任务」挪到启动阶段。
//
// 内部走与首次调用同一条路径，本身不阻塞；调用方仍应用 goroutine 包一层，
// 与 mcppkg.PrewarmBuiltins 的启动预热并排执行。
func WarmCounter() { GetCounter() }

// loadEncoder 后台下载并解析 BPE 词典，成功后原子替换全局计数器。
//
// 失败只降级记日志：计数器保持启发式估算，上下文管理链路不断
// （tiktoken-go 首次使用某编码要从 OpenAI 公网 blob 拉词典，内网可能不可达）。
func loadEncoder() {
	pinCacheDir()
	enc, err := tiktoken.GetEncoding(tiktokenEncoding)
	if err != nil {
		log.Printf("[contextmgr] tiktoken 初始化失败，降级为启发式估算: %v", err)
		return
	}
	globalCounter.Store(&Counter{enc: enc})
	log.Printf("[contextmgr] tiktoken 就绪（%s），token 计数切换为精确 BPE 分词", tiktokenEncoding)
}

// pinCacheDir 把 BPE 词典缓存目录固定到用户缓存目录。
//
// tiktoken-go 默认缓存在 os.TempDir()/data-gym-cache —— 系统重启或
// 临时目录清理后即失效，于是每次服务启动都要重新从 OpenAI 公网 blob
// 下载一次词典（实测首次约 2.7s，内网环境可能直接失败并触发降级）。
//
// 固定到 os.UserCacheDir() 下，让词典只下载一次、跨重启复用。
// 已由外部显式设置 TIKTOKEN_CACHE_DIR 时不覆盖 —— 部署方可能刻意
// 挂载了持久化卷或预置了离线词典。
func pinCacheDir() {
	if os.Getenv("TIKTOKEN_CACHE_DIR") != "" || os.Getenv("DATA_GYM_CACHE_DIR") != "" {
		return
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return // 拿不到就沿用库的默认行为，不阻断
	}
	dir := filepath.Join(base, "flowagent", "tiktoken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.Setenv("TIKTOKEN_CACHE_DIR", dir)
}

// Degraded 是否处于启发式降级状态（供日志/调试页展示精度口径）。
func (c *Counter) Degraded() bool { return c.degraded }

// CountText 统计一段纯文本的 token 数。
func (c *Counter) CountText(s string) int {
	if s == "" {
		return 0
	}
	if c.degraded || c.enc == nil {
		return estimateTokens(s)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.enc.Encode(s, nil, nil))
}

// ============================================================
// 消息结构开销
// ============================================================

// 每条 message 除内容本身还有固定的协议开销：role 标记、分隔符、
// 消息边界 token。OpenAI cookbook 给 chat 格式的经验值是每条 +3、
// 带 name 再 +1，整个请求末尾再 +3（assistant 起始引导）。
//
// 各家协议（Gemini contents / Claude messages）的实际开销不同，
// 但都在同一量级。这里沿用 OpenAI 口径作为统一近似 —— 相比动辄
// 几十万 token 的正文，这点差异可以忽略。
const (
	tokensPerMessage = 3
	tokensPerName    = 1
	tokensPerRequest = 3
)

// CountMessages 统计一组消息的 token 数（含结构开销）。
//
// 会把 tool_calls 的函数名与参数、tool 结果的工具名一并计入 ——
// 这些都会真实占用输入窗口，漏算会低估水位。
func (c *Counter) CountMessages(messages []llm.Message) int {
	total := 0
	for i := range messages {
		total += c.countMessage(&messages[i])
	}
	if len(messages) > 0 {
		total += tokensPerRequest
	}
	return total
}

// countMessage 统计单条消息。
func (c *Counter) countMessage(m *llm.Message) int {
	n := tokensPerMessage
	n += c.CountText(string(m.Role))
	n += c.CountText(m.Content)

	// tool 结果消息带工具名，算作 name 字段
	if m.ToolName != "" {
		n += c.CountText(m.ToolName) + tokensPerName
	}

	// assistant 的工具调用：函数名 + 参数 JSON 都会序列化进请求
	for _, tc := range m.ToolCalls {
		n += c.CountText(tc.Name) + tokensPerName
		// Arguments 是 map 形态，按序列化后的 JSON 计数 —— 那才是上线形态
		if tc.Arguments != nil {
			if b, err := json.Marshal(tc.Arguments); err == nil {
				n += c.CountText(string(b))
			}
		}
		// Signature（Gemini 3 的思维签名）是不透明 base64，体量可观，
		// 必须计入 —— 它随每条历史 assistant 消息回放。
		n += c.CountText(tc.Signature)
	}

	// 图像部分：多模态输入的 token 由 provider 按分辨率折算，
	// 本地无法精确计算。这里按每张图一个保守常量记账，
	// 避免「带图对话的水位被严重低估」。
	n += len(m.Images) * tokensPerImage

	return n
}

// tokensPerImage 单张图片的保守 token 估值。
//
// 各家折算规则差异很大（Gemini 按 768x768 分块、OpenAI 按 512 分块），
// 且与实际分辨率相关，本地拿不到。取 1500 作为「中等尺寸截图」的
// 量级近似：宁可高估，让压缩早触发。
const tokensPerImage = 1500

// CountTools 统计工具定义（schema）的 token 数。
//
// 工具 schema 是容易被忽略的大头：几十个 MCP 工具、每个带完整
// JSON Schema，合计能占掉几千到上万 token，且每轮请求全量重发。
func (c *Counter) CountTools(tools []llm.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	total := 0
	for _, t := range tools {
		total += c.CountText(t.Name)
		total += c.CountText(t.Description)
		// Parameters 是 map[string]interface{} 形态的 JSON Schema。
		// 序列化后计数：这正是它上线时的形态。
		if t.Parameters != nil {
			if b, err := json.Marshal(t.Parameters); err == nil {
				total += c.CountText(string(b))
			}
		}
		// 每个工具定义的结构开销（名称/描述/参数三个字段的包裹）
		total += tokensPerMessage
	}
	return total
}

// ============================================================
// 启发式估算（tiktoken 不可用时的兜底）
// ============================================================

// estimateTokens 按字符类型分档估算 token 数。
//
// 依据：
//   - CJK 字符：o200k_base 下常见汉字多为 1 token，生僻字 2 token，
//     取 0.7 token/字（即约 1.4 字/token）作为含标点的综合值。
//   - ASCII：英文约 4 字符/token；代码与 JSON 因符号密集更费，
//     取 3.5 字符/token 略偏保守。
//
// 误差约 ±20%，仅用于保证降级可用，不作为计费依据。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	// +0.999 向上取整，避免短字符串被估成 0
	return int(float64(cjk)*0.7 + float64(other)/3.5 + 0.999)
}

// isCJK 判断是否为中日韩字符（含全角标点）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK 标点
		(r >= 0xFF00 && r <= 0xFFEF) // 全角字符
}

// TrimToTokens 把文本截到不超过 maxTokens，保留头部。
//
// 用于约束压缩摘要的体量：LLM 不一定严格遵守「不超过 N token」的指令，
// 需要一道硬性兜底。按 rune 二分逼近，避免逐 token 解码的开销。
func (c *Counter) TrimToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	if c.CountText(s) <= maxTokens {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if c.CountText(string(runes[:mid])) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimSpace(string(runes[:lo])) + "\n…（摘要超长，已截断）"
}
