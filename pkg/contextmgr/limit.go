// Package contextmgr 提供上下文水位度量与压缩决策的基础设施。
//
// 职责边界：
//   - 本包只回答「有多少 token」「上限是多少」「该不该压」三类问题，
//     不负责「怎么压」—— 压缩需要重建历史、调 LLM、写库，那属于
//     schedule/actuator 的职责（它才持有 DAO 与 LLM 客户端）。
//   - 本包不依赖 DAO，只依赖 pkg/llm 的消息类型，保持可单测。
package contextmgr

import "strings"

// ============================================================
// 上下文上限解析
// ============================================================

// DefaultContextLimit 模型未配置 context_limit 时的兜底上限（token）。
//
// 刻意取一个偏保守的值：当前主流模型的输入上限普遍 ≥ 128K，
// 取 128K 意味着「漏配时压缩会偏早触发」——浪费一点压缩开销，
// 但不会把请求打爆。反过来若取 1M 这种乐观值，漏配一个小窗口模型
// 就会直接撞上 provider 的硬限制而整轮失败。宁可早压，不可溢出。
const DefaultContextLimit = 128000

// modelContextLimits 已知模型的最大输入 token 数。
//
// 口径统一为「最大输入」而非「总窗口」，与 model.ModelConfig.ContextLimit
// 一致：总窗口通常 = 输入上限 + 输出上限，按总窗口做水位检查会高估可用空间。
//
// key 为小写模型名。查表用前缀匹配（见 lookupByModelName），
// 这样 hy4-preview / hy4-preview-0828 这类带日期后缀的变体能自动命中。
//
// 各厂商标注习惯不同，取值时必须认准口径（2026-09 逐个核对官方文档）：
//
//  1. 明确标注「最大输入」的，直接取该值：
//     - 腾讯混元：窗口与输入是两个数。hy4-preview 窗口 1024k = 输入 960k + 输出 64k；
//     hy3 差距更大，窗口 256k 但输入上限只有 192k。
//     - Google Gemini：官方标「输入 token 限制 1,048,576 / 输出 65,536」，
//     1M 就是输入口径，与窗口同值。
//     - 智谱 GLM：官方标「最大输入长度 1048576 / 最大输出 131072 /
//     上下文长度 1048576」，输入与窗口同值。
//     - OpenAI（含 Azure）：官方同时给「Context Window 1,050,000」与
//     「Input: 922,000 / Output: 128,000」，必须取后者。
//
//  2. 只标「上下文窗口 + 最大输出」的（Anthropic），按「窗口 - 最大输出」推算输入上限：
//     官方口径是输入与输出共享窗口，不单独给输入上限。取 窗口-输出 是保守做法，
//     保证输出打满也不会溢出。
//
// 注意 OpenAI 系输入超过 272K 后会进入更高计价档（仅影响成本，不影响可用性）。
var modelContextLimits = map[string]int{
	// 腾讯混元（口径：最大输入，严格小于窗口）
	"hy4-preview": 983040, // 960k；窗口 1024k / 输出 64k
	"hy4":         983040,
	"hy3-preview": 196608, // 192k；窗口 256k / 输出 128k —— 与窗口差 64k
	"hy3":         196608,

	// Google Gemini（口径：输入限制，与窗口同值）
	"gemini-3.1-pro":   1048576,
	"gemini-3-pro":     1048576,
	"gemini-3.5-flash": 1048576,
	"gemini-2.5-pro":   1048576,

	// OpenAI / Azure OpenAI（口径：窗口 1.05M 中的输入侧 922K）
	"gpt-6-astra":   922000,
	"gpt-5.6-sol":   922000,
	"gpt-5.6-terra": 922000,
	"gpt-5.6-luna":  922000,
	"gpt-5.6":       922000,

	// Anthropic Claude（口径：窗口共享，取 窗口 - 最大输出）
	"claude-fable-5-1":  917504, // 1M - 128K
	"claude-opus-5":     917504,
	"claude-sonnet-5":   917504,
	"claude-opus-4-8":   393216, // 500K - 128K
	"claude-opus-4-7":   393216,
	"claude-opus-4-6":   393216,
	"claude-sonnet-4-6": 393216,
	"claude-haiku-4-5":  139264, // 200K - 64K

	// 智谱 GLM（口径：最大输入，与窗口同值）
	"glm-5.3-flash": 1048576,
	"glm-5.3":       1048576,
	"glm-4.6v":      131072, // 多模态版窗口 128K
	"glm-4.6":       204800, // 窗口 200K（由 128K 扩展而来）
}

// ResolveContextLimit 解析某个模型的最大输入 token 数。
//
// 优先级：显式配置（model_config.context_limit）> 内置模型名查表 > 保守默认值。
//
// configured 来自 DB，为 0 表示未配置。modelName 为厂商模型名
// （如 hy4-preview / gpt-5.6-sol），用于在漏配时按前缀查内置表兜底 ——
// 这样新增模型即使忘了填上限，只要模型名在表里也能拿到正确值。
func ResolveContextLimit(configured int, modelName string) int {
	if configured > 0 {
		return configured
	}
	if n := lookupByModelName(modelName); n > 0 {
		return n
	}
	return DefaultContextLimit
}

// lookupByModelName 按模型名查内置上限表，未命中返回 0。
//
// 用最长前缀匹配而非精确匹配：厂商常在同一模型上挂日期/版本后缀
// （hy4-preview-0828、gpt-5.6-sol-2026-08-01），精确匹配会全部落空。
// 取最长匹配保证 glm-5.3-flash 不会被更短的 glm-5.3 抢先命中。
func lookupByModelName(modelName string) int {
	name := strings.ToLower(strings.TrimSpace(modelName))
	if name == "" {
		return 0
	}
	bestLen, best := 0, 0
	for k, v := range modelContextLimits {
		if strings.HasPrefix(name, k) && len(k) > bestLen {
			bestLen, best = len(k), v
		}
	}
	return best
}

// ============================================================
// 水位阈值
// ============================================================

// 压缩决策的三个水位线，均为「占上限的比例」。
//
//   - TriggerRatio  触发压缩的水位。取 0.8 而非 1.0 是为了吃掉 tiktoken
//     对非 OpenAI 模型（混元/GLM/Gemini 用的不是 o200k BPE）的计数误差，
//     实测这类误差在中文场景可达 ±20%。
//   - TargetRatio   压缩循环的理想目标：压到上限的 30% 以下才停。
//     留出足够余量给后续若干轮工具调用，避免压完一轮又马上触发。
//   - CeilingRatio  降级底线。不可压部分（system_prompt + tools + 工作流摘要）
//     本身就可能超过 TargetRatio，此时 TargetRatio 永远达不成。
//     只要压到 CeilingRatio 以下就算通过，否则才报错。
const (
	TriggerRatio = 0.8
	TargetRatio  = 0.3
	CeilingRatio = 0.8
	// SummaryRatio 单份「已压缩历史摘要」的体量上限（占上限比例）。
	// 摘要自身必须受控，否则压几轮之后摘要本身就成了新的膨胀源。
	SummaryRatio = 0.1
	// CompactCallRatio 压缩调用自身可占用的输入预算（占上限比例）。
	//
	// 压缩是「用 LLM 压 LLM 的输入」，它自己也在同一个上下文预算里 ——
	// 从大模型切到小模型后，待压缩素材可能远超新模型的上限，一次调用
	// 必然被 provider 拒绝。故压缩调用的输入也必须留水位：0.6 输入 +
	// 0.1 输出 = 0.7，剩余 30% 用于吸收 tiktoken 对非 OpenAI 模型的
	// 计数误差（实测中文场景可达 ±20%）与 provider 的额外开销。
	CompactCallRatio = 0.6
)

// Thresholds 把比例换算成具体 token 数，避免调用方到处写浮点乘法。
type Thresholds struct {
	Limit       int // 上下文上限（最大输入 token）
	Trigger     int // 超过则触发压缩
	Target      int // 压缩循环的理想目标
	Ceiling     int // 降级底线
	Summary     int // 摘要体量上限
	CompactCall int // 单次压缩调用的输入预算（素材放不下时按此分块）
}

// NewThresholds 按上限算出各档水位。
func NewThresholds(limit int) Thresholds {
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	return Thresholds{
		Limit:       limit,
		Trigger:     int(float64(limit) * TriggerRatio),
		Target:      int(float64(limit) * TargetRatio),
		Ceiling:     int(float64(limit) * CeilingRatio),
		Summary:     int(float64(limit) * SummaryRatio),
		CompactCall: int(float64(limit) * CompactCallRatio),
	}
}
