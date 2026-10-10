package actuator

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// 主 agent 网页抓取（fetch）配额
// ============================================================
//
// 背景：主 agent 一度完全不暴露 fetch（只有 deep_search 子任务里有），
// 目的是防止模型在主循环里反复抓网页把上下文撑爆。实测结果相反 ——
// 模型需要某个具体 URL 的原文时没有正当工具可用，就退回 run_command + curl
// 自己抓：抓取次数更多（一次循环能抓四五个 URL）、内容没有 Markdown 转换
// 与长度上限、行为完全不可观测。
//
// 因此改为「给工具、上约束」：
//  1. 配额：每个任务最多 fetchQuotaPerTask 次，用完返回引导文本而非报错，
//     模型可据此改用 deep_search（它内部的抓取不占本配额）或基于已有信息收口；
//  2. 长度：单次抓取的 max_length 被 clamp 到 fetchMaxLength，防止单次灌满上下文。
//
// 只对「主 agent / DAG 节点子任务」生效：deep_search 子任务是专门干检索的，
// 它的多轮抓取本就是设计意图，且结论会被压缩成摘要再回到主对话，不适用本配额。

// fetchQuotaPerTask 单个任务允许的主 agent 网页抓取次数。
//
// 取值考量：正常调研里「已知 URL 要读原文」的场景 3~5 次足够；
// 给到 8 是留出「抓到一个页面发现要跳下一个」的余量，又不至于让主对话
// 退化成抓取循环。真需要大范围检索时应走 deep_search。
const fetchQuotaPerTask = 8

// fetchMaxLength 主 agent 单次抓取允许的最大字符数。
//
// mcp-server-fetch 的默认 max_length 只有 5000，模型常会传更大的值
// （见过传 50000+）；不设上限的话一次抓取就能吃掉上万 token 的上下文，
// 而主对话的上下文还要留给历史与产物，比搜索子任务金贵得多。
//
// 定为 8000 而非更早的 20000：配额是 8 次，上限 × 配额决定了单个任务能被
// 抓取塞进多少上下文 —— 20000 时理论天花板约 100k token，小窗口模型下几次
// 抓取就能把「当前任务交互」撑爆（实测任务 7c09cf38 该块达 13743 tok）。
// 8000 字符约 5k token，仍比 MCP 默认的 5000 宽 60%，足够覆盖绝大多数文章
// 的核心内容；8 次全用满的累计天花板从 ~100k 降到 ~40k，与压缩后的空间更匹配。
const fetchMaxLength = 8000

// fetchBudget 一个任务的抓取用量。
//
// 必须加锁：一次 LLM 响应里的多个 tool_call 是并行执行的，
// 并发拿到同一个 budget 后 used++ 会 data race。
type fetchBudget struct {
	mu       sync.Mutex
	used     int
	lastSeen time.Time
}

// fetchBudgets 任务维度的抓取用量表。
//
// 任务终态后条目不再被访问，靠惰性回收清掉（见 maybeSweepFetchBudgets）：
// 调度器是长驻进程，不回收会随任务数无界增长。
var fetchBudgets sync.Map // taskID -> *fetchBudget

// fetchBudgetTTL 用量条目的过期时间：超过它没被访问即视为任务已结束。
const fetchBudgetTTL = 6 * time.Hour

// fetchBudgetSweepEvery 每累计这么多次配额申请做一次惰性回收，摊薄扫描成本。
const fetchBudgetSweepEvery = 256

// fetchSweepCounter 配额申请计数器（原子自增，只用于触发回收，允许偏斜）。
//
// 必须原子：一次 LLM 响应里的多个 tool_call 并行执行，并发 ++ 是 data race
// （-race 下实测会直接报错）。
var fetchSweepCounter atomic.Int64

// takeFetchQuota 申请一次抓取配额，返回 (本次是第几次, 是否放行)。
//
// 放行的同时就把用量记掉 —— 调用方拿到 true 就一定会发起抓取，
// 「先扣后抓」才不会被并发的批量 tool_call 钻空子。
func takeFetchQuota(taskID string) (int, bool) {
	if taskID == "" {
		// 拿不到任务维度就退化为「不计量但放行」：配额是防滥用的护栏，
		// 不是功能开关，不该因为缺个 ID 就废掉抓取能力。
		return 0, true
	}

	now := time.Now()
	v, _ := fetchBudgets.LoadOrStore(taskID, &fetchBudget{lastSeen: now})
	b := v.(*fetchBudget)

	b.mu.Lock()
	b.used++
	b.lastSeen = now
	used := b.used
	b.mu.Unlock()

	maybeSweepFetchBudgets(now)
	return used, used <= fetchQuotaPerTask
}

// fetchQuotaExceededMessage 配额用尽时回喂给模型的话术。
//
// 必须给出明确的替代路径，否则模型只会原地重试同一个工具：
// deep_search 内部的抓取不占这里的配额，正是它该走的路。
func fetchQuotaExceededMessage() string {
	return fmt.Sprintf(
		"本任务的网页抓取配额已用尽（每任务最多 %d 次）。请不要再调用 fetch，改为："+
			"1) 用已抓到的信息继续推进；2) 若还需大范围联网调研，改用 deep_search"+
			"（它内部的抓取不占本配额，且返回收口后的结论而不是原始正文）。",
		fetchQuotaPerTask)
}

// clampFetchMaxLength 把抓取参数里的 max_length 压到上限以内。
//
// 只收紧、不放大：模型没传时不替它填，交给 MCP server 自己的默认值。
func clampFetchMaxLength(args map[string]interface{}) {
	clampFetchMaxLengthTo(args, fetchMaxLength)
}

// fetchMaxLengthDeepSearch deep_search 子任务里抓取的单页字符上限。
//
// 为什么不是主 agent 的 8000：
//   - deep_search 是专门的检索子任务，靠「多来源交叉」出结论，单页信息量
//     需要比主对话宽，8000 会把长文的关键段落切掉；
//   - 但也不能不管 —— 限额之外没有任何长度约束时，实测单次抓取能返回
//     201KB（一次调研累计 12.2MB），既撑爆 DB 也让回喂的截断落在无意义的位置。
//
// 24000 字符约 15k token，够覆盖一篇长文的核心内容；叠加子任务的工具调用
// 预算（max_rounds × 3），累计天花板可控。
const fetchMaxLengthDeepSearch = 24000

// clampFetchMaxLengthTo 把抓取参数里的 max_length 压到 limit 以内。
func clampFetchMaxLengthTo(args map[string]interface{}, limit int) {
	if args == nil || limit <= 0 {
		return
	}
	v, ok := args["max_length"]
	if !ok {
		return
	}
	if n, ok := toIntValue(v); ok && n > limit {
		args["max_length"] = limit
	}
}

// fetchLengthLimitFor 取当前任务抓取单页的字符上限。
//
// deep_search 子任务放宽（它的抓取是本配额要保护的对象之外，但仍需限长），
// 其余任务用主 agent 的 fetchMaxLength。
func fetchLengthLimitFor(temp *model.TaskTemp) int {
	if temp != nil && isDeepSearchSubTask(temp) {
		return fetchMaxLengthDeepSearch
	}
	return fetchMaxLength
}

// capFetchedContent 抓取类工具返回的内容级限长（参数 clamp 的兜底）。
//
// 为什么参数 clamp 之外还要截内容：MCP server 不一定严格遵守 max_length
// （实测有返回 201KB 的情况），参数侧限长对这类 server 无效。内容侧兜底后，
// 无论 server 行为如何，单次抓取进上下文与进库的上限都是确定的。
//
// 保留头尾（头 80% / 尾 20%）并标注省略量：正文的关键结论多在开头，
// 结尾常是参考链接与附录，两头都丢不得。
func capFetchedContent(content string, limit int) string {
	if limit <= 0 {
		return content
	}
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	tail := limit / 5
	if tail < 1 {
		tail = 1
	}
	head := limit - tail
	return fmt.Sprintf("%s\n…(正文过长，中间 %d 字符已省略)…\n%s",
		string(runes[:head]), len(runes)-limit, string(runes[len(runes)-tail:]))
}

// toIntValue 把 JSON 反序列化后的数字（float64 / int / int64 …）统一成 int。
// 与 workspace 包的 intArg 重复，但那是包内私有函数，此处不便跨包依赖。
func toIntValue(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// maybeSweepFetchBudgets 惰性回收过期条目。
//
// 每 fetchBudgetSweepEvery 次申请扫一次全表，删掉 TTL 内没再被访问的条目。
func maybeSweepFetchBudgets(now time.Time) {
	if fetchSweepCounter.Add(1)%fetchBudgetSweepEvery != 0 {
		return
	}
	fetchBudgets.Range(func(k, v interface{}) bool {
		b, ok := v.(*fetchBudget)
		if !ok {
			fetchBudgets.Delete(k)
			return true
		}
		b.mu.Lock()
		stale := now.Sub(b.lastSeen) > fetchBudgetTTL
		b.mu.Unlock()
		if stale {
			fetchBudgets.Delete(k)
		}
		return true
	})
}

// resetFetchBudgets 清空全部用量（仅测试使用）。
func resetFetchBudgets() {
	fetchBudgets.Range(func(k, _ interface{}) bool {
		fetchBudgets.Delete(k)
		return true
	})
}
