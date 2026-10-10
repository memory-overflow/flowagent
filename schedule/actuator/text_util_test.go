package actuator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// TestRenderUserInputNoFiles 无附件时只返回输入文本，不加任何噪音。
func TestRenderUserInputNoFiles(t *testing.T) {
	in := &model.TaskInputData{InputText: "帮我写篇文章"}
	got := renderUserInput(in, "")
	if got != "帮我写篇文章" {
		t.Errorf("无附件时应原样返回输入文本, got %q", got)
	}
	if renderUserInput(nil, "") != "" {
		t.Errorf("nil 输入应返回空串")
	}
}

// TestRenderUserInputManifest 附件清单要含路径/类型/大小与处理指引。
// 只给文件名的话模型会忽略附件或臆造内容，这几项都是必需信息。
func TestRenderUserInputManifest(t *testing.T) {
	in := &model.TaskInputData{
		InputText: "分析这些材料",
		InputFiles: []model.FileInfo{
			{Name: "需求.md", Path: "inputs/需求.md", Size: 2048,
				MimeType: "text/markdown", FileType: workspacepkg.FileTypeText},
			{Name: "报表.xlsx", Path: "inputs/报表.xlsx", Size: 5 << 20,
				FileType: workspacepkg.FileTypeOther},
			{Name: "截图.png", Path: "inputs/截图.png", Size: 1024,
				FileType: workspacepkg.FileTypeImage},
		},
	}
	got := renderUserInput(in, "")

	// 输入文本必须保留
	if !strings.Contains(got, "分析这些材料") {
		t.Errorf("丢失了用户输入文本")
	}
	// 每个附件的路径都要出现（模型据此调 read_file）
	for _, p := range []string{"inputs/需求.md", "inputs/报表.xlsx", "inputs/截图.png"} {
		if !strings.Contains(got, p) {
			t.Errorf("清单缺少路径 %s", p)
		}
	}
	// 类型标签
	for _, label := range []string{"文本", "二进制/其他", "图片"} {
		if !strings.Contains(got, label) {
			t.Errorf("清单缺少类型标签 %s", label)
		}
	}
	// 大小要人类可读
	if !strings.Contains(got, "2.0 KB") || !strings.Contains(got, "5.0 MB") {
		t.Errorf("清单缺少可读大小: %s", got)
	}
	// 三类处理指引都要给
	if !strings.Contains(got, "read_file") {
		t.Errorf("缺少文本类处理指引")
	}
	if !strings.Contains(got, "乱码") {
		t.Errorf("缺少二进制文件处理指引（不要直接 read_file）")
	}
	if !strings.Contains(got, "图片理解") {
		t.Errorf("缺少图片处理指引（指向系统图片策略）")
	}
}

// TestRenderUserInputInlinePreview 小体积文本附件应内联预览，省掉 read_file 往返。
func TestRenderUserInputInlinePreview(t *testing.T) {
	ws := t.TempDir()
	inputsDir := filepath.Join(ws, workspacepkg.InputsDirName)
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# 项目需求\n\n1. 支持文件上传\n2. 支持产物下载\n"
	if err := os.WriteFile(filepath.Join(inputsDir, "spec.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	in := &model.TaskInputData{
		InputText: "按需求实现",
		InputFiles: []model.FileInfo{
			{Name: "spec.md", Path: "inputs/spec.md", Size: int64(len(content)),
				FileType: workspacepkg.FileTypeText},
		},
	}

	// 传入 wsDir → 应内联预览
	withWs := renderUserInput(in, ws)
	if !strings.Contains(withWs, "支持文件上传") {
		t.Errorf("小体积文本附件应内联预览正文, got:\n%s", withWs)
	}
	if !strings.Contains(withWs, "```markdown") {
		t.Errorf("预览应带 markdown 代码块语言标注")
	}

	// 不传 wsDir → 只有清单，无预览（历史轮次场景）
	withoutWs := renderUserInput(in, "")
	if strings.Contains(withoutWs, "支持文件上传") {
		t.Errorf("wsDir 为空时不应内联预览（避免历史回放撑爆上下文）")
	}
	if !strings.Contains(withoutWs, "inputs/spec.md") {
		t.Errorf("wsDir 为空时仍应保留清单路径")
	}
}

// TestRenderUserInputSkipsLargeFilePreview 超过体积阈值的文本文件不内联，只给路径。
func TestRenderUserInputSkipsLargeFilePreview(t *testing.T) {
	ws := t.TempDir()
	inputsDir := filepath.Join(ws, workspacepkg.InputsDirName)
	os.MkdirAll(inputsDir, 0o755)
	big := strings.Repeat("x", inputPreviewMaxBytes+100)
	os.WriteFile(filepath.Join(inputsDir, "big.txt"), []byte(big), 0o644)

	in := &model.TaskInputData{
		InputText: "处理大文件",
		InputFiles: []model.FileInfo{
			{Name: "big.txt", Path: "inputs/big.txt", Size: int64(len(big)),
				FileType: workspacepkg.FileTypeText},
		},
	}
	got := renderUserInput(in, ws)
	if strings.Contains(got, "附件内容预览") {
		t.Errorf("超阈值文件不应内联预览")
	}
	if !strings.Contains(got, "inputs/big.txt") {
		t.Errorf("超阈值文件仍应出现在清单里")
	}
}

// TestReadFilePreviewRejectsTraversal 预览读取必须走沙箱校验。
func TestReadFilePreviewRejectsTraversal(t *testing.T) {
	ws := t.TempDir()
	// 在工作区外放一个文件
	outside := filepath.Join(filepath.Dir(ws), "secret.txt")
	os.WriteFile(outside, []byte("SECRET"), 0o644)
	defer os.Remove(outside)

	for _, bad := range []string{"../secret.txt", "/etc/passwd", "../../secret.txt"} {
		if preview, _ := readFilePreview(ws, bad, 100); preview != "" {
			t.Errorf("readFilePreview(%q) 应被沙箱拒绝, got %q", bad, preview)
		}
	}
}

// TestAggregateWorkflowOutputFiles 工作流产物聚合：并集去重 + 保留历史产物。
func TestAggregateWorkflowOutputFiles(t *testing.T) {
	nodes := []model.WorkflowNodeData{
		{NodeID: "n1", Output: &model.AgentOutputData{
			OutputFiles: []model.FileInfo{{Name: "draft.md", Path: "outputs/draft.md", Size: 100}},
		}},
		{NodeID: "n2", Output: nil}, // 尚未完成
		{NodeID: "n3", Output: &model.AgentOutputData{
			OutputFiles: []model.FileInfo{
				{Name: "draft.md", Path: "outputs/draft.md", Size: 200}, // 同路径已更新
				{Name: "final.md", Path: "outputs/final.md", Size: 300},
			},
		}},
	}
	// prev 里有一个已被重置节点留下的历史产物
	prev := []model.FileInfo{{Name: "old.md", Path: "outputs/old.md", Size: 50}}

	got := aggregateWorkflowOutputFiles(prev, nodes)

	paths := make(map[string]int64)
	for _, f := range got {
		paths[f.Path] = f.Size
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 个产物（去重后）, got %d: %+v", len(got), got)
	}
	// 节点重置后历史产物不能丢（文件仍在工作区里）
	if _, ok := paths["outputs/old.md"]; !ok {
		t.Errorf("历史产物 old.md 丢失")
	}
	// 同路径以最新元数据为准
	if paths["outputs/draft.md"] != 200 {
		t.Errorf("draft.md size = %d, want 200（应取最新）", paths["outputs/draft.md"])
	}
	if _, ok := paths["outputs/final.md"]; !ok {
		t.Errorf("final.md 丢失")
	}
}

// TestHasImageRelatedTool 图像类工具识别：按工具名/描述关键词匹配。
func TestHasImageRelatedTool(t *testing.T) {
	// nil 工具集 → false
	if hasImageRelatedTool(nil) {
		t.Errorf("nil 工具集应返回 false")
	}

	// 仅普通工具 → false
	plain := newToolSet([]ToolEntry{
		{ExposedName: "read_file", Description: "读取工作区文件内容"},
		{ExposedName: "get_current_time", Description: "获取当前时间"},
	})
	if hasImageRelatedTool(plain) {
		t.Errorf("无图像类工具应返回 false")
	}

	// 含 OCR 工具（名命中）→ true
	ocrByName := newToolSet([]ToolEntry{
		{ExposedName: "ocr_image", Description: "识别图片中的文字"},
	})
	if !hasImageRelatedTool(ocrByName) {
		t.Errorf("工具名含 ocr/image 应返回 true")
	}

	// 含图像类工具（描述命中，中文）→ true
	ocrByDesc := newToolSet([]ToolEntry{
		{ExposedName: "mcp__foo__bar", Description: "从图片中提取文字，支持文字识别"},
	})
	if !hasImageRelatedTool(ocrByDesc) {
		t.Errorf("描述含图像/文字识别关键词应返回 true")
	}

	// 大小写不敏感（英文关键词）
	ocrUpper := newToolSet([]ToolEntry{
		{ExposedName: "VisionOCR", Description: "extract text"},
	})
	if !hasImageRelatedTool(ocrUpper) {
		t.Errorf("英文关键词大小写不敏感匹配应返回 true")
	}
}

func TestCodeFenceLang(t *testing.T) {
	cases := map[string]string{
		"a.go": "go", "a.py": "python", "a.md": "markdown",
		"a.json": "json", "a.yml": "yaml", "a.unknown": "",
	}
	for name, want := range cases {
		if got := codeFenceLang(name); got != want {
			t.Errorf("codeFenceLang(%s) = %q, want %q", name, got, want)
		}
	}
}
