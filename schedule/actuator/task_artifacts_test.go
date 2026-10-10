package actuator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// TestDumpLongReplyBelowThreshold 未超阈值的回复原样返回，不产生文件。
// 正常问答、状态汇报不应被误转存。
func TestDumpLongReplyBelowThreshold(t *testing.T) {
	ws := t.TempDir()
	reply := strings.Repeat("短", 100)

	got, dumped := dumpLongReply(ws, "", "task-1", reply)
	if got != reply {
		t.Errorf("未超阈值时应原样返回")
	}
	if dumped != nil {
		t.Errorf("未超阈值时不应产生文件, got %+v", dumped)
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, workspacepkg.OutputsDirName)); len(entries) > 0 {
		t.Errorf("未超阈值时不应创建 outputs 目录内容")
	}
}

// TestDumpLongReplyAboveThreshold 超阈值时转存为文件，回复替换为摘要+引用。
// 这是防止长文章撑爆 task_output 与后续 LLM 上下文的确定性兜底。
func TestDumpLongReplyAboveThreshold(t *testing.T) {
	ws := t.TempDir()
	// 构造一篇超过 maxInlineReplyRunes 的长文
	body := "文章开头导语。" + strings.Repeat("正文内容", maxInlineReplyRunes)
	totalRunes := len([]rune(body))

	got, dumped := dumpLongReply(ws, "", "12c2e91d-5edf-4fdf", body)

	if dumped == nil {
		t.Fatalf("超阈值时应产生转存文件")
	}
	if got == body {
		t.Errorf("超阈值时回复应被替换为摘要")
	}
	if r := []rune(got); len(r) > replySummaryRunes+400 {
		t.Errorf("替换后的回复过长: %d runes", len(r))
	}
	// 回复里必须带上文件路径，否则用户拿不到完整正文
	if !strings.Contains(got, dumped.Path) {
		t.Errorf("替换后的回复未包含文件路径 %s: %s", dumped.Path, got)
	}
	// 摘要要保留正文开头，让用户能判断是不是想要的东西
	if !strings.HasPrefix(got, "文章开头导语。") {
		t.Errorf("摘要未保留正文开头: %.60s", got)
	}

	// 文件必须真的落在 outputs/ 下且内容完整
	if !strings.HasPrefix(dumped.Path, workspacepkg.OutputsDirName+"/") {
		t.Errorf("转存路径应在 outputs/ 下, got %s", dumped.Path)
	}
	abs := filepath.Join(ws, dumped.Path)
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("转存文件读取失败: %v", err)
	}
	if string(raw) != body {
		t.Errorf("转存文件内容与原文不一致（长度 %d vs %d）", len([]rune(string(raw))), totalRunes)
	}
	if dumped.FileType != workspacepkg.FileTypeText {
		t.Errorf("转存文件应为文本类, got %d", dumped.FileType)
	}
}

// TestDumpLongReplyNoWorkspace 没有工作区时保留原文，不能静默截断用户产物。
func TestDumpLongReplyNoWorkspace(t *testing.T) {
	body := strings.Repeat("正文", maxInlineReplyRunes)
	got, dumped := dumpLongReply("", "", "task-1", body)
	if got != body {
		t.Errorf("无工作区时应保留原文，不能截断")
	}
	if dumped != nil {
		t.Errorf("无工作区时不应产生文件")
	}
}

// TestTruncateMarkdownKeepsTableIntact 截断不得把 markdown 表格切成半张表。
//
// 这是真实事故：一个 Agent 平台选型调研里，转存摘要被硬切在
// 「| 工具编排 | **原生** |」处，用户看到的是语法破损的半行表格。
func TestTruncateMarkdownKeepsTableIntact(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 选型报告\n\n导语段落。\n\n")
	b.WriteString("| 框架 | 编排 | 语言 |\n|---|---|---|\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "| 框架%d | 原生 | Go |\n", i)
	}

	got := truncateMarkdown(b.String(), replySummaryRunes)
	if r := []rune(got); len(r) > replySummaryRunes+200 {
		t.Fatalf("截断后仍超预算: %d runes", len(r))
	}
	// 每条被保留的表格行都必须是完整行（以 | 结尾）
	for _, ln := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "|") && !strings.HasSuffix(trimmed, "|") {
			t.Fatalf("出现被切断的表格行: %q", ln)
		}
	}
	// 表格一旦出现表头分隔行，就必须有至少一条数据行跟随，不能孤零零留个头
	if strings.Contains(got, "|---|---|---|") && strings.HasSuffix(strings.TrimSpace(got), "|---|---|---|") {
		t.Fatalf("表格只剩表头分隔行")
	}
}

// TestTruncateMarkdownKeepsFenceBalanced 截断不得留下未闭合的代码围栏。
func TestTruncateMarkdownKeepsFenceBalanced(t *testing.T) {
	var b strings.Builder
	b.WriteString("导语。\n\n")
	b.WriteString("```go\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "func f%d() {}\n", i)
	}
	b.WriteString("```\n\n结尾段落。\n")

	got := truncateMarkdown(b.String(), replySummaryRunes)
	if n := strings.Count(got, "```"); n%2 != 0 {
		t.Fatalf("代码围栏未闭合（出现 %d 次围栏标记）: %q", n, got[len(got)-min2(len(got), 80):])
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestTruncateMarkdownBelowBudget 未超预算时原样返回，不做任何加工。
func TestTruncateMarkdownBelowBudget(t *testing.T) {
	in := "| a | b |\n|---|---|\n| 1 | 2 |\n"
	if got := truncateMarkdown(in, replySummaryRunes); got != in {
		t.Fatalf("未超预算时应原样返回, got %q", got)
	}
}

// TestDumpLongReplyUsesHeadingAsFileName 转存文件应取正文标题作文件名。
//
// 「reply-12c2e91d-20260923-153802.md」这种时间戳名字对用户毫无信息量，
// 产物列表里一排这样的文件根本分不清谁是谁。
func TestDumpLongReplyUsesHeadingAsFileName(t *testing.T) {
	ws := t.TempDir()
	body := "# Agent 平台选型调研报告\n\n" + strings.Repeat("正文内容。", maxInlineReplyRunes)

	_, dumped := dumpLongReply(ws, "outputs/wf-ac049a2f/n1_open_source", "task-1", body)
	if dumped == nil {
		t.Fatalf("超阈值时应产生转存文件")
	}
	if !strings.Contains(dumped.Name, "Agent") || !strings.HasSuffix(dumped.Name, ".md") {
		t.Fatalf("文件名应取正文标题, got %q", dumped.Name)
	}
	if dumped.Name == "reply-task-1.md" {
		t.Fatalf("不应回落到 task_id 命名")
	}
	// 必须落在本任务的产出目录下，而不是 outputs/ 根
	if !strings.HasPrefix(dumped.Path, "outputs/wf-ac049a2f/n1_open_source/") {
		t.Fatalf("转存路径应在任务产出目录下, got %s", dumped.Path)
	}
}

// TestCollectTaskArtifactsScopedToDir 有独立产出目录时只收本目录的产物。
//
// 这是并行节点产物串味的回归测试：工作区是对话级的，两个并行节点执行窗口
// 重叠，按 mtime 扫描会让每个节点都把所有人的文件算成自己的。
func TestCollectTaskArtifactsScopedToDir(t *testing.T) {
	ws := t.TempDir()
	dirA := "outputs/wf-aaaaaaaa/n1_open_source"
	dirB := "outputs/wf-aaaaaaaa/n2_cloud_service"

	write := func(rel, content string) {
		abs := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}
	write(dirA+"/open-source-report.md", "A 的产物")
	write(dirB+"/cloud-report.md", "B 的产物")

	files := collectTaskArtifacts(ws, nil, dirA)
	if len(files) != 1 {
		t.Fatalf("只应收本任务目录的产物, got %d: %+v", len(files), files)
	}
	if !strings.HasPrefix(files[0].Path, dirA+"/") {
		t.Fatalf("产物不属于本任务目录: %s", files[0].Path)
	}
}

// TestCollectTaskArtifacts 产物收集要能识别任务开始后写入的文件。
func TestCollectTaskArtifacts(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()

	// 任务开始前的历史文件
	old := filepath.Join(ws, "old.md")
	os.WriteFile(old, []byte("old"), 0o644)
	past := start.Add(-time.Hour)
	os.Chtimes(old, past, past)

	// 任务产出
	newFile := filepath.Join(ws, "report.md")
	os.WriteFile(newFile, []byte("新报告"), 0o644)
	future := start.Add(time.Minute)
	os.Chtimes(newFile, future, future)

	files := collectTaskArtifacts(ws, &start, "")
	if len(files) != 1 {
		t.Fatalf("期望 1 个产物, got %d: %+v", len(files), files)
	}
	if files[0].Path != "report.md" {
		t.Errorf("产物路径不对: %s", files[0].Path)
	}
	if files[0].Source != "local" {
		t.Errorf("产物 source 应为 local, got %s", files[0].Source)
	}
}

// TestCollectTaskArtifactsNilStart start_time 缺失时不报产物，
// 避免把工作区历史文件全量误报为本次产出。
func TestCollectTaskArtifactsNilStart(t *testing.T) {
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "a.md"), []byte("x"), 0o644)

	if files := collectTaskArtifacts(ws, nil, ""); files != nil {
		t.Errorf("start_time 为 nil 时应返回 nil, got %+v", files)
	}
	zero := time.Time{}
	if files := collectTaskArtifacts(ws, &zero, ""); files != nil {
		t.Errorf("start_time 为零值时应返回 nil, got %+v", files)
	}
}

// TestCollectTaskArtifactsScopedToDirAlsoRoot 回归：artifactDir 非空时，除了
// 本任务目录和 outputs/ 直属，还要补收工作区根目录直属文件（按 mtime 过滤）。
//
// 根因回归：模型用 run_command 生成文件时，相对路径落到工作区根（run_command
// 的 cwd 是工作区根，不像 write_file 会写进 artifactDir）。只扫本任务目录会漏掉
// 这些根目录产物 → 工作流结束没有产物列表（前端因 output_files 空不渲染）。
func TestCollectTaskArtifactsScopedToDirAlsoRoot(t *testing.T) {
	ws := t.TempDir()
	dirA := "outputs/wf-aaaaaaaa/n2_build_docx"
	start := time.Now()

	write := func(rel, content string, mtime time.Time) {
		abs := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write failed: %v", err)
		}
		if err := os.Chtimes(abs, mtime, mtime); err != nil {
			t.Fatalf("chtimes failed: %v", err)
		}
	}

	write(dirA+"/report.docx", "本任务目录产物", start.Add(time.Minute))
	write("report_converted.docx", "根目录产物", start.Add(time.Minute))
	write("old-root.md", "历史", start.Add(-time.Hour))

	files := collectTaskArtifacts(ws, &start, dirA)

	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
	}
	if !got["report_converted.docx"] {
		t.Fatalf("根目录直属产物未补收，got %v", got)
	}
	if got["old-root.md"] {
		t.Fatalf("历史根目录文件被误收，got %v", got)
	}
	if !got[dirA+"/report.docx"] {
		t.Fatalf("本任务目录产物丢失，got %v", got)
	}
}

// TestIsFrameworkArtifactDir 框架自有目录判定：outputs/wf-* 与 outputs/ds-* 是
// 其它任务的专属产出目录，模型自选的 outputs/<自定义目录>/ 不是。
func TestIsFrameworkArtifactDir(t *testing.T) {
	cases := map[string]bool{
		"outputs/wf-abc123/n1/x.md":         true,
		"outputs/ds-abc123/r.md":            true,
		"outputs/golden-coat-penguin/x.png": false,
		"outputs/report.md":                 false,
		"outputs":                           false,
		"foo/x.md":                          false,
	}
	for p, want := range cases {
		if got := isFrameworkArtifactDir(p); got != want {
			t.Errorf("isFrameworkArtifactDir(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestCollectTaskArtifactsScopedToDirAlsoOutputsSubdir 回归：artifactDir 非空时，
// 除了本任务目录，还要补收 outputs/ 下模型自选的子目录（如 outputs/golden-coat-penguin/），
// 但排除框架自有的 outputs/wf-*/ 与 outputs/ds-*/（其它任务的专属目录）。
func TestCollectTaskArtifactsScopedToDirAlsoOutputsSubdir(t *testing.T) {
	ws := t.TempDir()
	dirA := "outputs/wf-aaaaaaaa/n2_build"

	write := func(rel, content string) {
		abs := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	write(dirA+"/own.md", "本任务目录产物")
	write("outputs/golden-coat-penguin/image-0.png", "img")
	write("outputs/golden-coat-penguin/textured-model/extracted/x.glb", "glb")
	// 其它任务的框架目录：应排除
	write("outputs/wf-bbbbbbbb/n1_other/o.md", "别节点")
	write("outputs/ds-cccccccc/r.md", "deepsearch")

	files := collectTaskArtifacts(ws, nil, dirA)
	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
	}

	for _, want := range []string{
		dirA + "/own.md",
		"outputs/golden-coat-penguin/image-0.png",
		"outputs/golden-coat-penguin/textured-model/extracted/x.glb",
	} {
		if !got[want] {
			t.Fatalf("缺 %q，got %v", want, got)
		}
	}
	for _, bad := range []string{
		"outputs/wf-bbbbbbbb/n1_other/o.md",
		"outputs/ds-cccccccc/r.md",
	} {
		if got[bad] {
			t.Fatalf("不应收框架目录 %q，got %v", bad, got)
		}
	}
}

// TestMergeOutputFiles 按 path 去重，保留首次出现顺序，后者覆盖元数据。
func TestMergeOutputFiles(t *testing.T) {
	a := []model.FileInfo{
		{Name: "a.md", Path: "a.md", Size: 10},
		{Name: "b.md", Path: "b.md", Size: 20},
	}
	b := []model.FileInfo{
		{Name: "a.md", Path: "a.md", Size: 99}, // 同路径，size 已变
		{Name: "c.md", Path: "c.md", Size: 30},
	}

	got := mergeOutputFiles(a, b)
	if len(got) != 3 {
		t.Fatalf("期望 3 个文件（去重后）, got %d: %+v", len(got), got)
	}
	// 顺序：a, b, c（首次出现顺序）
	wantOrder := []string{"a.md", "b.md", "c.md"}
	for i, w := range wantOrder {
		if got[i].Path != w {
			t.Errorf("位置 %d 应为 %s, got %s", i, w, got[i].Path)
		}
	}
	// a.md 的 size 应被后者覆盖
	if got[0].Size != 99 {
		t.Errorf("同路径应以最新元数据为准, size = %d, want 99", got[0].Size)
	}
}

// TestAggregateWorkflowOutputFilesOnlySinks 交付清单只认汇总输出节点（sink）的产物。
//
// 中间调研节点（探测/OCR）写的脚本与调试图是过程文件，不应进入最终交付清单；
// 只有不被任何节点依赖的叶子节点（汇总输出节点）的产物 + 历史交付物才计入。
func TestAggregateWorkflowOutputFilesOnlySinks(t *testing.T) {
	prev := []model.FileInfo{{Name: "history.md", Path: "history.md"}}

	nodes := []model.WorkflowNodeData{
		{
			NodeID: "n1_probe", DependsOn: nil,
			Output: &model.AgentOutputData{OutputFiles: []model.FileInfo{
				{Name: "probe_ocr.py", Path: "probe_ocr.py"},
				{Name: "ocr.txt", Path: "probe_out/ocr.txt"},
			}},
		},
		{
			NodeID: "n2_probe", DependsOn: nil,
			Output: &model.AgentOutputData{OutputFiles: []model.FileInfo{
				{Name: "probe_pixels.py", Path: "probe_pixels.py"},
			}},
		},
		{
			NodeID: "n3_summary", DependsOn: []string{"n1_probe", "n2_probe"},
			Output: &model.AgentOutputData{OutputFiles: []model.FileInfo{
				{Name: "image_tags.md", Path: "image_tags.md"},
			}},
		},
	}

	got := aggregateWorkflowOutputFiles(prev, nodes)

	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}

	want := []string{"history.md", "image_tags.md"}
	if len(paths) != len(want) {
		t.Fatalf("交付清单应只含历史产物 + sink 节点产物，期望 %v, got %v", want, paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("位置 %d 应为 %s, got %s", i, want[i], paths[i])
		}
	}
}

// TestComputeWorkflowSinkIDs 验证叶子节点（不被依赖者）判定。
func TestComputeWorkflowSinkIDs(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", DependsOn: nil},
		{NodeID: "n2", DependsOn: nil},
		{NodeID: "n3", DependsOn: []string{"n1", "n2"}},
	}

	sinks := computeWorkflowSinkIDs(nodes)
	if len(sinks) != 1 || !sinks["n3"] {
		t.Fatalf("唯一 sink 应为 n3, got %v", sinks)
	}
}

// TestFillDownloadURLs 下载链接要带 chat_id 与 path，且已有 URL 不被覆盖。
func TestFillDownloadURLs(t *testing.T) {
	files := []model.FileInfo{
		{Name: "a.md", Path: "outputs/a.md"},
		{Name: "b.md", Path: "b.md", URL: "http://existing"},
		{Name: "noPath"},
	}
	fillDownloadURLs("chat-9", files)

	if !strings.Contains(files[0].URL, "chat_id=chat-9") {
		t.Errorf("URL 缺少 chat_id: %s", files[0].URL)
	}
	if !strings.Contains(files[0].URL, "path=") {
		t.Errorf("URL 缺少 path: %s", files[0].URL)
	}
	if files[1].URL != "http://existing" {
		t.Errorf("已有 URL 不应被覆盖, got %s", files[1].URL)
	}
	if files[2].URL != "" {
		t.Errorf("无 path 的文件不应生成 URL, got %s", files[2].URL)
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{2048, "2.0 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, c := range cases {
		if got := humanSize(c.n); got != c.want {
			t.Errorf("humanSize(%d) = %s, want %s", c.n, got, c.want)
		}
	}
}
