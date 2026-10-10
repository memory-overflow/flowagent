// Package actuator — 任务产物（output_files）与长文本落盘。
//
// 职责：
//  1. 长回复自动转存 —— reply_text 超阈值时写成工作区文件，
//     task_output 里只留摘要 + 文件引用，避免撑爆 DB 与后续 LLM 上下文
//  2. 产物清单收集 —— 扫描工作区内本次任务写过的文件，填 OutputFiles
//
// 为什么必须有第 1 条的确定性兜底（而不是只靠 prompt 引导模型主动落盘）：
// 模型不听话是常态。写长文章时它经常把上万字直接塞进最终回复，
// 结果既撑大 task_output，又在下一轮 buildFullHistory 里被整段回放，
// 迅速吃满上下文。兜底层把这件事变成确定性的。
package actuator

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// maxInlineReplyRunes 最终回复保留在 task_output 内的字符上限。
//
// 超出即自动转存为工作区文件。取 4000 的依据：
//   - 正常问答、状态汇报、方案说明都在 2000 字以内，不会被误转存
//   - 长文章 / 完整报告 / 大段代码普遍上万字，必然命中
//   - 与 renderWorkflowSummary 的叶子产出上限（30000）留出量级差，
//     保证转存后的摘要在工作流摘要里也不会被二次截断
const maxInlineReplyRunes = 4000

// replySummaryRunes 转存后在 task_output 里保留的摘要长度（取正文开头）。
//
// 取 2000 而不是更小的 800：转存后留在库里的这段是**用户唯一能直接看到的
// 正文**，太短就退化成「开头两段的残片」，用户只能靠打开文件才能判断结果
// 是不是自己要的。实测 800 字经常切在 markdown 表格中间（一个 Agent 选型
// 调研里出现过「| 工具编排 | **原生** |」被拦腰截断），结构破损比内容少更糟。
const replySummaryRunes = 2000

// collectTaskArtifacts 扫描本次任务在工作区内产出的文件。
//
// 两条判定路径，优先用第一条：
//  1. artifactDir 非空 —— 任务有独立产出目录，目录内文件归属确定，
//     直接全量收（ScanArtifactsIn）。这是子任务的常规路径。
//  2. 否则按 mtime —— since 是判定基准（应传任务的开始时间 StartTime ——
//     任务首次进入执行时记录、之后保持不变的恒定值）。wsDir 为空（未配置
//     工作区）或 since 为零值时返回 nil：拿不到判定基准时宁可不报产物，
//     也不要把工作区历史文件全量误报为本次产出。
//
// 为什么必须有路径 1：工作区是对话级的，并行节点的执行窗口互相重叠，
// 纯 mtime 判定会把同一时刻其它节点（及其 deep_search 子任务）写的文件
// 一并算进来 —— 实测一次选型调研里三个并行节点各自报出同样的 24 个文件。
func collectTaskArtifacts(wsDir string, since *time.Time, artifactDir string) []model.FileInfo {
	if strings.TrimSpace(wsDir) == "" {
		return nil
	}

	var arts []workspacepkg.Artifact
	if strings.TrimSpace(artifactDir) != "" {
		arts = workspacepkg.ScanArtifactsIn(wsDir, artifactDir)
		// 补收 outputs/ 下的产物：模型（尤其 skill 脚本经 run_command）常把
		// 交付物写到 outputs/<自定义目录>/ 下（如 outputs/golden-coat-penguin/），
		// 而非本任务的 artifactDir（outputs/wf-*/nX）。只补 outputs/ 直属一层
		// 会漏掉这些子目录 —— 实测一个建模工作流把 20+ 个产物（图/FBX/GLB/OBJ）
		// 全写进 outputs/golden-coat-penguin/，结果产物清单为空。
		//
		// 排除 outputs/wf-*/ 与 outputs/ds-*/：那是其它任务（工作流节点 /
		// deep_search 子任务）的专属产出目录，收进来就退回「并行节点产物
		// 串味」的旧问题。其余 outputs/ 子目录都是模型自选的交付目录，应收。
		for _, a := range workspacepkg.ScanArtifactsIn(wsDir, workspacepkg.OutputsDirName) {
			if isFrameworkArtifactDir(a.RelPath) {
				continue
			}
			arts = append(arts, a)
		}
		// 补收工作区根目录直属文件：模型用 run_command 生成文件时，相对路径
		// 落到工作区根（run_command 的 cwd 是工作区根，不像 write_file 会把
		// 文件写进 artifactDir），例如 python 脚本里 Path('report.docx') 直接
		// 写根。只扫本任务目录和 outputs/ 会漏掉它们，而漏产物比误收更糟
		// （用户下载不到自己要的东西）。按 mtime 过滤只收本次任务开始之后写的，
		// 避免把历史轮次写到根目录的旧产物串进来。
		if since != nil && !since.IsZero() {
			for _, a := range workspacepkg.ScanArtifacts(wsDir, since.Add(-2*time.Second)) {
				if strings.Count(a.RelPath, "/") == 0 {
					arts = append(arts, a)
				}
			}
		}
	} else {
		if since == nil || since.IsZero() {
			return nil
		}
		// 往前放宽 2s：任务 start_time 由调度器写库，文件 mtime 由内核记录，
		// 两者存在毫秒级偏差；卡死边界会漏掉任务刚启动瞬间写下的文件。
		arts = workspacepkg.ScanArtifacts(wsDir, since.Add(-2*time.Second))
	}
	if len(arts) == 0 {
		return nil
	}

	files := make([]model.FileInfo, 0, len(arts))
	for _, a := range arts {
		files = append(files, model.FileInfo{
			Source:   "local",
			Name:     a.Name,
			Path:     a.RelPath,
			Size:     a.Size,
			MimeType: a.MimeType,
			FileType: a.FileType,
		})
	}
	return files
}

// isFrameworkArtifactDir 判断路径是否落在框架自有的任务专属产出目录下：
// outputs/wf-* 是工作流节点目录、outputs/ds-* 是 deep_search 子任务目录。
// 这些是其它任务的私有产出，collectTaskArtifacts 补收 outputs/ 时要排除，
// 避免并行节点互相串味；模型自选的 outputs/<自定义目录>/ 不受影响、照收。
func isFrameworkArtifactDir(relPath string) bool {
	parts := strings.SplitN(relPath, "/", 2)
	if len(parts) < 2 || parts[0] != workspacepkg.OutputsDirName {
		return false
	}
	return strings.HasPrefix(parts[1], "wf-") || strings.HasPrefix(parts[1], "ds-")
}

// dumpLongReply 把过长的最终回复转存成工作区文件。
//
// artifactDir 是本任务的产出目录（相对工作区，可为空）：子任务各自持有独立
// 目录，转存文件随之落在本任务目录内，产物归属不会串到并行节点上。
// 为空时落到 outputs/ 下（顶层对话任务的路径）。
//
// 返回：
//   - newReply 替换后的回复（摘要 + 文件引用）；未触发转存时为原文
//   - dumped   转存产生的文件信息；未触发转存时为 nil
//
// 任何一步失败都返回原文 —— 答案本身比「省上下文」重要，
// 不能因为落盘失败就把用户的结果弄丢。
func dumpLongReply(wsDir, artifactDir, taskID, reply string) (newReply string, dumped *model.FileInfo) {
	runes := []rune(reply)
	if len(runes) <= maxInlineReplyRunes {
		return reply, nil
	}
	if strings.TrimSpace(wsDir) == "" {
		// 没有工作区可落盘：保留原文，只记日志。
		// 这种情况说明配置缺失，不该静默截断用户的产物。
		log.Printf("[artifacts][%s] reply too long (%d runes) but no workspace, keep inline",
			taskID, len(runes))
		return reply, nil
	}

	dir := strings.TrimSpace(artifactDir)
	if dir == "" {
		dir = workspacepkg.OutputsDirName
	}
	outDir := filepath.Join(wsDir, filepath.FromSlash(dir))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Printf("[artifacts][%s] mkdir %s failed: %v, keep inline", taskID, dir, err)
		return reply, nil
	}

	// 文件名取正文标题（有含义），取不到才回落到 task_id 短前缀 ——
	// 「reply-12c2e91d-20260923-153802.md」这种时间戳名字对用户毫无信息量，
	// 产物列表里一排这样的文件根本分不清谁是谁。
	name := dumpFileName(reply, taskID)
	relPath := filepath.ToSlash(filepath.Join(dir, name))
	absPath := filepath.Join(outDir, name)

	if err := os.WriteFile(absPath, []byte(reply), 0o644); err != nil {
		log.Printf("[artifacts][%s] write long reply failed: %v, keep inline", taskID, err)
		return reply, nil
	}

	size := int64(len(reply))
	info := &model.FileInfo{
		Source:   "local",
		Name:     name,
		Path:     relPath,
		Size:     size,
		MimeType: "text/markdown",
		FileType: workspacepkg.FileTypeText,
	}

	log.Printf("[artifacts][%s] long reply dumped: %s (%d runes → %s)",
		taskID, relPath, len(runes), humanSize(size))

	return renderDumpedReply(reply, relPath, len(runes)), info
}

// dumpFileName 生成转存文件的文件名：优先取正文标题，取不到用 task_id 短前缀。
//
// 标题比时间戳有信息量得多：产物列表里「agent-平台选型调研报告.md」一眼能认，
// 而「reply-12c2e91d-20260923-153802.md」完全分辨不出内容。
//
// 只取正文里**第一个** markdown 标题（# 开头，不含 ## 以下的子标题 ——
// 子标题往往是「一、背景」这类章节名，作文件名没有区分度）。
func dumpFileName(reply, taskID string) string {
	if slug := slugifyHeading(reply); slug != "" {
		return slug + ".md"
	}
	shortID := taskID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	return fmt.Sprintf("reply-%s.md", shortID)
}

// slugifyHeading 从正文里提取第一个一级标题并清洗成可安全用作文件名的 slug。
//
// 清洗规则：去掉 markdown 标记字符、把文件系统不允许的字符换成 -、压缩空白、
// 截到 maxFileNameRunes 个字符。中文保留 —— 中文标题对用户更有辨识度，
// 现代文件系统（含 macOS/Windows/Linux）都支持 UTF-8 文件名。
func slugifyHeading(reply string) string {
	for _, ln := range strings.Split(reply, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		if slug := sanitizePathSegment(strings.TrimPrefix(trimmed, "# "), 40); slug != "" {
			return slug
		}
	}
	return ""
}

// renderDumpedReply 生成转存后的回复文本：开头摘要 + 完整产物的文件指引。
//
// 摘要取正文开头而非让模型再总结一遍 —— 多一次 LLM 调用既慢又可能失真，
// 开头段落通常已包含标题与导语，足以让用户判断这是不是想要的东西。
func renderDumpedReply(reply, relPath string, totalRunes int) string {
	head := truncateMarkdown(reply, replySummaryRunes)
	cutRunes := len([]rune(head))

	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\n---\n\n")
	fmt.Fprintf(&b, "**完整内容已保存为文件**：`%s`（共 %d 字，以上为前 %d 字）\n\n",
		relPath, totalRunes, cutRunes)
	b.WriteString("以上为按段落/表格边界截取的完整开头部分，完整内容请查看该文件（可在产物列表中下载）。")
	return b.String()
}

// truncateMarkdown 按结构边界把 markdown 文本截到 maxRunes 以内。
//
// 为什么不直接切前 N 个字符：硬切会落在段落、表格行、代码块的中间，
// 产出语法破损的片段（实测出现过「| 工具编排 | **原生** |」被拦腰截断），
// 用户看到的是半张表 —— 比内容少更糟，因为它看起来像渲染出错。
//
// 规则：逐行累计长度，超预算就停；切断点回退到「当前不可分割块」之前。
// 不可分割块有两类：
//   - 代码块（``` 围栏内的全部行）
//   - markdown 表格（连续以 | 开头的行）
//
// 极端情况（第一个块本身就超预算，例如开头就是一张巨表）退化为按行硬切：
// 至少保证不停在一行的中间。
func truncateMarkdown(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	lines := strings.Split(s, "\n")
	var (
		used       int  // 已累计的 rune 数（含换行）
		safeEnd    int  // 可安全切断的行下标（exclusive）
		blockStart int  // 当前不可分割块的起始行
		inFence    bool // 是否处于 ``` 围栏内
		inTable    bool // 是否处于表格行序列内
	)

	for i := 0; i < len(lines); i++ {
		need := len([]rune(lines[i])) + 1 // +1 为换行符
		if used+need > maxRunes {
			break
		}
		used += need

		trimmed := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(trimmed, "```"):
			// 围栏开关：进入时块从本行开始，离开时块到本行结束
			if inFence {
				inFence = false
				blockStart = i + 1
			} else {
				inFence = true
				blockStart = i
			}
			inTable = false
		case strings.HasPrefix(trimmed, "|"):
			if !inTable {
				inTable = true
				blockStart = i
			}
		default:
			inTable = false
			if !inFence {
				blockStart = i + 1
			}
		}

		if inFence || inTable {
			// 处在块内部：只能切到块开始之前，否则块被切一半
			safeEnd = blockStart
		} else {
			safeEnd = i + 1
		}
	}

	if safeEnd <= 0 {
		// 开头就是一个超预算的整块（例如正文是一整段没有换行的长文本），
		// 无法回退到块前：先尝试按行边界切，连第一行都超预算时才按字符硬切。
		// 硬切是最后手段 —— 它保证长度可控，代价是可能停在一行中间。
		acc, cut := 0, 0
		for i := range lines {
			acc += len([]rune(lines[i])) + 1
			if acc > maxRunes {
				break
			}
			cut = i + 1
		}
		if cut > 0 {
			return strings.TrimRight(strings.Join(lines[:cut], "\n"), "\n")
		}
		return string(runes[:maxRunes])
	}

	out := strings.Join(lines[:safeEnd], "\n")
	return strings.TrimRight(out, "\n")
}

// mergeOutputFiles 合并多组产物清单，按 Path 去重（后出现的覆盖先出现的）。
//
// 用途：工作流任务把各子任务的产物聚合成一份整体交付清单。
// 保持首次出现的顺序 —— 子任务按 DAG 顺序遍历，先产出的排前面更符合阅读预期。
func mergeOutputFiles(groups ...[]model.FileInfo) []model.FileInfo {
	seen := make(map[string]int) // path → 在 out 里的下标
	var out []model.FileInfo
	for _, g := range groups {
		for _, f := range g {
			key := f.Path
			if key == "" {
				key = f.Name
			}
			if i, ok := seen[key]; ok {
				out[i] = f // 同路径以最新的元数据为准（size 可能已变）
				continue
			}
			seen[key] = len(out)
			out = append(out, f)
		}
	}
	return out
}

// aggregateWorkflowOutputFiles 把各节点子任务的产物聚合成工作流级交付清单。
//
// prev 是上一轮已聚合的结果，先放进来保证「历史产物不因节点被重置而丢失」——
// 节点重试会清空 node.Output（见 resetWorkflowNodesForRetry），
// 但文件已经落在工作区里，用户仍需要能下载。
//
// 交付清单只认「汇总输出节点」（DAG 叶子、不被任何节点依赖）的产物。
// 中间调研节点（如探测 / OCR 节点）会写大量脚本与调试图，那些是给下游
// 节点用的过程文件，不是给用户的最终交付物，不该出现在交付清单里。
//
// 注意：这里只过滤交付清单；节点自身的 Output.OutputFiles 保持不变，
// 下游节点仍能通过依赖传递读到上游产出的文件（见 workflow_actuator）。
//
// 节点按 DAG 声明顺序遍历，先产出的排前面，更符合阅读预期。
func aggregateWorkflowOutputFiles(
	prev []model.FileInfo, nodes []model.WorkflowNodeData,
) []model.FileInfo {
	sinks := computeWorkflowSinkIDs(nodes)

	groups := make([][]model.FileInfo, 0, len(nodes)+1)
	groups = append(groups, prev)
	for i := range nodes {
		if !sinks[nodes[i].NodeID] {
			continue
		}
		if nodes[i].Output == nil || len(nodes[i].Output.OutputFiles) == 0 {
			continue
		}
		groups = append(groups, nodes[i].Output.OutputFiles)
	}
	return mergeOutputFiles(groups...)
}

// computeWorkflowSinkIDs 计算 DAG 的「汇总输出节点」id 集合。
//
// 汇总输出节点 = 没有任何其他节点依赖它的节点（depends_on 里没出现它的 id），
// 即 DAG 的叶子。规划期已保证有且只有一个，这里按集合返回以兼容防御。
func computeWorkflowSinkIDs(nodes []model.WorkflowNodeData) map[string]bool {
	depended := make(map[string]bool)
	for i := range nodes {
		for _, d := range nodes[i].DependsOn {
			depended[d] = true
		}
	}
	sinks := make(map[string]bool)
	for i := range nodes {
		if !depended[nodes[i].NodeID] {
			sinks[nodes[i].NodeID] = true
		}
	}
	return sinks
}

// resolveTaskArtifactDir 取本任务的产出目录（相对工作区，可能为空）。
//
// 空串表示顶层对话任务，产出直接落在工作区（outputs/ 或模型自选路径），
// 产物收集退化为按 mtime 扫描 —— 顶层任务之间没有并行重叠，mtime 判得准。
func resolveTaskArtifactDir(temp *model.TaskTemp) string {
	if temp == nil || temp.TaskInput.Data == nil {
		return ""
	}
	return strings.TrimSpace(temp.TaskInput.Data.ArtifactDir)
}

// buildWorkflowNodeArtifactDir 工作流节点子任务的产出目录。
//
// 结构 outputs/wf-<工作流短id>/<节点id>：
//   - wf 短 id 区分同一对话里的多条工作流
//   - 节点 id 由规划模型命名（约定 n<N>_<slug>，如 n1_open_source），
//     本身就有含义，产物一落到哪个节点下看路径即知
func buildWorkflowNodeArtifactDir(wfTaskID, nodeID string) string {
	node := sanitizePathSegment(nodeID, 40)
	if node == "" {
		node = "node"
	}
	return workspacepkg.OutputsDirName + "/wf-" + shortTaskID(wfTaskID) + "/" + node
}

// buildDeepSearchArtifactDir 搜索子任务的产出目录。
//
// 挂在父任务的产出目录下（父任务是对话任务时挂在 outputs/ 下）：
// 搜索子任务是父任务派出去干活的一条支线，它的产出理应随父任务归档，
// 而不是散落到工作区根。
func buildDeepSearchArtifactDir(parentDir, subTaskID string) string {
	base := strings.TrimSpace(parentDir)
	if base == "" {
		base = workspacepkg.OutputsDirName
	}
	return base + "/ds-" + shortTaskID(subTaskID)
}

// shortTaskID 取 task_id 的前 8 位作为短标识。
func shortTaskID(taskID string) string {
	if len(taskID) > 8 {
		return taskID[:8]
	}
	return taskID
}

// sanitizePathSegment 把任意字符串清洗成可安全用作路径分段（目录名/文件名）的形式。
//
// 处理：markdown 标记与文件系统禁用字符 → 去掉或换成 -，空白压缩成 -，
// 首尾去分隔符，长度截断。中文保留（现代文件系统都支持 UTF-8 文件名，
// 且中文对用户更有辨识度）。
func sanitizePathSegment(s string, maxRunes int) string {
	s = strings.NewReplacer(
		"`", "", "*", "", "_", "", "[", "", "]", "", "(", "", ")", "",
		"/", "-", "\\", "-", ":", "-", ";", "-",
		"?", "", "\"", "", "<", "", ">", "", "|", "",
	).Replace(s)
	s = strings.Join(strings.Fields(s), "-")
	s = strings.Trim(s, "-.")
	runes := []rune(s)
	if len(runes) > maxRunes {
		s = string(runes[:maxRunes])
	}
	return strings.Trim(s, "-.")
}

// ============================================================
// 工作区定位与下载链接
// ============================================================

// resolveWorkspaceDirForTask 计算任务的工作区绝对目录。
//
// 与 tool_call 侧的 resolveWorkspaceDir 保持同一套 key 规则（ChatID），
// 否则「工具写文件的目录」与「收口时扫描产物的目录」会对不上。
// 之所以不复用那个函数：它绑定 toolCallRegistry，而 functional_call
// 侧有自己的依赖桥 funcCallRegistry，两个 actuator 可能不在同一进程实例。
// resolveChatWorkspaceDir 按 chat 取工作区目录。
//
// 工作区是**对话级**的：同一对话下的主任务、工作流任务与所有节点子任务
// 共享同一份文件（上游落盘的产物下游要能读到）。因此按 chat_id 定位，
// 而不是按 task_id —— 否则子任务会拿到一个空目录，既读不到用户上传的
// 附件，也读不到上游节点的产出。
func resolveChatWorkspaceDir(chatID string) string {
	if chatID == "" || funcCallRegistry.ws == nil {
		return ""
	}
	return funcCallRegistry.ws.Dir(chatID)
}

func resolveWorkspaceDirForTask(temp *model.TaskTemp) string {
	if temp == nil || funcCallRegistry.ws == nil {
		return ""
	}
	key := temp.ChatID
	if key == "" {
		key = temp.TaskID
	}
	return funcCallRegistry.ws.Dir(key)
}

// fillDownloadURLs 就地给产物补下载链接。
//
// URL 不在 collectTaskArtifacts 里拼：那层只认工作区，不知道 chat_id，
// 而下载端点必须带 chat_id 才能定位工作区并做沙箱校验。
func fillDownloadURLs(chatID string, files []model.FileInfo) {
	if chatID == "" {
		return
	}
	for i := range files {
		if files[i].URL != "" || files[i].Path == "" {
			continue
		}
		files[i].URL = buildWorkspaceDownloadURL(chatID, files[i].Path)
	}
}

// buildWorkspaceDownloadURL 拼工作区文件的下载地址（与 handler 侧口径一致）。
//
// 这里重复一份而不引 handler 包：scheduler 进程不依赖 handler，
// 反向引用会把 gin 等 HTTP 依赖拖进调度器。路径契约很稳定，重复成本低于耦合成本。
func buildWorkspaceDownloadURL(chatID, relPath string) string {
	q := url.Values{}
	q.Set("chat_id", chatID)
	q.Set("path", relPath)
	return "/api/v1/files/download?" + q.Encode()
}

// humanSize 把字节数格式化成人类可读的大小，用于日志与 prompt 文件清单。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, u := range units {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PB", f/unit)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
