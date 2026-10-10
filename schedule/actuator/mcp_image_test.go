package actuator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
)

// materializeMCPImages 必须把 [image:<mime>] 占位落盘成文件引用，
// base64 绝不能回灌进工具结果。
func TestMaterializeMCPImagesDumpsToFile(t *testing.T) {
	ws := t.TempDir()
	img := mcppkg.ToolImage{Data: []byte("\xff\xd8\xff\xe0test"), MIMEType: "image/jpeg"}

	content := "[image:image/jpeg]"
	got := materializeMCPImages(content, []mcppkg.ToolImage{img}, ws)

	if strings.Contains(got, "[image:image/jpeg]") {
		t.Errorf("占位标记没被替换：%s", got)
	}
	if !strings.Contains(got, "analyze_image") {
		t.Errorf("替换文本没引导模型用 analyze_image：%s", got)
	}
	if !strings.Contains(got, "outputs/") {
		t.Errorf("替换文本没给出相对路径：%s", got)
	}

	// 文件真的落盘了，且内容一致
	entries, err := os.ReadDir(filepath.Join(ws, "outputs"))
	if err != nil {
		t.Fatalf("outputs 目录没建：%v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("落盘文件数 = %d, want 1", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(ws, "outputs", entries[0].Name()))
	if err != nil {
		t.Fatalf("读回落盘文件失败：%v", err)
	}
	if string(b) != string(img.Data) {
		t.Errorf("落盘内容不一致：%q", b)
	}
}

// 同 mime 多图按顺序一一落盘，替换不能错位。
func TestMaterializeMCPImagesMultipleKeepOrder(t *testing.T) {
	ws := t.TempDir()
	imgs := []mcppkg.ToolImage{
		{Data: []byte("first"), MIMEType: "image/png"},
		{Data: []byte("second"), MIMEType: "image/png"},
	}
	content := "[image:image/png]\n[image:image/png]"
	got := materializeMCPImages(content, imgs, ws)

	if strings.Contains(got, "[image:image/png]") {
		t.Errorf("还有占位标记没替换：%s", got)
	}
	entries, _ := os.ReadDir(filepath.Join(ws, "outputs"))
	if len(entries) != 2 {
		t.Fatalf("落盘文件数 = %d, want 2", len(entries))
	}
	// 两个文件内容分别是 first / second
	var gotContents []string
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(ws, "outputs", e.Name()))
		gotContents = append(gotContents, string(b))
	}
	if !strings.Contains(strings.Join(gotContents, ","), "first") ||
		!strings.Contains(strings.Join(gotContents, ","), "second") {
		t.Errorf("多图落盘内容错乱：%v", gotContents)
	}
}

// 无工作区时降级为提示，绝不能把 base64 塞回来。
func TestMaterializeMCPImagesNoWorkspace(t *testing.T) {
	img := mcppkg.ToolImage{Data: []byte("base64bytes"), MIMEType: "image/png"}
	got := materializeMCPImages("[image:image/png]", []mcppkg.ToolImage{img}, "")

	if strings.Contains(got, "base64bytes") {
		t.Errorf("无工作区时 base64 被回灌：%s", got)
	}
	if !strings.Contains(got, "未保存") {
		t.Errorf("无工作区时没有降级提示：%s", got)
	}
}

// 无图片时零开销原样返回。
func TestMaterializeMCPImagesNoImages(t *testing.T) {
	got := materializeMCPImages("纯文本", nil, t.TempDir())
	if got != "纯文本" {
		t.Errorf("无图片时被改写：%q", got)
	}
}

// mime → 扩展名映射。
func TestImageExt(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":      ".jpg",
		"image/png":       ".png",
		"image/webp":      ".webp",
		"image/svg+xml":   ".svg",
		"image/heic":      ".heic", // 短后缀直接取 mime 后半段
		"image/x-unknown": ".img",  // 后缀过长不合法，兜底 .img
		"":                ".img",  // 空 mime 兜底
	}
	for mime, want := range cases {
		if got := imageExt(mime); got != want {
			t.Errorf("imageExt(%q) = %q, want %q", mime, got, want)
		}
	}
}
