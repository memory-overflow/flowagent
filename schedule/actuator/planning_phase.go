// Package actuator — functional_call 阶段的「规划期」实现
//
// 规划期（model.PhasePlanning）与执行期共享 functional_call 这一个调度阶段，
// 差异只在 prompt 与收口方式：
//
//	prompt ：规划器人格 + 对话已累积 workflow + 可用工具能力清单
//	工具   ：真实工具（MCP / Skill）+ submit_plan 协议工具
//	收口   ：模型调用 submit_plan → 执行器拦截 → 落库 plan → 按 mode 分流
//
// 关键设计：规划期允许调真实工具。
// 拆解任务前往往需要先看现状（查目录、搜资料、读文件），
// 只让模型"闭眼拆解"必然拆得不准。规划期直接复用
// functional_call ↔ tool_call 循环：模型先调工具调研，
// 调研够了再调 submit_plan 提交规划，无需为规划单独复制一套循环。
//
// submit_plan 之后按 mode 分三条路（见 finishPlanning）：
//
//	direct_reply 规划器已直接给出答案（submit_plan.reply），落库为回复后当轮收口，
//	             不再进执行期 —— 闲聊 / 常识问答这类任务省掉一整轮执行期调用
//	single_agent 切执行期，当前对话任务继续 agent 循环产出答案
//	workflow     节点并入对话绑定的工作流任务（没有则新建并绑定），
//	             当前对话任务写一段「执行流程概述」后立即收口 —— 不再跑单 Agent，
//	             真正的 DAG 执行由工作流任务负责（见 workflow_binding.go）
//
// 对话级 workflow 语义（v1.4 起一个对话可以有多条）：
//   - DAG 存在工作流任务的 task_output，对话用 conversation.workflow_task_id
//     指向「当前激活」的那条；全集按 chat_id + task_type=workflow 查
//   - 每次规划时把全部候选工作流作为上下文注入 prompt，由模型决定
//     workflow_action=append（续接哪条）还是 new（另起一条）
//   - LLM 只输出「本次新增节点」，后端负责合并
//   - 续接时新节点的 depends_on 只能引用「可依赖锚点」（见 model.WorkflowAppendPolicy），
//     依赖了废弃节点会被判为 replanError 并带反馈重规划
package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	skillpkg "github.com/memory-overflow/flowagent/pkg/skill"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
	"github.com/memory-overflow/flowagent/schedule/container"
)

// planningMaxToolCallRound 规划期允许的最大工具调研轮次。
//
// 规划期与执行期共享同一个 tool_call 轮次计数（都统计 tool_call turn 数），
// 若不单独限制，模型可能把全部预算耗在调研上，真正干活时无轮次可用。
// 达到该轮次仍未 submit_plan 时强制降级切执行期。
const planningMaxToolCallRound = 4

// planningMaxReplanRounds 规划期因「输出节点校验」不过而允许的最大重规划次数。
//
// 与降级不同：多输出节点 / 无输出节点的规划是可以靠回喂失败原因修复的，
// 直接降级 single_agent 会丢掉工作流拆解。这里给有限的重试上限，
// 超过后与其他协议违规一样降级 single_agent，避免模型陷入无限重规划。
const planningMaxReplanRounds = 3

// ============================================================
// 提示词
// ============================================================

// planningSystemPrompt 告诉 LLM 做什么。
// 结构化约束由 tool schema 强制，这里只做业务说明。
//
// 关于「调研」段落的措辞（不要随意改回"可选/克制"）：
// 早期版本写成「调研是可选的、要克制、1~2 轮内收敛」，实测导致模型
// 遇到不认识的专有名词时直接 submit_plan(reason="需求模糊，应先向用户澄清")，
// 把本该自己查的事推给用户。规划器手里有搜索/抓取工具却反问用户，
// 是最糟糕的体验。因此这里改成硬约束：能查的必须先查，
// 且明确禁止把「向用户澄清客观信息」当作规划理由。
const planningSystemPrompt = `你是一个对话级任务规划器。一个对话可以承载多条相互独立的工作流（DAG），每条工作流对应一类相对完整的任务；用户每一轮请求，你都要判断它应该「续接到某条已有工作流上」还是「另起一条新工作流」。

你的职责：结合「对话已有的工作流清单」「本轮用户输入」和「本轮可用工具清单」（见下方），判断本轮任务应当以何种模式处理，并调用 submit_plan 提交规划。
可用工具能力是任务划分的重要依据：若需要多步操作或依赖特定工具，优先考虑 workflow 模式，并把每个子步骤对应到合适的工具；若需要动手执行但单步即可完成，用 single_agent；若根本不需要动手执行、你现在就能给出完整答案，用 direct_reply 直接回复。

调研优先原则（重要）：
- 遇到你不确定的专有名词、作品名、产品名、人名、术语、代码库现状等**客观信息**时，**必须先调用工具查证**（搜索、抓取网页、读文件、看目录），不要凭猜测规划，更不要因此中断去问用户。
- 用户的输入可能有错别字或简称（例如把片名写错、只说了个大概）。这种情况要先用工具搜索确认它指的是什么，而不是让用户重说一遍。
- 只有「用户的主观意图与偏好」才需要向用户确认（例如预算、风格取向、交付形式）。**客观事实一律自己查。**
- 禁止把「需求模糊，需要用户补充信息」作为 submit_plan 的理由 —— 在提交规划前，你应当已经通过调研把能查清的部分查清了。
- 调研完成后必须调用 submit_plan 收尾，不要无限调研下去。
- 每次调用调研工具时，都要在工具的 summary 必填参数里写一句话说明你这一步要查什么、为什么查。
- 联网调研优先用 deep_search（它内部完成「搜索→抓取→综合」，只返回收口结论）；
  只有已明确具体 URL 且确实需要该页原文时才用 fetch，并注意它有每任务次数配额。
- 严禁用 run_command 跑 curl / wget 等命令抓网页 —— 抓取一律走 deep_search / fetch。

已知信息沉淀（workflow 模式，重要）：
- 下游工作流节点看不到主对话历史和你的调研过程，它们只依赖你在 submit_plan 里交给它们的内容。
  因此，只要本轮调用过调研工具（deep_search / web_search / fetch / 读文件等），或你在主对话历史里
  看到了对后续节点重要的信息，提交 workflow 规划时**必须**在 known_context 里把这些信息完整写出。
- 不沉淀的后果是确定的：节点拿不到你已经掌握的东西，会从零重搜一遍同样内容，既浪费大量检索预算，
  还会因重复检索被死循环检测掐断，产出质量反而更差。
- known_context 是一段自由说明，把「下游节点需要知道的一切」都写进去，包括但不限于：
  · 调研查到的关键结论与事实（带具体名称、版本、数字、结论，可直接引用；不写推断、不写待办）；
  · 你读过/定位到的文件，给出完整路径（相对工作区路径，或用户给的绝对路径原样填），
    节点要读原文时按路径 read_file，不必重新 find 或猜文件名；
  · 你已经做出的关键判断与理由；
  · 从对话历史里提炼的、与本次任务相关的用户约束 / 偏好 / 已确认事实；
  · 节点需要留意的边界与风险。
- 宁全勿缺：宁可多写一点，不要遗漏会让节点重查或出错的关键信息；但不要粘贴搜索结果的原始条目
  （噪声大、过长），要归纳成可复用的结论。
- 反过来，节点描述里就不要再写「先去查 XX 是什么」这类已被调研解决的问题 —— 让节点直接基于 known_context 干活。
- known_context 是「工作流全局」共享的。此外，**每个节点都要填自己的 context**：写这个节点完成任务所需的、
  只属于它自己的上下文（它要处理的具体对象/文件、要产出的成果、特有的约束或要求）。
  全局 known_context 是所有节点共享的背景，节点 context 是只属于这个节点的部分，两者不重复。
  续接（append）场景尤其要在新节点的 context 里写清「本轮用户新增的诉求」—— 工作流全局的「用户原始输入」
  是创建那轮的原文，续接轮的新输入只有靠节点 context 传给节点，不写就丢了。

诊断/排查类任务的取证顺序（重要）：
- 当用户问「为什么失败 / 为什么没结果 / 哪里出了问题」时，先取真实信号：重放一次真实调用、跑一次最小复现、
  查运行时日志与原始响应体（状态码、报文体、错误码），拿到事实后再回到代码做静态分析。
- 只读代码就下结论是常见误判来源 —— 代码里「看起来会这样」不等于运行时真的这样。
- 节点描述只写「要查什么」（例如「重放该请求并抓取原始状态码与响应体」），
  禁止写预期结论或引导性措辞（例如「确认兜底逻辑吞掉了空结果」）；
  否则子节点会在你的假设里找证据，而不是找事实。

聚焦核心目标（重要）：
- 先问「完成用户本轮诉求最少需要几个节点」，默认就取这个最小集合。节点数是成本不是成绩：
  多一个节点就多一次执行、多一处失败面、多一段上下文。
- 每个节点都必须直接推进用户的核心目标。凡是「可能顺便有用」「顺便补充」「顺便对比」「顺便优化」
  的边缘内容一律不开节点 —— 用户没要求的事不要做。
- 不要为「完整性」铺开范围：不设写背景 / 写前言 / 写结语这类无信息增量的节点，
  也不把同一件事拆成「调研 + 复核 + 再复核」多道工序。
- 追加轮（append）尤其要克制：只补当前确实缺的那一步，禁止借追加把范围扩到用户没提过的方向。

模式说明（按「能直接答就直接答 → 要动手就单 Agent → 要拆解才工作流」的顺序判断）：
0. direct_reply（直接回复）：本轮不需要任何执行动作，你此刻就能给出完整、准确的最终答案。
   例：闲聊寒暄、常识/概念解释、基于对话历史或上下文已有内容的追问与解释、
   短文本的翻译/改写/润色（原文就在输入里）、你在本轮调研后已拿到足以作答的信息。
   → 调用 submit_plan(mode="direct_reply", reason="...", reply="给用户的完整回复")。
   → reply 就是用户最终看到的回答（支持 Markdown），必须完整作答，
     禁止写「我将……」「稍后为你……」这类过渡语，也不要提及规划、模式、工具等内部概念。
   → 以下情况不能用 direct_reply，应选 single_agent 或 workflow：
     · 需要读写文件、执行命令、生成/修改代码文件或任何产物文件；
     · 需要联网查证而本轮尚未查证（先调研，调研完再决定）；
     · 用户要看对话里已生成的文件/产物内容，而其全文不在当前上下文里（需要 read_file）；
     · 答案篇幅很长、需要多步推导或反复校对。
   → 拿不准时选 single_agent：它多花一轮调用，但答错的代价更大。

1. single_agent（单 Agent 模式）：需要动手执行，但一个 LLM + 工具循环即可完成。
   例：读一个文件后回答、跑一条命令、改一处代码、单次检索并总结、生成单个文件等。
   → 调用 submit_plan(mode="single_agent", reason="...")，new_nodes 留空或省略。
   → 本轮不会写入对话 workflow。

2. workflow（工作流模式）：任务复杂，需要拆解为多个有依赖关系的子步骤。
   例：研究报告（检索→归纳→撰写→审校）、代码重构（分析→拆分→修改→测试）等。
   → 调用 submit_plan(mode="workflow", reason="...", workflow_action=..., new_nodes=[...])
   → new_nodes 只列本轮新增的节点。若与已有节点有依赖关系，用已有 node_id 填 depends_on。
   → 注意：选择该模式后，本轮你不需要自己动手完成任务，只需给出拆解；
     这些步骤会进入对话的工作流由后续流程逐个执行。

工作流归属决策（workflow 模式下必须明确给出 workflow_action）：
- append（续接）：本轮任务是某条已有工作流的延续或补充 —— 例如基于它的产出继续加工、
  给它补一个步骤、补救它失败的部分。此时必须填 workflow_id（取自下方清单），
  并且只把该工作流清单里标为「可依赖」的节点填进 depends_on。
  续接的价值在于复用：已完成节点的产出数据会直接注入新节点，不必重跑。
- new（新建）：本轮任务与已有工作流无关，是一个新话题、新目标，或已有工作流都不可追加。
  此时必须填 workflow_title 作为这条新工作流的短标题。
- 判断依据是本轮任务与已有工作流在「目标」上是否连贯，而不是对话是否连续 ——
  同一个对话里用户完全可以先后提两件不相干的事，那就该是两条工作流，硬塞进一条是错误的。

不可追加的情况（清单里会标注，且不会出现在可依赖节点里）：
- 被用户主动停止的工作流：说明规划本身有问题，没有复用价值，只能新开。
- 失败工作流中失败节点及其下游分支：它们的依赖永远无法满足，不可作为依赖锚点；
  但失败节点上游那些「已完成」的节点仍然可依赖，续接时应尽量利用它们。

产物呈现原则（重要）：
- 当用户询问「对话中已经生成/产出的成果」（文章、报告、代码、方案、清单等）的内容、结果或成品本身时，应把该产物完整呈现给用户，而不是只回复「已完成」「进度 100%」「有 N 个节点」这类状态描述。
- 典型问法：「文章写得怎么样了」「给我看看报告」「代码呢」「结果是什么」——这些都是在要最终产出本身，优先把最终产出完整输出。
- 只有用户明确只问进度/状态（如「到哪一步了」「进度如何」「还剩几个节点」）时，才用状态摘要回答。
- 该场景下应选 single_agent 模式直接输出产物内容，而不是再拆成 workflow
  （产物全文已完整出现在上下文里时，也可用 direct_reply 直接给出）。
- 需要给用户文件下载链接时，用工作区相对路径（如 outputs/xxx/report.md）或
  /api/v1/files/download?chat_id=<chat_id>&path=<URL 编码的相对路径>；严禁编
  sandbox:/、file://、/mnt/data 这类假路径 —— 那些用户点不开。

并行度与粒度（重要）：
- 能并行就并行：凡是彼此没有数据依赖的子步骤，各自独立成节点并让 depends_on 留空，使它们并行执行，不要人为串行。
- depends_on 只在「下游确实需要上游的产出作为输入」时才填写；仅凭「先做 A 再做 B」的时序直觉不要填依赖，那会把本可并行的活白白串起来。
- 每个节点必须是有意义的独立工作单元：对应一个真实、完整、目标单一的子任务（能由一个工具或一次 LLM 调用完成），而不是凭空切出来的碎片。
- 不要为凑并行而堆砌无意义节点：把本可一步完成的事硬拆成多步、或塞入没有实际产出的「占位/装饰」节点，只会徒增调度开销与失败面，属于反面教材。
- 粒度适度、不要过度复杂：拆分是为了「并行 + 目标单一」，不是为了追求节点数。简单任务一个节点能讲清就不拆；复杂任务拆到「每个节点可独立执行、目标明确」即可，不要拆到碎片化。
- 首轮规划给出完整、合理的节点拆解即可，后续追加轮只做增量补充，不再重复已规划的节点。

增量约束（workflow 模式下）：
- new_nodes 数量 1~10 个（首轮至少 2 个，才构成 DAG；追加轮可以只加 1 个）。
- 每个新节点 node_id 全局唯一（跨全对话），不得与已有节点冲突。建议命名 n<N>_<slug>，例如 n1_search、n2_analyze。
- depends_on 里每个 id 必须是「本次 new_nodes 中的 id」或「已有 workflow 中的 id」，且不能自依赖。
- 每个节点的 input.input_text 描述该子节点要处理的具体内容，可显式引用前置节点的产出（比如「基于 n1_search 的结果做归纳」）。

输出节点约束（重要，必须严格遵守）：
- 本轮新规划的节点集合必须且只能有一个「输出节点」—— 它是本轮新增子任务的最终出口，负责把本轮所有分支的结果汇总成本轮要交付的产物（文章、报告、代码、方案、清单等）。
- 输出节点的判定标准：在本轮新增节点里，没有任何其他新增节点依赖它（没有任何新增节点的 depends_on 指向它），也就是本轮新增 DAG 的「叶子」。
- 除这唯一输出节点外，本轮其余新增节点都必须被某个下游新增节点依赖；本轮出现多个并行子步骤时，必须再加一个汇总节点把它们汇聚起来，而不是让多个叶子各自悬空。
- 反例：n1、n2、n3 三个子步骤彼此独立、都不依赖谁 → 产生了 3 个输出节点，不合法。正例：再加 n4_summary，让 depends_on=[n1,n2,n3]，此时只有 n4_summary 一个输出节点。

除调研性的工具调用外，你必须调用 submit_plan 收尾，不要用普通文本回复。`

// ============================================================
// Tool schema —— 强制规范化输出的核心
// ============================================================

// buildSubmitPlanTool 构造 submit_plan 协议工具的 JSON Schema。
//
// 它不是真实工具，而是规划期注入的「提交出口」：
// 模型调用它即表示规划完成，由执行器拦截处理，不会真的去执行任何东西。
func buildSubmitPlanTool() llm.Tool {
	// 单个 node 的 schema
	nodeSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"node_id": map[string]interface{}{
				"type":        "string",
				"description": "节点全局唯一 id（跨全对话不重复），建议 n<N>_<slug>",
			},
			"name": map[string]interface{}{
				"type":        "string",
				"description": "节点简短名称",
			},
			"description": map[string]interface{}{
				"type":        "string",
				"description": "该节点要做什么的详细说明。只写「要查什么 / 要做什么」，不要写预期结论或引导性判断，避免子节点在假设里找证据；内容须直接服务用户本轮核心目标",
			},
			"input": map[string]interface{}{
				"type":        "object",
				"description": "该节点的输入，input_text 描述具体待处理内容",
				"properties": map[string]interface{}{
					"input_text": map[string]interface{}{
						"type":        "string",
						"description": "该子节点的输入文本，可引用前置节点结果",
					},
				},
				"required": []string{"input_text"},
			},
			"context": map[string]interface{}{
				"type": "string",
				"description": "该节点完成任务所需的专属上下文（每个节点都要填）：它要处理的具体对象/文件、要产出的成果、特有的约束或要求。" +
					"这是只属于本节点的上下文，与工作流全局 known_context（所有节点共享的背景）不重复。" +
					"续接（append）节点尤其要写清本轮用户新增的诉求（全局「用户原始输入」是创建轮的原文，续接轮新输入只能靠这里传）。",
			},
			"depends_on": map[string]interface{}{
				"type":        "array",
				"description": "依赖的前置节点 id 列表。仅当本节点确实需要该上游节点的输出作为输入时才填写；彼此无数据依赖的节点一律留空数组以便并行执行。可引用已有 workflow 节点或本次 new_nodes 中的节点；无依赖填空数组。注意：并行分支最终必须汇聚到唯一一个汇总输出节点（没有任何节点依赖它），见系统提示的输出节点约束",
				"items": map[string]interface{}{
					"type": "string",
				},
			},
		},
		"required": []string{"node_id", "name", "description", "input", "depends_on"},
	}

	// 顶层参数 schema
	paramsSchema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"mode": map[string]interface{}{
				"type":        "string",
				"description": "处理模式：direct_reply（无需执行动作，直接给出答案）、single_agent（需要动手执行的简单任务）或 workflow（复杂任务需拆解）",
				"enum":        []string{model.PlanModeDirectReply, model.PlanModeSingleAgent, model.PlanModeWorkflow},
			},
			"reply": map[string]interface{}{
				"type": "string",
				"description": "direct_reply 模式必填：直接给用户的完整最终回复（支持 Markdown）。" +
					"必须完整作答，不写「我将……」之类过渡语，不提规划/模式/工具等内部概念。其它模式不要填。",
			},
			"reason": map[string]interface{}{
				"type":        "string",
				"description": "选择该模式的简要理由（一句话）",
			},
			"plan_summary": map[string]interface{}{
				"type":        "string",
				"description": "本轮规划的一句话摘要；workflow 模式下建议提供并汇总对话整体计划",
			},
			"new_nodes": map[string]interface{}{
				"type":        "array",
				"description": "本轮新增的 DAG 节点列表；single_agent 模式留空数组；workflow 模式 1~10 个。按实际数据依赖拆分：无依赖的子步骤并行，但每个节点须是有意义的独立子任务，不要为凑并行堆砌无意义节点或过度拆分；只取能完成用户核心目标的最小节点集合，不设顺便补充/对比/优化类边缘节点；并保证本轮新增节点集合有且只有一个输出节点（见系统提示的输出节点约束）",
				"items":       nodeSchema,
			},
			"workflow_action": map[string]interface{}{
				"type":        "string",
				"description": "workflow 模式下本轮规划落在哪里：append=在 workflow_id 指定的已有工作流上追加节点（复用其已完成节点的产出）；new=规划一个全新的工作流。当本轮任务与已有工作流无关，或已有工作流都不可追加时，必须选 new",
				"enum":        []string{"append", "new"},
			},
			"workflow_id": map[string]interface{}{
				"type":        "string",
				"description": "workflow_action=append 时必填：目标工作流 ID，必须是上下文「可追加工作流清单」中列出的 id，不可凭空编造",
			},
			"workflow_title": map[string]interface{}{
				"type":        "string",
				"description": "workflow_action=new 时必填：新工作流的短标题（8~16 字），用于在对话的工作流列表里区分它，例如「竞品调研」「季度财报分析」",
			},
			"known_context": map[string]interface{}{
				"type": "string",
				"description": "workflow 模式且本轮有调研/已知信息时必填：下游工作流节点需要继承的全部已知信息（一段自由说明）。" +
					"下游节点看不到主对话历史和你的调研过程，只依赖这段内容，请把以下都写进去：" +
					"① 调研查到的关键结论与事实（带具体数字/版本/名称，可直接引用，不写推断待办）；" +
					"② 读过的文件完整路径（节点要按路径 read_file）；" +
					"③ 已做的关键判断与理由；" +
					"④ 从对话历史提炼的、与本次任务相关的用户约束/偏好/已确认事实；" +
					"⑤ 需要留意的边界与风险。" +
					"宁全勿缺，但不要粘贴搜索原始条目（噪声大过长），要归纳成可复用结论。",
			},
		},
		"required": []string{"mode", "reason"},
	}

	return llm.Tool{
		Name:        model.SubmitPlanToolName,
		Description: "提交任务规划结果。完成调研后必须调用此函数收尾，不要用普通文本回复。",
		Parameters:  paramsSchema,
	}
}

// ============================================================
// LLM 返回结构（tool_call 参数解析后的内部表示）
// ============================================================

type planningLLMResp struct {
	Mode        string `json:"mode"`
	Reason      string `json:"reason"`
	PlanSummary string `json:"plan_summary,omitempty"`
	// Reply direct_reply 模式下给用户的最终回复。
	//
	// 落库 task_plan 时会清空（见 finishPlanning）：正文已写进 task_output.reply_text，
	// 再在 plan 里存一份只会让同一段长文本重复占库。turn 快照里的原始参数不受影响。
	Reply    string            `json:"reply,omitempty"`
	NewNodes []planningLLMNode `json:"new_nodes,omitempty"`
	// WorkflowAction workflow 模式下本次规划落在哪里：
	//   - "append" 在 workflow_id 指定的已有工作流上追加节点（复用已完成节点的数据）
	//   - "new"    规划一个全新的工作流
	//
	// 为空时由后端推断：有可追加的候选工作流就续接激活的那条，否则新建。
	WorkflowAction string `json:"workflow_action,omitempty"`
	// WorkflowID action=append 时必填：目标工作流任务 ID，必须来自提示词里
	// 给出的「可追加工作流清单」。指向清单外的 id 会被后端回退成新建。
	WorkflowID string `json:"workflow_id,omitempty"`
	// WorkflowTitle action=new 时的短标题（8~16 字）。一个对话可以有多条工作流，
	// 前端切换器按标题区分它们。缺失时后端回落到 plan_summary 截断。
	WorkflowTitle string `json:"workflow_title,omitempty"`
	// KnownContext 下游工作流节点需要继承的全部已知信息（自由文本）。
	//
	// 这是「已知信息沉淀」的统一出口：不再按信息类型拆字段（检索结论、文件
	// 路径、决策、历史约束……每出现一类信息就补一个字段是无穷枚举），而是让
	// 模型在 submit_plan 收口时，自主把「下游节点必须知道的一切」写成一段说明，
	// 后端原样注入每个节点。
	//
	// 为什么必须由模型自己写而不是后端去捞原始工具输出：调研工具返回的是
	// 逐条搜索结果的原始素材（噪声大、常达数万字符），而节点需要的是「哪些
	// 已经是确定的事实」。这层归纳只有做调研的那次调用能做对。
	//
	// 缺失时后端会自动从规划期记录里抽取一份兜底（source=auto），
	// 但质量明显不如模型总结，因此 prompt 里按必填项要求。
	KnownContext string `json:"known_context,omitempty"`
	// 以下三个字段是历史枚举字段，仅保留做旧模型 / 旧数据兼容读取，
	// 不再引导模型填写；新数据一律写 KnownContext。
	ResearchSummary string   `json:"research_summary,omitempty"`
	KeyFindings     []string `json:"key_findings,omitempty"`
	SourceFiles     []string `json:"source_files,omitempty"`
}

type planningLLMNode struct {
	NodeID      string              `json:"node_id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Input       model.TaskInputData `json:"input"`
	DependsOn   []string            `json:"depends_on"`
	// Context 节点级上下文（自由文本）：这个节点特有的、工作流全局 known_context
	// 没覆盖到的背景信息。尤其续接（append）节点要写清「本轮用户新增的诉求」——
	// 工作流全局的「用户原始输入」是创建那轮的原文，续接轮的新输入只有靠这里承载，
	// 不写会导致续接节点不知道用户这轮到底要它做什么。
	Context string `json:"context,omitempty"`
}

// UnmarshalJSON 容错解析 submit_plan 的顶层参数。
//
// 混元（hy4-preview）等部分模型在 function calling 时，会把嵌套的数组/对象参数
// 错误地序列化成 JSON 字符串：new_nodes 不是一个真正的 array，而是一个内容为
// 数组文本的 string。标准 json.Unmarshal 会因此报
// "cannot unmarshal string into ... []planningLLMNode"，使一份本来合理的规划
// 被误判为非法而降级 single_agent。这里对 new_nodes 做「字符串剥一层」的容错。
func (r *planningLLMResp) UnmarshalJSON(data []byte) error {
	type rawResp struct {
		Mode        string `json:"mode"`
		Reason      string `json:"reason"`
		PlanSummary string `json:"plan_summary"`
		// Reply 同 KnownContext：自定义解析必须显式声明，否则 direct_reply 的正文会被静默丢弃。
		Reply          string          `json:"reply"`
		NewNodes       json.RawMessage `json:"new_nodes"`
		WorkflowAction string          `json:"workflow_action"`
		WorkflowID     string          `json:"workflow_id"`
		WorkflowTitle  string          `json:"workflow_title"`
		// KnownContext 必须在这里显式声明：本结构是自定义 UnmarshalJSON，
		// 不走默认反射解析，漏一个字段模型填的内容就会被静默丢弃。
		// 曾因此把 known_context 丢掉，导致 DAG 无 plan_context、执行期节点
		// 拿不到规划期已知信息。
		KnownContext string `json:"known_context"`
	}
	var raw rawResp
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	r.Mode = raw.Mode
	r.Reason = raw.Reason
	r.PlanSummary = raw.PlanSummary
	r.Reply = raw.Reply
	r.WorkflowAction = raw.WorkflowAction
	r.WorkflowID = raw.WorkflowID
	r.WorkflowTitle = raw.WorkflowTitle
	r.KnownContext = raw.KnownContext

	if len(raw.NewNodes) == 0 || string(raw.NewNodes) == "null" {
		return nil
	}
	nodes, err := decodeNodeList(raw.NewNodes)
	if err != nil {
		return err
	}
	r.NewNodes = nodes
	return nil
}

// UnmarshalJSON 容错解析单个规划节点。
// 与顶层同理，depends_on（数组）与 input（对象）也可能被模型序列化成字符串。
func (n *planningLLMNode) UnmarshalJSON(data []byte) error {
	type rawNode struct {
		NodeID      string          `json:"node_id"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Input       json.RawMessage `json:"input"`
		DependsOn   json.RawMessage `json:"depends_on"`
		// Context 同 top-level 的 known_context：自定义解析必须显式声明，
		// 否则节点级 context 会被静默丢弃。
		Context string `json:"context"`
	}
	var raw rawNode
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n.NodeID = raw.NodeID
	n.Name = raw.Name
	n.Description = raw.Description
	n.Context = raw.Context

	if len(raw.Input) > 0 && string(raw.Input) != "null" {
		if err := json.Unmarshal(raw.Input, &n.Input); err != nil {
			var s string
			if err2 := json.Unmarshal(raw.Input, &s); err2 != nil {
				return fmt.Errorf("node %s: input must be an object or a JSON string: %v", n.NodeID, err)
			}
			if err := json.Unmarshal([]byte(s), &n.Input); err != nil {
				return fmt.Errorf("node %s: input string does not contain a valid object: %v", n.NodeID, err)
			}
		}
	}

	if len(raw.DependsOn) > 0 && string(raw.DependsOn) != "null" {
		if err := json.Unmarshal(raw.DependsOn, &n.DependsOn); err != nil {
			var s string
			if err2 := json.Unmarshal(raw.DependsOn, &s); err2 != nil {
				return fmt.Errorf("node %s: depends_on must be an array or a JSON string: %v", n.NodeID, err)
			}
			if err := json.Unmarshal([]byte(s), &n.DependsOn); err != nil {
				return fmt.Errorf("node %s: depends_on string does not contain a valid array: %v", n.NodeID, err)
			}
		}
	}
	return nil
}

// decodeNodeList 解析 new_nodes：可能是数组，也可能是被序列化成字符串的数组。
func decodeNodeList(raw json.RawMessage) ([]planningLLMNode, error) {
	var nodes []planningLLMNode
	if err := json.Unmarshal(raw, &nodes); err == nil {
		return nodes, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("new_nodes must be an array or a JSON string of an array: %v", err)
	}
	if err := json.Unmarshal([]byte(s), &nodes); err != nil {
		return nil, fmt.Errorf("new_nodes string does not contain a valid array: %v", err)
	}
	return nodes, nil
}

// ============================================================
// 执行逻辑
// ============================================================

// planningPhaseImpl 规划期的实际逻辑（同步执行）。
//
// 全程用一条 turn_type=functional_call + phase=planning 的 task_turn
// 记录这次 LLM 原子调用：
// 入口 BeginTurn（turn_count+1），成功 FinishTurn，任何失败 FailTurn。
//
// 三种收口：
//   - 模型调 submit_plan  → 落库 plan，再按 mode 分流：
//     · direct_reply → reply 落库为最终回复 + hint.PlanDelegated=true（当轮收口）
//     · single_agent → 切执行期 + hint.PlanSubmitted=true（继续 agent 循环）
//     · workflow     → 节点并入对话工作流 + 写流程概述 + hint.PlanDelegated=true（当轮收口）
//   - 模型只调真实工具    → hint.NeedToolCall=true，走 tool_call 后再回规划期
//   - 模型只给文本 / 调研轮次耗尽 → 降级：写默认 single_agent plan 并切执行期
func planningPhaseImpl(ctx context.Context, temp *model.TaskTemp) (data interface{}, err error) {
	if funcCallRegistry.conversationDAO == nil {
		return nil, fmt.Errorf("func-call-actuator: conversationDAO not initialized (planning phase)")
	}

	log.Printf("[func-call-actuator][%s] start phase=planning (chat=%s turn_count=%d)",
		temp.TaskID, temp.ChatID, temp.TurnCount)

	// 1. 加载对话已有的工作流集合（可能有多条，也可能一条都没有）。
	//
	// v1.4 起一个对话可以有多条工作流：本轮规划要么续接其中一条，要么新开一条。
	// 因此这里拿的是全集 + 各自的可追加性判定，而不是「唯一绑定的那条」。
	wfs, err := loadWorkflowCandidates(ctx,
		funcCallRegistry.conversationDAO, funcCallRegistry.taskDAO, temp.ChatID)
	if err != nil {
		return nil, fmt.Errorf("func-call-actuator: load conversation workflows failed: %w", err)
	}
	// 用户显式要求「新开一个工作流」时，模型判断让位于用户判断
	forceNewWorkflow := temp.TaskInput.Data != nil && temp.TaskInput.Data.ForceNewWorkflow
	appendable := 0
	for _, c := range wfs.Candidates {
		if c.Policy.Appendable {
			appendable++
		}
	}
	log.Printf("[func-call-actuator][%s] chat workflows: %d (appendable=%d, active=%q, force_new=%v)",
		temp.TaskID, len(wfs.Candidates), appendable, wfs.ActiveID, forceNewWorkflow)

	// 2. 加载可用工具。
	// 规划期同样把真实工具暴露给模型（可先调研再规划），
	// 加载失败降级为「无工具」，不阻断规划。
	userText := renderUserInput(temp.TaskInput.Data, resolveWorkspaceDirForTask(temp))
	if strings.TrimSpace(userText) == "" {
		return nil, fmt.Errorf("func-call-actuator: task %s has empty input_text", temp.TaskID)
	}

	toolSet, tErr := funcCallRegistry.toolProvider.Load(ctx, temp.ChatID)
	if tErr != nil {
		log.Printf("[func-call-actuator][%s] load tools failed, planning without tools: %v",
			temp.TaskID, tErr)
		toolSet = nil
	}
	realTools := toolSet.LLMTools()

	// 3. system prompt 与本轮输入。
	//
	// 规划期同样要注入 prompt 型 skill 指令（与执行期一致）：规划期允许调工具
	// 调研，若用户任务涉及 PDF/Excel 等，模型需要知道有对应的 skill 能力
	// （如 pdfkit CLI），否则只能退回 run_command 硬解 —— 实测 PDF 任务在规划期
	// 因拿不到 pdfkit 指令，用 pdftotext（系统未装）/ python（Python2）反复失败，
	// 4 轮调研预算耗尽仍未收口。prompt 型 skill 不变成工具，只能靠这里注入。
	systemPrompt := planningSystemPrompt + "\n\n" + workspacepkg.ShellHint() + "\n\n" + imagePolicySection(ctx, temp.ChatID, toolSet) + "\n\n" + renderAvailableTools(toolSet)
	if skillConfigs, sErr := funcCallRegistry.toolProvider.LoadSkills(ctx, temp.ChatID); sErr != nil {
		log.Printf("[func-call-actuator][%s] load skills for planning failed (ignore): %v", temp.TaskID, sErr)
	} else if section := skillpkg.BuildPromptSection(skillConfigs); section != "" {
		systemPrompt = systemPrompt + "\n\n" + section
	}

	userContent := renderWorkflowContext(wfs, forceNewWorkflow) + "\n\n【本轮用户输入】\n" + userText

	// 4. 工具列表 = 真实工具 + submit_plan 协议工具。
	// tool_choice=any：规划期没有「纯文本」的合理出口，必须落到某个工具上。
	tools := make([]llm.Tool, 0, len(realTools)+1)
	tools = append(tools, realTools...)
	tools = append(tools, buildSubmitPlanTool())
	choice := llm.ToolChoiceAny
	opts := llm.DefaultOptions()

	// 5. 调研轮次是否已耗尽。耗尽后不再给真实工具，只留 submit_plan，
	// 逼模型立即收口（而非直接判失败，规划结果仍由模型给出更准）。
	round, _, _, _ := resolveLoopState(ctx, temp.TaskID, model.PhasePlanning)
	researchExhausted := round >= planningMaxToolCallRound
	if researchExhausted {
		log.Printf("[func-call-actuator][%s] planning research budget exhausted (%d/%d), submit_plan only",
			temp.TaskID, round, planningMaxToolCallRound)
		tools = []llm.Tool{buildSubmitPlanTool()}
	}

	// 5.5 解析本对话使用的 LLM 客户端（按 conversation.model_id，空则默认/回退）。
	// 要在组装之前解析：压缩调用复用同一个模型生成摘要。
	llmClient, err := resolveLLMClient(ctx, temp)
	if err != nil {
		return nil, err
	}

	// 5.6 组装输入（消息体本身在循环里组装 —— 重规划轮要带反馈）。
	//
	// 走 assembleTurnMessages：规划期与执行期共用的同一个入口，压缩、
	// 分块归属、底线判死三条规则完全一致。此前规划期自己拼历史
	// （buildAgentHistorySegments 传 nil covered + 直接 flatten），历史
	// 段只增不减，而规划期是每轮必经 —— 小窗口模型下对话一长，第一轮
	// 规划就超限判死（实测 task 9a87d093：sys 2595 + tools 2013 + 历史
	// 4799 = 9407 > ceiling 6400，报 input over ceiling）。
	//
	// 后来一度改成「调压缩 + 自己按增量补算反馈」，规则仍是两套：
	// 反馈的计量与判死在规划期，其余在压缩路径，改一处就会不一致。
	// 现在统一由入口负责，规划期只管把反馈塞进 Tail。
	//
	// 顺序硬约束：必须在 tools 定稿之后（调研预算耗尽后只剩 submit_plan，
	// 提前算会把 tools 开销算大），也必须在 client 解析之后。
	planInput := turnAssembleInput{
		Temp:         temp,
		SystemPrompt: systemPrompt,
		UserText:     userContent,
		Tools:        tools,
		Client:       llmClient,
	}

	// 6~8. 调 LLM 并分流。用循环承载「输出节点校验不过 → 带反馈重规划」：
	// 收口后若检出 DAG 有多个输出节点（或没有输出节点），不直接降级，
	// 而是把失败原因回喂模型、让它重新 submit_plan，最多重试
	// planningMaxReplanRounds 次；超过后与其他协议违规一样降级 single_agent。
	//
	// 反馈按轮累积在 replanTail 里整体交给组装入口，而不是「取回 messages
	// 后自己 append」—— 追加必须伴随重新折叠（连续 assistant 会让 Gemini
	// 判 400），交给调用方做就又变回两处各维护一套了。
	var replanTail []llm.Message
	replanRounds := 0
	for {
		// 组装（含历史压缩 + 尾部反馈计量 + 底线判死），与执行期同一套规则。
		planInput.Tail = replanTail
		asm, err := assembleTurnMessages(ctx, planInput)
		if err != nil {
			return nil, err
		}
		messages, ctxBlocks := asm.Messages, asm.Blocks

		// 输入快照（带分块计量），随 turn 落库供调试页 / 用量弹窗读取
		inputSnap := buildLLMTurnInput(llmClient.Name(), messages, tools, choice, opts)
		inputSnap.ContextBlocks = ctxBlocks

		// 开启 turn 记录（task.turn_count += 1，拿到 turn_seq）
		turn, err := funcCallRegistry.turnDAO.BeginTurn(ctx, dao.BeginTurnParams{
			TaskID:       temp.TaskID,
			ChatID:       temp.ChatID,
			ParentTaskID: temp.ParentTaskID,
			// 调用类型与语义阶段是两个正交维度：这是一次 LLM 调用
			// （turn_type=functional_call），发生在规划期（phase=planning）。
			// 早期把 planning 当成一种 turn_type，把两个维度压成了一个，
			// 结果丢掉了「它也是一次 functional_call」的事实。
			TurnType: model.TurnTypeFunctionalCall,
			Phase:    model.PhasePlanning,
			Stage:    model.TaskStage(temp.Stage),
			Round:    int32(round),
			Model:    llmClient.Name(),
			Input:    inputSnap,
		})
		if err != nil {
			return nil, fmt.Errorf("func-call-actuator: begin planning turn failed: %w", err)
		}
		log.Printf("[func-call-actuator][%s] planning turn #%d started (id=%d)",
			temp.TaskID, turn.TurnSeq, turn.ID)

		// 任何后续失败都回写 turn 失败状态；
		// 若 LLM 已返回结果，一并落库到 output，便于调试页排查原始 tool_calls/text。
		var llmResult *llm.ChatResult
		failTurn := func(cause error) error {
			var outSnap interface{}
			if llmResult != nil {
				outSnap = buildLLMTurnOutput(llmResult)
			}
			if ferr := funcCallRegistry.turnDAO.FailTurn(ctx, turn.ID, cause, outSnap); ferr != nil {
				log.Printf("[func-call-actuator][%s] fail turn %d error: %v", temp.TaskID, turn.ID, ferr)
			}
			// 失败 turn 也算结束 —— 前端收到 turn_done 才会停 spinner 并刷新
			publishTurnDone(temp.TaskID, turn.ID, turn.TurnSeq)
			return cause
		}

		if reqBytes, mErr := json.Marshal(map[string]interface{}{
			"messages": messages,
			"tools":    toolNames(tools),
			"choice":   choice,
		}); mErr == nil {
			log.Printf("[func-call-actuator][%s] planning llm req=%s",
				temp.TaskID, truncate(string(reqBytes), 4000))
		}

		// 调 LLM，强制走 tool_call
		result, err := chatWithToolsStreaming(ctx, llmClient,
			temp.TaskID, turn.ID, turn.TurnSeq, messages, tools, choice, opts)
		if err != nil {
			return nil, failTurn(fmt.Errorf("func-call-actuator: planning llm chat failed: %w", err))
		}
		llmResult = result

		if rspBytes, mErr := json.Marshal(result); mErr == nil {
			log.Printf("[func-call-actuator][%s] planning llm rsp=%s",
				temp.TaskID, truncate(string(rspBytes), 2000))
		}

		// 分流：submit_plan（收口）/ 真实工具（调研）/ 都没有（降级）
		planCall, realCalls := splitPlanningToolCalls(result.ToolCalls)

		switch {
		case planCall != nil:
			// ── 收口：提交规划 ──
			if len(realCalls) > 0 {
				// 模型同时给了调研与提交。以提交为准 —— 既然它已能给出规划，
				// 说明信息足够；那些工具调用交给执行期按需重发即可。
				//
				// 但必须把它们从 turn 快照里剔掉：这些调用不会被执行，
				// 也就没有对应的 tool 结果消息。留在快照里，历史重建时会得到一条
				// 「assistant 请求了工具却没有任何响应」的消息，provider 直接判 400。
				log.Printf("[func-call-actuator][%s] submit_plan came with %d unexecuted tool call(s), "+
					"trimmed from turn snapshot: %v",
					temp.TaskID, len(realCalls), callNames(realCalls))
				result = withOnlyPlanCall(result, *planCall)
			}

			// 协议违规（参数解不开 / 规划不合法）不判任务失败。
			// 规划是增强项，用户要的是答案 —— 降级成 single_agent 继续干活。
			resp, target, pErr := decodeAndValidatePlan(planCall, wfs, forceNewWorkflow)
			if pErr != nil {
				var replanErr *replanError
				if errors.As(pErr, &replanErr) && replanRounds < planningMaxReplanRounds {
					// 输出节点/依赖锚点校验不过：收尾本轮 turn，把失败原因回喂模型重新规划。
					finishPlanningTurn(ctx, temp, turn.ID, turn.TurnSeq, result)
					replanRounds++
					log.Printf("[func-call-actuator][%s] plan rejected (%v), "+
						"replan round %d/%d",
						temp.TaskID, pErr, replanRounds, planningMaxReplanRounds)
					replanTail = append(replanTail, replanFeedbackMessages(planCall, replanErr)...)
					continue
				}
				log.Printf("[func-call-actuator][%s] invalid plan (%v), fallback to single_agent", temp.TaskID, pErr)
				resp = fallbackPlan("模型提交的规划不合法（" + pErr.Error() + "），按单 Agent 模式直接执行")
			}
			// 先收尾规划 turn，再落库 plan 与任务输出。顺序不能反：
			// finishPlanning → delegateToWorkflow → persistAgentOutput 会读 task_turn
			// 生成 think_process，此时若 turn 仍处于 running，buildThinkSteps 会把它跳过，
			// 导致「任务规划：判定为 workflow…」这条规划期收尾项在思考过程里丢失。
			finishPlanningTurn(ctx, temp, turn.ID, turn.TurnSeq, result)

			delegated, err := finishPlanning(ctx, temp, resp, target)
			if err != nil {
				// plan 落库 / 工作流委托失败：把刚收尾为 success 的 turn 回改为 failed，
				// 保持「任务失败 ↔ 对应 turn 失败」的一致口径（FailTurn 会覆盖 success 状态）。
				return nil, failTurn(err)
			}
			if delegated {
				setPlanDelegatedHint(temp.TaskID)
			} else {
				setPlanSubmittedHint(temp.TaskID)
			}
			log.Printf("[func-call-actuator][%s] planning turn #%d done → %s (mode=%s new_nodes=%d): %s",
				temp.TaskID, turn.TurnSeq, planningOutcome(resp.Mode, delegated),
				resp.Mode, len(resp.NewNodes), resp.Reason)
			return nil, nil

		case len(realCalls) > 0:
			// ── 调研：走 tool_call，回来后仍在规划期 ──
			if _, invalid := splitValidToolCalls(realCalls, toolSet); len(invalid) > 0 {
				log.Printf("[func-call-actuator][%s] planning requested unknown tools (will feed back): %v",
					temp.TaskID, invalid)
			}
			finishPlanningTurn(ctx, temp, turn.ID, turn.TurnSeq, result)
			setResearchHint(temp.TaskID, round)
			log.Printf("[func-call-actuator][%s] planning turn #%d done → tool_call (research round %d/%d, %d calls: %v)",
				temp.TaskID, turn.TurnSeq, round, planningMaxToolCallRound,
				len(realCalls), callNames(realCalls))
			return nil, nil

		default:
			// ── 降级：模型没调任何工具（不遵守协议 / provider 不支持强制）──
			//
			// 不判任务失败：规划是增强项，用户要的是答案。
			// 写一条兜底的 single_agent plan，切执行期继续干活。
			log.Printf("[func-call-actuator][%s] planning turn #%d returned no tool call (text=%q), "+
				"fallback to single_agent and switch to executing",
				temp.TaskID, turn.TurnSeq, truncate(result.Text, 200))

			if _, err := finishPlanning(ctx, temp,
				fallbackPlan("模型未提交结构化规划，按单 Agent 模式直接执行"),
				nil); err != nil {
				return nil, failTurn(err)
			}
			finishPlanningTurn(ctx, temp, turn.ID, turn.TurnSeq, result)
			setPlanSubmittedHint(temp.TaskID)
			return nil, nil
		}
	}
}

// planningOutcome 规划收口方向的日志描述
func planningOutcome(mode string, finished bool) string {
	switch {
	case finished && mode == model.PlanModeDirectReply:
		return "direct reply (task finished)"
	case finished:
		return "workflow (delegated, task finished)"
	}
	return "executing phase"
}

// splitPlanningToolCalls 把模型返回的工具调用拆成「submit_plan」与「真实工具」两部分。
// submit_plan 只取第一个（重复提交属异常，以首个为准）。
func splitPlanningToolCalls(calls []llm.ToolCall) (planCall *llm.ToolCall, realCalls []llm.ToolCall) {
	for i := range calls {
		if calls[i].Name == model.SubmitPlanToolName {
			if planCall == nil {
				planCall = &calls[i]
			}
			continue
		}
		realCalls = append(realCalls, calls[i])
	}
	return planCall, realCalls
}

// withOnlyPlanCall 复制一份只保留 submit_plan 调用的结果，用于 turn 快照。
//
// 不原地改 result.ToolCalls：调用方后续还要按原始返回打日志与判断，
// 就地截断会让日志与实际模型输出不一致，排查时容易误判。
func withOnlyPlanCall(result *llm.ChatResult, planCall llm.ToolCall) *llm.ChatResult {
	trimmed := *result
	trimmed.ToolCalls = []llm.ToolCall{planCall}
	return &trimmed
}

// fallbackPlan 构造兜底的 single_agent 规划。
// 模型不给规划、或给了不合法的规划时用它顶上，保证任务能进执行期。
func fallbackPlan(reason string) *planningLLMResp {
	return &planningLLMResp{
		Mode:   model.PlanModeSingleAgent,
		Reason: reason,
	}
}

// replanFeedbackMessages 生成「模型刚提交的 submit_plan + 校验失败反馈」这一对消息，
// 让模型看到自己上一版规划为何被拒，从而在下一轮给出修正后的规划。
//
// 语义与 tool_call 的回喂一致：assistant 发起了 submit_plan 工具调用，
// 就必须补一条对应的 tool 结果消息，否则 Gemini/OpenAI 会因
// 「functionCall 没有对应 functionResponse」判 400。
//
// 只负责生成消息、不负责拼接：拼接（连同重新折叠）由共用组装入口
// assembleTurnMessages 按 Tail 统一处理，规划期不自己 append。
func replanFeedbackMessages(planCall *llm.ToolCall, err *replanError) []llm.Message {
	assistant := llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{*planCall},
	}
	feedback := llm.Message{
		Role:     llm.RoleTool,
		ToolName: model.SubmitPlanToolName,
		Content:  "规划被拒绝，请按以下要求调整后重新调用 submit_plan 提交：\n" + err.msg,
	}
	return []llm.Message{assistant, feedback}
}

// decodeAndValidatePlan 解析并校验 submit_plan 的参数，同时解析出本轮规划的目标工作流。
//
// 返回值里的 target 只在 mode=workflow 时非空，调用方把它一路带到落库层，
// 保证「判定依据」与「落库动作」用的是同一份决策 —— 分开算会出现
// 「按 A 工作流校验通过、却写进了 B 工作流」这类不一致。
//
// 校验分三层，只有后两层会走「带反馈重规划」：
//  1. 参数解不开 —— 纯协议违规，直接降级 single_agent
//  2. 依赖锚点非法（续接时依赖了不可追加/已废弃的节点）—— replanError
//  3. 输出节点不唯一 —— replanError
func decodeAndValidatePlan(
	planCall *llm.ToolCall, wfs *workflowContext, forceNew bool,
) (*planningLLMResp, *planWorkflowTarget, error) {
	resp, err := decodeToolCallArgs(planCall.Arguments)
	if err != nil {
		return nil, nil, fmt.Errorf("decode submit_plan args: %w", err)
	}
	target := resolveWorkflowTarget(resp, wfs, forceNew)
	// new（以及 single_agent）时 target 为 nil 或 DAG 为空，
	// 后续校验自然退化成「首轮规划」的口径。
	var existingWF *model.WorkflowOutputData
	if target != nil {
		existingWF = target.DAG
	}

	inferMissingDeps(resp, existingWF, target)
	if err := validatePlanningResp(resp, existingWF, target); err != nil {
		return nil, nil, err
	}
	return resp, target, nil
}

// inferMissingDeps 为缺省 depends_on 的节点，从它的 description 与 input_text
// 中推断对其他节点的引用并补全依赖。
//
// 混元等模型常把「n4 汇总 n1/n2/n3」这类依赖只写在描述文本里（如
// "汇总 n1_profile、n2_market、n3_project 三份结果"），而不填 depends_on 字段
// （无视 tool schema 的 required 约束）。若不补全，这些节点全都会被当作叶子，
// 触发「多输出节点」重规划，甚至 3 轮补不齐后被降级 single_agent。
// 这里做一次保守推断：仅当某节点 depends_on 为空时，扫描其文本中出现的其他
// node_id（按标识符 token 精确匹配，避免 n1 误命中 n10），把它们填进 depends_on。
// 显式声明过的依赖一律不动。
//
// 续接已有工作流时，候选 id 只取可依赖锚点 —— 把已归档的失败节点也算进来，
// 会让一次好心的推断反而替模型填上一条永远等不到的依赖（紧接着被锚点硬校验
// 拒掉，白白消耗一轮重规划）。
func inferMissingDeps(resp *planningLLMResp, existing *model.WorkflowOutputData, target *planWorkflowTarget) {
	ids := make(map[string]bool)
	if existing != nil {
		for _, n := range existing.Nodes {
			if target.IsAppend() && !target.Policy.Appendable {
				continue
			}
			if target.IsAppend() && !target.Policy.AnchorIDs[n.NodeID] {
				continue
			}
			ids[n.NodeID] = true
		}
	}
	for _, n := range resp.NewNodes {
		ids[n.NodeID] = true
	}

	// 反向引用过滤：先收集「谁显式依赖了谁」——dependsOnMe[被依赖节点] = 依赖它的节点集合。
	// 推断节点 n 的依赖时，若候选 id 已显式依赖 n，说明 n 是 id 的上游
	// （n 的文本里写「供 id 使用 / 传给 id」正是这个意思），n 不该反向依赖 id，
	// 否则会制造出 n ⇄ id 的环。
	dependsOnMe := map[string]map[string]bool{}
	for _, n := range resp.NewNodes {
		for _, dep := range n.DependsOn {
			if dependsOnMe[dep] == nil {
				dependsOnMe[dep] = map[string]bool{}
			}
			dependsOnMe[dep][n.NodeID] = true
		}
	}

	for i := range resp.NewNodes {
		n := &resp.NewNodes[i]
		if len(n.DependsOn) > 0 {
			continue
		}
		haystack := n.Description + "\n" + n.Input.InputText
		var inferred []string
		for id := range ids {
			if id == n.NodeID || !strings.Contains(haystack, id) {
				continue
			}
			// 反向引用过滤：候选 id 显式依赖本节点时，本节点是它的上游，跳过
			if dependsOnMe[n.NodeID] != nil && dependsOnMe[n.NodeID][id] {
				continue
			}
			if isStandaloneIDToken(haystack, id) {
				inferred = append(inferred, id)
			}
		}
		if len(inferred) > 0 {
			n.DependsOn = inferred
		}
	}
}

// detectPlanningCycle 检测合并后 DAG（已有节点 + 本轮新节点）的依赖环。
//
// 环的来源有两类：① inferMissingDeps 的反向引用推断（现已加过滤，但仍可能
// 有漏网）；② 模型直接填错 depends_on（如把下游节点填进起点的依赖）。
// 两者都会让执行器的 depsSatisfied 永远等不到依赖、整个工作流死锁在 running。
// 落库前用 DFS 三色标记拦住，返回描述环的 error（供 replanError 包装，触发带反馈重规划）。
//
// 依赖边的方向是 dep → node（node 依赖 dep），DFS 沿 dep 方向走，遇到灰色节点即回到栈中节点，构成环。
func detectPlanningCycle(existing *model.WorkflowOutputData, newNodes []planningLLMNode) error {
	adj := map[string][]string{} // nodeID → 它依赖的节点列表
	ids := map[string]bool{}
	if existing != nil {
		for _, n := range existing.Nodes {
			adj[n.NodeID] = n.DependsOn
			ids[n.NodeID] = true
		}
	}
	for _, n := range newNodes {
		adj[n.NodeID] = n.DependsOn
		ids[n.NodeID] = true
	}

	order := make([]string, 0, len(ids))
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var dfs func(id string) bool
	dfs = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range adj[id] {
			if !ids[dep] {
				continue // 未知节点由依赖校验另行处理，这里跳过
			}
			switch color[dep] {
			case gray:
				for i, s := range stack {
					if s == dep {
						cycle = append(cycle, stack[i:]...)
						cycle = append(cycle, dep)
						return true
					}
				}
			case white:
				if dfs(dep) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}

	for _, id := range order {
		if color[id] == white {
			if dfs(id) {
				return fmt.Errorf("检测到依赖环: %s", strings.Join(cycle, " → "))
			}
		}
	}
	return nil
}

// isStandaloneIDToken 判断 id 是否作为独立标识符 token 出现在文本中，
// 而非某个更长标识符的子串（例如 id="n1" 不应命中 "n10" 或 "n1_profile"）。
func isStandaloneIDToken(haystack, id string) bool {
	idx := strings.Index(haystack, id)
	for idx != -1 {
		var before, after byte
		if idx > 0 {
			before = haystack[idx-1]
		}
		end := idx + len(id)
		if end < len(haystack) {
			after = haystack[end]
		}
		// node_id 只含 ASCII 标识符字符；中文等多字节字符的每个字节都 >= 0x80，
		// 天然不属于 ID 字节，因此直接用字节判断边界即可，无需做 rune 对齐。
		if !isIDByte(before) && !isIDByte(after) {
			return true
		}
		// 继续找下一处出现
		rest := haystack[idx+len(id):]
		next := strings.Index(rest, id)
		if next == -1 {
			break
		}
		idx += len(id) + next
	}
	return false
}

func isIDByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// planningContextLimits 规划期上下文注入节点时的长度约束。
//
// 上限存在的理由：上下文是**每个节点**都要注入的，一份 58KB 的调研结论
// 原样塞进去，四个节点就是 232KB —— 比不注入还糟。这里压到「够用」的量级。
const (
	// planningContextMaxRunes 模型声明 known_context 的上限。
	// 比旧 planningSummaryMaxRunes(3000) 略宽：known_context 是「全部已知信息」
	// 的统一出口（结论 + 文件路径 + 决策 + 历史约束），内容天然比纯摘要多。
	planningContextMaxRunes = 4000
	// planningAutoSummaryMaxRunes 自动抽取（source=auto）时整段摘要的上限。
	// 自动抽取的内容是子任务回复的拼接，噪声高于模型总结，因此上限更紧。
	planningAutoSummaryMaxRunes = 4000
	// planningAutoItemMaxRunes 自动抽取时单个子任务结论的上限。
	planningAutoItemMaxRunes = 1200
	// planningKeyFactMaxRunes 单条关键事实的上限（过长说明模型没写成条目）。
	planningKeyFactMaxRunes = 200
	// planningKeyFactMaxCount 关键事实最多保留条数。
	planningKeyFactMaxCount = 12
)

// buildPlanResearchContext 组装规划期已知信息，随 DAG 一起写入工作流。
//
// 两条来源，优先级明确：
//  1. model —— 模型在 submit_plan 里给的 known_context（统一自由文本出口）。
//     质量最好：做调研的那次调用才知道哪些是已确认事实、哪些是噪声。
//     旧模型没填 known_context 时，从旧三字段（research_summary / key_findings /
//     source_files）拼一份兼容。
//  2. auto  —— 模型没给时，后端从规划期 deep_search 子任务的产出里抽取。
//     兜底而非替代：抽出来的是子任务回复的拼接，噪声明显更大。
//
// 两种来源都会带上规划期落盘的产物文件 —— 那是完整素材的位置，
// 节点需要细节时可以 read_file 读全文，不必把全文塞进上下文。
//
// 返回 nil 表示没有可注入的内容（规划期没调研），节点照常从零开始。
func buildPlanResearchContext(
	ctx context.Context, temp *model.TaskTemp, resp *planningLLMResp,
) *model.WorkflowPlanContext {
	if temp == nil {
		return nil
	}
	files := collectPlanningArtifactFiles(ctx, temp)

	if resp != nil {
		text := strings.TrimSpace(resp.KnownContext)
		if text == "" {
			// 兼容：旧模型 / 旧数据没填 known_context，从旧三字段拼一份。
			text = legacyContextText(resp)
		}
		if text != "" || len(files) > 0 {
			return &model.WorkflowPlanContext{
				Context: truncateRunes(text, planningContextMaxRunes),
				Files:   files,
				Source:  model.PlanContextSourceModel,
			}
		}
	}

	// 兜底：模型没给（老模型 / 不遵守协议），从规划期子任务抽取。
	summary := extractPlanningResearchSummary(ctx, temp)
	if strings.TrimSpace(summary) == "" && len(files) == 0 {
		return nil
	}
	return &model.WorkflowPlanContext{
		Context: truncateRunes(summary, planningAutoSummaryMaxRunes),
		Files:   files,
		Source:  model.PlanContextSourceAuto,
	}
}

// legacyContextText 把旧三字段（research_summary / key_findings / source_files）
// 拼成一段自由文本，用于兼容还没升级到 known_context 的旧模型 / 旧数据。
// 新数据不会再走这里。
func legacyContextText(resp *planningLLMResp) string {
	var b strings.Builder
	if s := strings.TrimSpace(resp.ResearchSummary); s != "" {
		b.WriteString(s)
	}
	if kf := normalizeKeyFindings(resp.KeyFindings); len(kf) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("关键事实：\n")
		for _, f := range kf {
			b.WriteString("- " + f + "\n")
		}
	}
	var paths []string
	for _, p := range resp.SourceFiles {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("读过/定位到的文件：\n")
		for _, p := range paths {
			b.WriteString("- " + p + "\n")
		}
	}
	return b.String()
}

// collectPlanningArtifactFiles 收集规划期落盘的产物文件。
//
// 只看本规划任务直接创建的子任务（parent_task_id = 本任务）的 output_files，
// 不做工作区全量扫描 —— 全量扫描会把同一对话里其它工作流的文件也算进来。
// 查询失败返回 nil：产物清单是增强信息，不该阻断规划收口。
func collectPlanningArtifactFiles(ctx context.Context, temp *model.TaskTemp) []model.FileInfo {
	if funcCallRegistry.taskDAO == nil {
		return nil
	}
	subs, err := funcCallRegistry.taskDAO.ListSubTasksByParent(ctx, temp.TaskID)
	if err != nil {
		log.Printf("[func-call-actuator][%s] list planning subtasks for context failed (ignore): %v",
			temp.TaskID, err)
		return nil
	}
	var out []model.FileInfo
	seen := make(map[string]bool)
	for _, s := range subs {
		agentOut := parseAgentOutput(s.TaskOutput)
		for _, f := range agentOut.OutputFiles {
			key := f.Path
			if key == "" {
				key = f.Name
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	fillDownloadURLs(temp.ChatID, out)
	return out
}

// extractPlanningResearchSummary 从规划期子任务的产出里抽取调研结论。
//
// 子任务回复可能很长（deep_search 一次能写上万字），这里按条截断再拼接，
// 并整体受 planningAutoSummaryMaxRunes 约束 —— 注入的是「要点」不是「全文」，
// 全文通过 Files 里的路径按需读。
func extractPlanningResearchSummary(ctx context.Context, temp *model.TaskTemp) string {
	if funcCallRegistry.taskDAO == nil {
		return ""
	}
	subs, err := funcCallRegistry.taskDAO.ListSubTasksByParent(ctx, temp.TaskID)
	if err != nil {
		log.Printf("[func-call-actuator][%s] list planning subtasks for summary failed (ignore): %v",
			temp.TaskID, err)
		return ""
	}
	var b strings.Builder
	for _, s := range subs {
		agentOut := parseAgentOutput(s.TaskOutput)
		body := strings.TrimSpace(agentOut.ReplyText)
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("◆ ")
		b.WriteString(truncateRunes(body, planningAutoItemMaxRunes))
		if b.Len() > planningAutoSummaryMaxRunes*2 {
			// 已经远超上限，继续拼没有意义（下面会整体截断）
			break
		}
	}
	return b.String()
}

// normalizeKeyFindings 清洗模型给的关键事实清单：
// 去空、去过长、限量。模型偶尔会把整段话塞进一条，超过
// planningKeyFactMaxRunes 的按条目语义截断，避免单条吃掉整个配额。
func normalizeKeyFindings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, f := range in {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, truncateRunes(f, planningKeyFactMaxRunes))
		if len(out) >= planningKeyFactMaxCount {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// finishPlanning 规划收口的持久化动作，返回当前任务是否当轮收口
// （workflow 已移交 / direct_reply 已作答，二者都由 PlanDelegated 驱动 finished）。
//
// direct_reply：写 plan → reply 作为最终回复落库 → 切执行期（同 workflow，仅为状态一致）。
// single_agent：写 plan → 切执行期，当前任务继续 agent 循环。
// workflow    ：写 plan → 节点并入对话绑定的工作流（无则新建并绑定）
//
//	→ 当前任务写「执行流程概述」作为回复 → 切执行期。
//	  切 phase 只是为了让状态一致（不留在规划期），任务紧接着就被
//	  状态机按 PlanDelegated 判为 finished，不会真的再进执行期。
//
// 顺序讲究：phase 必须最后切。plan / workflow 落库失败时任务仍留在规划期，
// 下一轮可重试；反过来先切 phase 再写，失败后规划信息就永久丢了。
func finishPlanning(
	ctx context.Context, temp *model.TaskTemp,
	resp *planningLLMResp, target *planWorkflowTarget,
) (delegated bool, err error) {
	// 落库的 plan 不带 reply 正文（见 planningLLMResp.Reply 注释），用副本清空，
	// 不动 resp 本身 —— 下面 direct_reply 分支还要用它。
	planRecord := *resp
	planRecord.Reply = ""
	planJSON, err := json.Marshal(planRecord)
	if err != nil {
		return false, fmt.Errorf("func-call-actuator: marshal plan: %w", err)
	}
	if err := funcCallRegistry.taskDAO.UpdateTaskPlan(ctx, temp.TaskID, string(planJSON)); err != nil {
		return false, fmt.Errorf("func-call-actuator: persist plan failed: %w", err)
	}

	if resp.Mode == model.PlanModeDirectReply {
		// 直接回复：规划器给出的 reply 就是最终答案，落库后当轮收口。
		// 走与执行期同一个 persistAgentOutput —— 长回复转存、产物清单、
		// think_process 汇总全部复用，前端看到的与正常答复完全同构。
		if err := persistAgentOutput(ctx,
			funcCallRegistry.taskDAO, funcCallRegistry.turnDAO,
			temp, strings.TrimSpace(resp.Reply), ""); err != nil {
			return false, fmt.Errorf("func-call-actuator: persist direct reply failed: %w", err)
		}
		// 任务就此收尾，清掉 before 段历史缓存（不会再进 functional_call）
		globalAgentHistoryCache.delete(temp.TaskID)
		delegated = true
	}

	if resp.Mode == model.PlanModeWorkflow {
		// 调研上下文必须在这里抽：它读的是本任务的 turn 与子任务记录，
		// 属于「规划期」这一语义阶段的产出，移交给工作流后就不再属于这里。
		planCtx := buildPlanResearchContext(ctx, temp, resp)
		if planCtx != nil {
			log.Printf("[func-call-actuator][%s] plan context built: source=%s summary=%d runes, facts=%d, files=%d",
				temp.TaskID, planCtx.Source, len([]rune(planCtx.Summary)),
				len(planCtx.KeyFacts), len(planCtx.Files))
		}
		if err := delegateToWorkflow(ctx, temp, resp, target, planCtx); err != nil {
			return false, err
		}
		delegated = true
	}

	if err := funcCallRegistry.taskDAO.UpdateTaskPhase(ctx, temp.TaskID, int(model.PhaseExecuting)); err != nil {
		return delegated, fmt.Errorf("func-call-actuator: switch phase to executing failed: %w", err)
	}
	// 同步内存快照，避免同一批处理里再被当成规划期
	temp.Phase = int(model.PhaseExecuting)
	return delegated, nil
}

// delegateToWorkflow 把本轮规划移交给目标工作流任务（续接已有或新建一条），
// 并给当前对话任务写一段执行流程概述作为回复。
func delegateToWorkflow(
	ctx context.Context, temp *model.TaskTemp,
	resp *planningLLMResp, target *planWorkflowTarget,
	planCtx *model.WorkflowPlanContext,
) error {
	result, err := applyWorkflowPlan(ctx,
		funcCallRegistry.conversationDAO, funcCallRegistry.taskDAO,
		temp, resp, target, planCtx)
	if err != nil {
		return fmt.Errorf("func-call-actuator: apply workflow plan failed: %w", err)
	}

	// 对话任务的输出契约始终是 AgentOutputData（它是一条对话消息）。
	// DAG 不写在这里 —— 那是工作流任务 task_output 的职责，
	// 两处都写必然出现节点状态更新不同步。
	// WorkflowTaskID 随输出透出：前端据此在回复上渲染「查看工作流」跳转按钮。
	if err := persistAgentOutput(ctx,
		funcCallRegistry.taskDAO, funcCallRegistry.turnDAO,
		temp, renderWorkflowReply(resp, result), result.TaskID); err != nil {
		return fmt.Errorf("func-call-actuator: persist workflow reply failed: %w", err)
	}

	// 任务就此收尾，清掉 before 段历史缓存（不会再进 functional_call）
	globalAgentHistoryCache.delete(temp.TaskID)
	return nil
}

// finishPlanningTurn 收尾规划 turn 并广播 SSE。
// 落库失败只记日志 —— turn 已跑完，订阅方仍需要收到结束通知。
func finishPlanningTurn(
	ctx context.Context, temp *model.TaskTemp,
	turnID int64, turnSeq int32, result *llm.ChatResult,
) {
	if err := funcCallRegistry.turnDAO.FinishTurn(ctx, turnID, dao.FinishTurnParams{
		Output:           buildLLMTurnOutput(result),
		TotalTokens:      result.Usage.TotalTokens,
		PromptTokens:     result.Usage.PromptTokens,
		CompletionTokens: result.Usage.CompletionTokens,
	}); err != nil {
		log.Printf("[func-call-actuator][%s] finish planning turn %d error: %v", temp.TaskID, turnID, err)
	}
	publishTurnDone(temp.TaskID, turnID, turnSeq)
}

// setPlanSubmittedHint 写「已提交规划」的流转决策：
// 状态机据此回到 functional_call_wait（此时 phase 已是执行期）。
//
// 同时把轮次与熔断计数归零 —— 规划期调研消耗的 tool_call 轮次
// 不该占用执行期干活的预算，规划期的工具失败也不该让执行期一进来就接近熔断。
// 两个 phase 各自持有一份完整预算，是有意的取舍：
// 规划期上限 planningMaxToolCallRound 已单独约束，不存在无限循环风险。
func setPlanSubmittedHint(taskID string) {
	store := container.GetTransitionStore()
	store.SetHint(taskID, &model.StageTransitionHint{
		PlanSubmitted:    true,
		NeedToolCall:     false,
		ToolCallRound:    0,
		MaxToolCallRound: model.DefaultMaxToolCallRound,
	})
}

// setPlanDelegatedHint 写「已移交工作流」的流转决策：
// 状态机据此把当前对话任务直接判为 finished。
//
// 与 setPlanSubmittedHint 互斥 —— 后者会让任务回到 functional_call 继续跑
// 单 Agent 循环，那正是判定 workflow 后要避免的重复劳动。
func setPlanDelegatedHint(taskID string) {
	store := container.GetTransitionStore()
	store.SetHint(taskID, &model.StageTransitionHint{
		PlanDelegated:    true,
		PlanSubmitted:    false,
		NeedToolCall:     false,
		ToolCallRound:    0,
		MaxToolCallRound: model.DefaultMaxToolCallRound,
	})
}

// setResearchHint 写「规划期要去调研」的流转决策：走 tool_call 后回到规划期。
func setResearchHint(taskID string, round int) {
	store := container.GetTransitionStore()
	hint := store.GetHint(taskID)
	if hint == nil {
		hint = &model.StageTransitionHint{
			MaxToolCallRound: model.DefaultMaxToolCallRound,
			ToolCallRound:    round,
		}
	}
	if hint.MaxToolCallRound <= 0 {
		hint.MaxToolCallRound = model.DefaultMaxToolCallRound
	}
	hint.PlanSubmitted = false
	hint.NeedToolCall = true
	store.SetHint(taskID, hint)
}

// ============================================================
// workflow 上下文渲染
// ============================================================
//
// 具体渲染实现见 workflow_binding.renderWorkflowContext —— 多工作流之后
// 上下文要按「候选清单 + 各自可依赖锚点」的形态呈现，与绑定逻辑放在一起
// 才能保证提示词口径与后端硬校验口径一致。

// renderAvailableTools 把可用工具渲染成简要信息列表（name + 截断描述），
// 供规划期模型判断是否用工作流、以及如何按工具能力拆分子任务。
//
// 只放「名字 + 一句描述」，不放完整入参 schema：
// 既控制 token，也让模型聚焦在「有哪些能力」而非「怎么填参数」上。
// （真实调用所需的完整 schema 已随 tools 参数下发。）
func renderAvailableTools(toolSet *ToolSet) string {
	var b strings.Builder
	b.WriteString("【本轮可用工具】\n")
	b.WriteString("以下是本轮任务可调用的工具及能力简述，供你判断是否需要拆解为多步骤工作流，以及每个子步骤应使用哪些工具。必要时你也可以先调用它们做调研。\n")
	if toolSet == nil || toolSet.Len() == 0 {
		b.WriteString("（无可用工具，本轮只能依赖模型自身知识处理）")
		return b.String()
	}
	for _, t := range toolSet.LLMTools() {
		fmt.Fprintf(&b, "- %s: %s\n", t.Name, truncateRunes(t.Description, 120))
	}
	return b.String()
}

// ============================================================
// 规划结果解析与校验
// ============================================================

// decodeToolCallArgs 把 tool_call.Arguments (map[string]interface{}) 转成强类型结构
func decodeToolCallArgs(args map[string]interface{}) (*planningLLMResp, error) {
	if args == nil {
		return nil, fmt.Errorf("empty tool call arguments")
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal args: %w", err)
	}
	var resp planningLLMResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal args: %w (raw=%s)", err, truncate(string(raw), 400))
	}
	return &resp, nil
}

// replanError 可修复的规划校验失败。
//
// 与普通协议违规（参数解不开、id 冲突等）不同，这类错误通常可以靠
// 「把失败原因回喂模型、让它重新规划」修复。因此单独成类型，供 planningPhaseImpl
// 区分「带反馈重规划」与「降级 single_agent」两条处理路径。
//
// 目前两类：
//   - 输出节点不唯一（多叶子 / 无叶子，见 newOutputNodeError）
//   - 依赖锚点非法（续接时 depends_on 指到了不可依赖的节点，见 newAnchorError）
//
// 两者共同点：模型能看懂错在哪，且改一遍就有较大概率改对 —— 这正是
// 「值得回喂一次」的判断标准。反过来，参数解不开、节点 id 冲突这类
// 属于模型没遵守 schema，回喂收益低，直接降级更省心。
type replanError struct {
	msg string
}

func (e *replanError) Error() string { return e.msg }

// computeSinkNodeIDs 计算「本轮新增节点」中的输出节点 id。
//
// 输出节点 = 没有任何其他节点依赖它的节点（depends_on 里没有出现它的 id），
// 即叶子。约束已从「整个工作流唯一输出节点」收窄为「每轮新规划出的节点
// 集合唯一输出节点」：
//
//   - 首轮（existing 为空）：新增节点即整个 DAG，仍要求有且只有一个叶子，
//     即整个 DAG 只有一个汇总输出 —— 与旧语义一致。
//   - 续接（existing 非空）：只统计本轮新增节点内部的叶子，不把已有节点
//     （包括上一轮的原输出节点）算进候选。这样「新节点跳过原输出节点、
//     直接挂到某个已完成上游」这类合理的增量追加不会被误判成多输出节点。
//
// 已有节点的 depends_on 仍照常记账：虽然它们只指向已有节点、不影响新增
// 节点的叶子判定，但记账保证了 depended 始终是完整的全图依赖集合，防御
// 未来「已有节点反向依赖新增节点」之类的扩展不会悄悄漏判。
func computeSinkNodeIDs(existing *model.WorkflowOutputData, newNodes []planningLLMNode) []string {
	depended := make(map[string]bool)
	newIDs := make([]string, 0, len(newNodes))

	if existing != nil {
		for _, n := range existing.Nodes {
			for _, d := range n.DependsOn {
				depended[d] = true
			}
		}
	}
	for _, n := range newNodes {
		newIDs = append(newIDs, n.NodeID)
		for _, d := range n.DependsOn {
			depended[d] = true
		}
	}

	var sinks []string
	for _, id := range newIDs {
		if !depended[id] {
			sinks = append(sinks, id)
		}
	}
	return sinks
}

// newOutputNodeError 构造输出节点校验失败的错误，附带可读的原因说明。
func newOutputNodeError(sinks []string) *replanError {
	if len(sinks) == 0 {
		return &replanError{msg: "本轮新规划没有任何输出节点（可能出现了依赖环），必须且只能有一个汇总输出节点作为最终出口"}
	}
	return &replanError{msg: fmt.Sprintf(
		"本轮新规划的节点必须且只能有 1 个输出节点，但当前产生了 %d 个：%s。请调整：让本轮新增节点中的并行分支最终汇聚到唯一一个汇总输出节点，其余新增节点都应作为它的上游被它依赖",
		len(sinks), strings.Join(sinks, "、"))}
}

// newAnchorError 构造「依赖了不可依赖的节点」的校验失败错误。
//
// 续接场景下模型能依赖的只有锚点集合（target.Policy.AnchorIDs）：
// 全集减掉「失败节点及其后代」「已停止工作流的全部节点」「尚未跑完的节点」。
// 模型若把废弃节点填进 depends_on，新节点会永远等不到依赖，
// 整条工作流再也无法收敛 —— 必须在落库前拒掉。
//
// 错误信息里把可用锚点列出来，模型照着改即可，不必重新推理哪些能用。
func newAnchorError(bad []string, anchors map[string]bool) *replanError {
	usable := make([]string, 0, len(anchors))
	for id := range anchors {
		usable = append(usable, id)
	}
	sort.Strings(usable)
	return &replanError{msg: fmt.Sprintf(
		"以下节点不能作为 depends_on 的目标：%s。它们属于已废弃/未完成的分支，依赖它们会导致新节点永远等不到上游产出。"+
			"请改用本次 new_nodes 里的节点，或下列可依赖的已有节点：%s",
		strings.Join(bad, "、"), strings.Join(usable, "、"))}
}

// validatePlanningResp 校验 LLM 返回的规划是否合法（含增量语义与续接锚点约束）
//
// target 为 nil 或 Action=new 时 existing 也是 nil，等价于「首轮规划」校验。
func validatePlanningResp(
	r *planningLLMResp, existing *model.WorkflowOutputData, target *planWorkflowTarget,
) error {
	switch r.Mode {
	case model.PlanModeSingleAgent:
		return nil
	case model.PlanModeDirectReply:
		// 空回复不走重规划，直接按普通协议违规降级 single_agent：
		// 模型既然选了直接回复却没写出答案，让执行期老老实实再答一遍最稳，
		// 不值得为此多花一轮规划调用。
		if strings.TrimSpace(r.Reply) == "" {
			return fmt.Errorf("mode=direct_reply but reply is empty")
		}
		return nil
	case model.PlanModeWorkflow:
		if len(r.NewNodes) == 0 {
			return fmt.Errorf("mode=workflow but new_nodes is empty")
		}
		// 已有节点 id 集合
		existingIDs := existing.NodeIDs()
		// 首轮至少 2 个才构成 DAG
		if len(existingIDs) == 0 && len(r.NewNodes) < 2 {
			return fmt.Errorf("first-round workflow must have at least 2 new_nodes, got %d", len(r.NewNodes))
		}
		// 新增节点 id 集合 + 与已有不冲突
		newIDs := make(map[string]bool, len(r.NewNodes))
		for _, n := range r.NewNodes {
			if n.NodeID == "" || n.Name == "" {
				return fmt.Errorf("node_id and name must be non-empty")
			}
			if existingIDs[n.NodeID] {
				return fmt.Errorf("new node_id %s conflicts with existing workflow", n.NodeID)
			}
			if newIDs[n.NodeID] {
				return fmt.Errorf("duplicate new node_id: %s", n.NodeID)
			}
			newIDs[n.NodeID] = true
		}
		// 依赖校验：depends_on ∈ existingIDs ∪ newIDs，且不自依赖
		for _, n := range r.NewNodes {
			for _, dep := range n.DependsOn {
				if dep == n.NodeID {
					return fmt.Errorf("node %s depends on itself", n.NodeID)
				}
				if !existingIDs[dep] && !newIDs[dep] {
					return fmt.Errorf("node %s depends on unknown node %s", n.NodeID, dep)
				}
			}
		}
		// 依赖环校验（硬约束）：合并后 DAG 不能有环，否则执行器死锁。
		// 来源：inferMissingDeps 的方向推断（虽已过滤反向引用，仍留兜底）+ 模型直接填错。
		if err := detectPlanningCycle(existing, r.NewNodes); err != nil {
			return &replanError{msg: err.Error()}
		}
		// 续接锚点校验（硬约束）：依赖的已有节点必须落在可依赖集合里。
		//
		// 这是需求 4/5 的落地点：不能基于用户主动停止的工作流追加（target 在
		// resolveWorkflowTarget 阶段已被回退为 new，走不到这里），失败工作流的
		// 失败节点及其后代也已被排除在锚点外。这里再做一次硬校验，是因为
		// 提示词只能「劝」，模型仍可能照着旧上下文里的 id 填 —— 落库前必须拦住，
		// 否则新节点会挂在永远完成不了的上游上。
		if target.IsAppend() {
			var bad []string
			for _, n := range r.NewNodes {
				for _, dep := range n.DependsOn {
					if newIDs[dep] {
						continue // 本轮新增节点之间互依，不受锚点约束
					}
					if !target.Policy.AnchorIDs[dep] {
						bad = append(bad, dep)
					}
				}
			}
			if len(bad) > 0 {
				return newAnchorError(bad, target.Policy.AnchorIDs)
			}
		}
		// 输出节点校验（硬约束）：本轮新增节点集合必须且只能有一个输出节点。
		// 多个输出节点意味着本轮新增的并行分支没有汇聚，最终产物会散落成多份；
		// 零个输出节点通常意味着出现了依赖环。这类错误返回 replanError，
		// 供调用方走「带反馈重规划」而非直接降级。
		if sinks := computeSinkNodeIDs(existing, r.NewNodes); len(sinks) != 1 {
			return newOutputNodeError(sinks)
		}
		return nil
	default:
		return fmt.Errorf("unknown mode: %q", r.Mode)
	}
}

// ============================================================
// 执行期的规划上下文注入
// ============================================================

// renderPlanContext 把已提交的规划渲染成执行期的上下文段落。
//
// 规划期的 submit_plan turn 不进 agent 对话历史（见 agent_history.go），
// 否则执行期会看到一个自己没有的工具调用而报错。
// 规划信息通过这里注入 system prompt，让执行期知道「上一步定了什么计划」。
//
// 只处理 single_agent：workflow 模式的任务在规划收口时就已结束，
// 根本不会进执行期。真要走到这里说明状态有异常，此时给出计划步骤
// 反而会让模型把已移交给工作流的活再做一遍。
func renderPlanContext(planJSON string) string {
	if strings.TrimSpace(planJSON) == "" {
		return ""
	}
	var resp planningLLMResp
	if err := json.Unmarshal([]byte(planJSON), &resp); err != nil {
		return ""
	}
	if resp.Mode != model.PlanModeSingleAgent {
		return ""
	}

	var b strings.Builder
	b.WriteString("【本轮已确定的执行计划】\n")
	b.WriteString("处理模式：单 Agent 直接执行\n")
	if resp.Reason != "" {
		b.WriteString("判定理由：" + truncateRunes(resp.Reason, 300) + "\n")
	}
	if resp.PlanSummary != "" {
		b.WriteString("计划摘要：" + truncateRunes(resp.PlanSummary, 500) + "\n")
	}
	b.WriteString("请按上述计划推进，逐步完成后给出最终答案。")
	return b.String()
}
