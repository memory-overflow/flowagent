package handler

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeUploadNameBlocksTraversal 上传文件名必须被清洗成纯 basename。
// 这是 inputs/ 目录的第一道防线（第二道是 Manager.Resolve）。
func TestSanitizeUploadNameBlocksTraversal(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"report.md", "report.md"},
		{"季度财报.xlsx", "季度财报.xlsx"},
		// 目录穿越：只保留 basename
		{"../../etc/passwd", "passwd"},
		{"/etc/shadow", "shadow"},
		{"dir/sub/a.txt", "a.txt"},
		// Windows 写法（unix 下 filepath.Base 不识别反斜杠，需单独处理）
		{`C:\Users\me\a.docx`, "a.docx"},
		{`..\..\win.ini`, "win.ini"},
		// 隐藏文件：去掉前导点，避免产物扫描把它跳过
		{".env", "env"},
		{"...hidden", "hidden"},
	}
	for _, c := range cases {
		got := sanitizeUploadName(c.raw)
		if got != c.want {
			t.Errorf("sanitizeUploadName(%q) = %q, want %q", c.raw, got, c.want)
		}
		// 无论输入什么，结果都不能含路径分隔符或 ..
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeUploadName(%q) = %q 仍含路径分隔符", c.raw, got)
		}
		if got != filepath.Base(got) {
			t.Errorf("sanitizeUploadName(%q) = %q 不是纯 basename", c.raw, got)
		}
	}
}

// TestSanitizeUploadNameEmptyFallback 空名/纯点名要有兜底，不能返回空串。
func TestSanitizeUploadNameEmptyFallback(t *testing.T) {
	for _, raw := range []string{"", "   ", ".", "..", "..."} {
		got := sanitizeUploadName(raw)
		if got == "" {
			t.Errorf("sanitizeUploadName(%q) 返回空串", raw)
		}
		if !strings.HasPrefix(got, "upload-") && (got == "." || got == "..") {
			t.Errorf("sanitizeUploadName(%q) = %q 仍是危险名", raw, got)
		}
	}
}

// TestSanitizeUploadNameTruncatesLongName 超长名要截断但保留扩展名。
func TestSanitizeUploadNameTruncatesLongName(t *testing.T) {
	long := strings.Repeat("长", 300) + ".md"
	got := sanitizeUploadName(long)
	if r := []rune(got); len(r) > 120 {
		t.Errorf("超长名未截断: %d runes", len(r))
	}
	if filepath.Ext(got) != ".md" {
		t.Errorf("截断后丢了扩展名: %q", got)
	}
}

// TestBuildDownloadURLEscapes 下载链接必须对中文路径与特殊字符做转义。
func TestBuildDownloadURLEscapes(t *testing.T) {
	u := buildDownloadURL("chat-1", "outputs/季度 报告.md")
	if strings.Contains(u, " ") {
		t.Errorf("下载链接未转义空格: %s", u)
	}
	if !strings.HasPrefix(u, "/api/v1/files/download?") {
		t.Errorf("下载链接前缀不对: %s", u)
	}
	for _, want := range []string{"chat_id=chat-1", "path="} {
		if !strings.Contains(u, want) {
			t.Errorf("下载链接缺少 %s: %s", want, u)
		}
	}
}

// TestSanitizeHeaderName 去掉会破坏 HTTP header 的字符。
func TestSanitizeHeaderName(t *testing.T) {
	got := sanitizeHeaderName("a\"b\r\nc.md")
	for _, bad := range []string{`"`, "\r", "\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeHeaderName 未去掉 %q: %q", bad, got)
		}
	}
}
