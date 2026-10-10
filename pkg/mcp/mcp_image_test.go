package mcp

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// flattenContent 必须把图片从文本里剥离：base64 一旦进 Content 就会撑爆上下文，
// 且对模型没有任何可读信息。这是 computer-use 截图 161KB base64 灌进 tool 结果的根因。
func TestFlattenContentExtractsImage(t *testing.T) {
	imgBytes := []byte("\xff\xd8\xff\xe0fake-jpeg-bytes")
	imgB64 := base64.StdEncoding.EncodeToString(imgBytes)

	contents := []mcp.Content{
		mcp.NewTextContent("先看截图："),
		mcp.NewImageContent(imgB64, "image/jpeg"),
		mcp.NewTextContent("以上就是结果。"),
	}

	text, images := flattenContent(contents)

	// base64 绝不能出现在文本里
	if strings.Contains(text, imgB64) {
		t.Fatalf("base64 图片数据泄漏进了 Content：%s", text)
	}
	if strings.Contains(text, "data:image") {
		t.Fatalf("Content 里出现图片数据：%s", text)
	}

	// 文本块保留，图片块变成占位标记
	for _, want := range []string{"先看截图：", "以上就是结果。", "[image:image/jpeg]"} {
		if !strings.Contains(text, want) {
			t.Errorf("Content 缺少 %q：%s", want, text)
		}
	}

	// 图片字节被提取出来，且解码正确
	if len(images) != 1 {
		t.Fatalf("提取到 %d 张图片，want 1", len(images))
	}
	if images[0].MIMEType != "image/jpeg" {
		t.Errorf("MIMEType = %q, want image/jpeg", images[0].MIMEType)
	}
	if string(images[0].Data) != string(imgBytes) {
		t.Errorf("图片字节解码错误：%q", images[0].Data)
	}
}

// 多张同 mime 图片也必须一一对应，不能合并。
func TestFlattenContentMultipleImagesKeepOrder(t *testing.T) {
	b1 := base64.StdEncoding.EncodeToString([]byte("aaa"))
	b2 := base64.StdEncoding.EncodeToString([]byte("bbb"))

	contents := []mcp.Content{
		mcp.NewImageContent(b1, "image/png"),
		mcp.NewImageContent(b2, "image/png"),
	}

	text, images := flattenContent(contents)

	if len(images) != 2 {
		t.Fatalf("提取到 %d 张图片，want 2", len(images))
	}
	if string(images[0].Data) != "aaa" || string(images[1].Data) != "bbb" {
		t.Errorf("图片顺序错乱：%q, %q", images[0].Data, images[1].Data)
	}
	// 两个占位标记都在，供调用方逐个 Replace
	if strings.Count(text, "[image:image/png]") != 2 {
		t.Errorf("占位标记数量 = %d，want 2：%s", strings.Count(text, "[image:image/png]"), text)
	}
}

// 无图片时零变化：不能因为加了图片处理就影响纯文本 MCP 工具。
func TestFlattenContentTextOnlyUnchanged(t *testing.T) {
	contents := []mcp.Content{mcp.NewTextContent("纯文本结果")}
	text, images := flattenContent(contents)
	if text != "纯文本结果" {
		t.Errorf("纯文本被改写：%q", text)
	}
	if len(images) != 0 {
		t.Errorf("纯文本不该提取出图片：%d", len(images))
	}
}

// base64 损坏时不能静默吞掉图片，占位要标注解码失败。
func TestFlattenContentBrokenBase64(t *testing.T) {
	contents := []mcp.Content{mcp.NewImageContent("!!not-base64!!", "image/png")}
	text, images := flattenContent(contents)
	if !strings.Contains(text, "解码失败") {
		t.Errorf("损坏的 base64 未标注：%s", text)
	}
	if len(images) != 0 {
		t.Errorf("损坏的 base64 不该进 images：%d", len(images))
	}
}

// SanitizeRaw 必须把 raw 里 image content 的 base64 剥离 —— 调试页落库用，
// 否则 turn.output 里会存上百 KB 的 base64，撑爆落库与调试页传输。
func TestSanitizeRawStripsImageData(t *testing.T) {
	big := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 5000)))

	raw := &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent("文本保留"),
			mcp.NewImageContent(big, "image/jpeg"),
		},
	}

	got := SanitizeRaw(raw).(*mcp.CallToolResult)

	// 文本原样保留
	if tc, ok := mcp.AsTextContent(got.Content[0]); !ok || tc.Text != "文本保留" {
		t.Errorf("文本 content 被改动：%+v", got.Content[0])
	}
	// 图片 data 被替换成占位，不再含 base64
	ic, ok := mcp.AsImageContent(got.Content[1])
	if !ok {
		t.Fatalf("图片 content 丢失")
	}
	if strings.Contains(ic.Data, big) || len(ic.Data) > 200 {
		t.Errorf("base64 未被剥离：len=%d", len(ic.Data))
	}
	if ic.MIMEType != "image/jpeg" {
		t.Errorf("MIMEType 被改动：%q", ic.MIMEType)
	}
}

// SanitizeRaw 对非 *mcp.CallToolResult 要原样返回，不 panic。
func TestSanitizeRawNonMCP(t *testing.T) {
	if got := SanitizeRaw("some-string"); got != "some-string" {
		t.Errorf("非 MCP 类型被改动：%v", got)
	}
	if got := SanitizeRaw(nil); got != nil {
		t.Errorf("nil 被改动：%v", got)
	}
}
