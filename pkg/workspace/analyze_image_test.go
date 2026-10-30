package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAnalyzeImageToolMeta 校验图片理解工具的元信息：名字与必填 path 参数。
func TestAnalyzeImageToolMeta(t *testing.T) {
	def := AnalyzeImageTool()
	if def == nil {
		t.Fatal("AnalyzeImageTool 返回 nil")
	}
	if def.Name != "analyze_image" {
		t.Errorf("工具名应为 analyze_image, got %s", def.Name)
	}
	if def.Timeout != 15*time.Minute {
		t.Errorf("工具超时应与 functional call 对齐为 15 分钟, got %s", def.Timeout)
	}
	props, _ := def.InputSchema["properties"].(map[string]interface{})
	if props == nil {
		t.Fatal("InputSchema 缺少 properties")
	}
	if _, ok := props["path"]; !ok {
		t.Errorf("schema 缺少 path 参数")
	}
}

// TestAnalyzeImageNilAnalyzer 未注入 VisionAnalyzer 时（非多模态模型）应直接报错，
// 而不是退回 OCR 等代码化方案。
func TestAnalyzeImageNilAnalyzer(t *testing.T) {
	def := AnalyzeImageTool()
	_, err := def.Handler(context.Background(), &Context{WorkspaceDir: t.TempDir()},
		map[string]interface{}{"path": "a.png"})
	if err == nil {
		t.Fatal("VisionAnalyzer 为 nil 时应报错")
	}
	if !strings.Contains(err.Error(), "不支持图片理解") {
		t.Errorf("错误信息应说明不支持图片理解, got: %v", err)
	}
}

// TestAnalyzeImageCallsAnalyzer 正常路径：读图字节、按扩展名推断 MIME、透传 question，
// 并把结果原样返回给调用方。
func TestAnalyzeImageCallsAnalyzer(t *testing.T) {
	ws := t.TempDir()
	raw := []byte("\x89PNG fake image bytes")
	if err := os.WriteFile(filepath.Join(ws, "img.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	def := AnalyzeImageTool()
	var gotMime, gotQuestion string
	var gotDataLen int
	analyzer := func(ctx context.Context, imageData []byte, mimeType, question string) (string, error) {
		gotMime = mimeType
		gotQuestion = question
		gotDataLen = len(imageData)
		return "这是描述", nil
	}

	out, err := def.Handler(context.Background(), &Context{
		WorkspaceDir:   ws,
		VisionAnalyzer: analyzer,
	}, map[string]interface{}{"path": "img.png", "question": "图里有什么"})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out != "这是描述" {
		t.Errorf("应返回回调结果, got %q", out)
	}
	if gotMime != "image/png" {
		t.Errorf("mime 应为 image/png, got %s", gotMime)
	}
	if gotQuestion != "图里有什么" {
		t.Errorf("question 应透传, got %q", gotQuestion)
	}
	if gotDataLen != len(raw) {
		t.Errorf("imageData 长度应为 %d, got %d", len(raw), gotDataLen)
	}
}

// TestImageMimeType 校验扩展名 → MIME 映射，未知扩展名回落 image/*。
func TestImageMimeType(t *testing.T) {
	cases := map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".bmp":  "image/bmp",
		".tiff": "image/*",
	}
	for ext, want := range cases {
		if got := imageMimeType(ext); got != want {
			t.Errorf("imageMimeType(%q) = %q, want %q", ext, got, want)
		}
	}
}
