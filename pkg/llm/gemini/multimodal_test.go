package gemini

import (
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestGeminiUserParts_Multimodal 验证多模态 user parts：无图纯 text，有图 text + inlineData。
func TestGeminiUserParts_Multimodal(t *testing.T) {
	plain := geminiUserParts(llm.Message{Role: llm.RoleUser, Content: "hi"})
	if len(plain) != 1 || plain[0].Text != "hi" || plain[0].InlineData != nil {
		t.Fatalf("无图 user parts 异常: %#v", plain)
	}

	parts := geminiUserParts(llm.Message{
		Role:    llm.RoleUser,
		Content: "看这张图",
		Images:  []llm.ImagePart{{MimeType: "image/jpeg", Data: "aGVsbG8="}},
	})
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if parts[0].Text != "看这张图" {
		t.Fatalf("parts[0].Text = %q", parts[0].Text)
	}
	img := parts[1].InlineData
	if img == nil || img.MimeType != "image/jpeg" || img.Data != "aGVsbG8=" {
		t.Fatalf("inlineData 异常: %#v", img)
	}
}
