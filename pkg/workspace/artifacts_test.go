package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAt 在 dir 下写一个文件并把 mtime 设成指定时刻。
func writeAt(t *testing.T, dir, rel, content string, mt time.Time) string {
	t.Helper()
	abs := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chtimes(abs, mt, mt); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
	return abs
}

func relPaths(arts []Artifact) map[string]bool {
	got := make(map[string]bool, len(arts))
	for _, a := range arts {
		got[a.RelPath] = true
	}
	return got
}

// TestScanArtifactsOnlyAfterSince 验证核心判定：只收 mtime 晚于 since 的文件。
// 这是「不用快照」方案能成立的前提，回归了整个 output_files 就会失真。
func TestScanArtifactsOnlyAfterSince(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()

	// 上一轮任务留下的文件（早于本次 start）
	writeAt(t, ws, "old.md", "old", start.Add(-2*time.Hour))
	// 本次任务产出
	writeAt(t, ws, "report.md", "new", start.Add(1*time.Minute))
	writeAt(t, ws, "src/main.go", "package main", start.Add(2*time.Minute))

	arts := ScanArtifacts(ws, start)
	got := relPaths(arts)

	if got["old.md"] {
		t.Errorf("早于 since 的历史文件不应计入产物, got=%v", got)
	}
	if !got["report.md"] || !got["src/main.go"] {
		t.Errorf("本次产出未被识别, got=%v", got)
	}
	if len(arts) != 2 {
		t.Errorf("产物数量应为 2, got %d (%v)", len(arts), got)
	}
}

// TestScanArtifactsSkipsInputsAndNoise 验证 inputs/ 与依赖缓存目录被排除。
// inputs/ 尤其关键：用户附件上传时间紧邻任务创建时间，不排除必被误报成产出。
func TestScanArtifactsSkipsInputsAndNoise(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()
	after := start.Add(time.Minute)

	writeAt(t, ws, "inputs/用户上传.png", "img", after)
	writeAt(t, ws, "node_modules/lib/index.js", "x", after)
	writeAt(t, ws, "__pycache__/mod.cpython-311.pyc", "x", after)
	writeAt(t, ws, ".git/HEAD", "ref", after)
	writeAt(t, ws, ".cache/vision/abc.md", "desc", after)
	writeAt(t, ws, ".scratch/probe_ocr.py", "x", after)
	writeAt(t, ws, ".DS_Store", "x", after)
	writeAt(t, ws, "build/app.o", "x", after)
	// 真正的产物
	writeAt(t, ws, "outputs/article.md", "正文", after)
	// dist/build 目录本身不排除（前端构建产物常是交付物），但 .o 按扩展名排除
	writeAt(t, ws, "build/index.html", "<html>", after)

	arts := ScanArtifacts(ws, start)
	got := relPaths(arts)

	for _, bad := range []string{
		"inputs/用户上传.png",
		"node_modules/lib/index.js",
		"__pycache__/mod.cpython-311.pyc",
		".git/HEAD",
		".cache/vision/abc.md",
		".scratch/probe_ocr.py",
		".DS_Store",
		"build/app.o",
	} {
		if got[bad] {
			t.Errorf("%s 应被排除, got=%v", bad, got)
		}
	}
	if !got["outputs/article.md"] {
		t.Errorf("outputs/ 下的自动落盘产物应计入, got=%v", got)
	}
	if !got["build/index.html"] {
		t.Errorf("build/ 下的非中间产物应计入（前端构建交付物）, got=%v", got)
	}
}

// TestScanArtifactsZeroSince since 为零值时不报任何产物，
// 避免拿不到任务开始时间就把工作区历史文件全量当成本次产出。
func TestScanArtifactsZeroSince(t *testing.T) {
	ws := t.TempDir()
	writeAt(t, ws, "a.md", "x", time.Now())

	if arts := ScanArtifacts(ws, time.Time{}); arts != nil {
		t.Errorf("since 为零值时应返回 nil, got %v", arts)
	}
	if arts := ScanArtifacts("", time.Now()); arts != nil {
		t.Errorf("wsDir 为空时应返回 nil, got %v", arts)
	}
}

// TestScanArtifactsSortedNewestFirst 产物按最新在前排序。
func TestScanArtifactsSortedNewestFirst(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()

	writeAt(t, ws, "first.md", "1", start.Add(1*time.Minute))
	writeAt(t, ws, "second.md", "2", start.Add(5*time.Minute))

	arts := ScanArtifacts(ws, start)
	if len(arts) != 2 {
		t.Fatalf("期望 2 个产物, got %d", len(arts))
	}
	if arts[0].RelPath != "second.md" {
		t.Errorf("最新产物应排最前, got %s", arts[0].RelPath)
	}
}

func TestDetectFileType(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"a.png", FileTypeImage},
		{"a.jpg", FileTypeImage},
		{"a.mp4", FileTypeVideo},
		{"a.md", FileTypeText},
		{"a.json", FileTypeText}, // MIME 主类型是 application/ 但内容是文本
		{"a.go", FileTypeText},
		{"main.py", FileTypeText},
		{"a.zip", FileTypeOther},
		{"a.pdf", FileTypeOther},
		{"a.xlsx", FileTypeOther},
		{"noext", FileTypeOther},
	}
	for _, c := range cases {
		if got := DetectFileType(c.name); got != c.want {
			t.Errorf("DetectFileType(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDetectMimeTypeStripsCharset(t *testing.T) {
	// 标准库对 .html 会返回 "text/html; charset=utf-8"，需去掉参数部分
	if m := DetectMimeType("a.html"); m != "text/html" {
		t.Errorf("DetectMimeType(a.html) = %q, want text/html", m)
	}
	if m := DetectMimeType("a.md"); m != "text/markdown" {
		t.Errorf("DetectMimeType(a.md) = %q, want text/markdown", m)
	}
}
