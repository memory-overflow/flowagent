package skill

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildTestZip(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	p := filepath.Join(t.TempDir(), "skill.zip")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}
	return p
}

const jsonManifest = `{"key":"demo-skill","name":"演示技能","description":"zip 导入测试","type":1,"content":"脚本位于 skills/demo-skill/scripts/run.py。"}`

// TestAnalyzeJsonPack skill.json 单包 → 1 条 item，并落地。
func TestAnalyzeJsonPack(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{
		"skill.json":     jsonManifest,
		"scripts/run.py": "print('hi')",
		"__MACOSX/junk":  "noise",
		".DS_Store":      "noise",
	})
	items, err := AnalyzeSkillZip(zipPath)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("item 数 = %d, want 1", len(items))
	}
	it := items[0]
	if it.Name != "演示技能" || it.Key != "demo-skill" || it.Type != 1 {
		t.Fatalf("item 解析错误: %+v", it)
	}
	if len(it.Files) != 1 || it.Files[0].name != "scripts/run.py" {
		t.Fatalf("files 错误: %+v", it.Files)
	}

	dest := t.TempDir()
	n, err := ExtractItemFiles(it, dest)
	if err != nil || n != 1 {
		t.Fatalf("extract: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "scripts/run.py")); err != nil {
		t.Fatalf("落地文件缺失: %v", err)
	}
}

const skillMD = `---
name: gen-360
description: 调用 gen_360 生成 360 视图。
---

# gen_360
python "${CODEBUDDY_SKILL_DIR}/scripts/run.py" --main_view x
`

// TestAnalyzeSkillMDSingle SKILL.md 单包 → 1 条，且 ${CODEBUDDY_SKILL_DIR} 被映射。
func TestAnalyzeSkillMDSingle(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{
		"gen-360/SKILL.md":       skillMD,
		"gen-360/scripts/run.py": "print('r')",
	})
	items, err := AnalyzeSkillZip(zipPath)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("item 数 = %d, want 1", len(items))
	}
	it := items[0]
	if it.Name != "gen-360" || it.Key != "gen-360" {
		t.Fatalf("name/key 错误: %s / %s", it.Name, it.Key)
	}
	if !strings.Contains(it.Content, "skills/user/gen-360/scripts/run.py") {
		t.Fatalf("${CODEBUDDY_SKILL_DIR} 未映射: %s", it.Content)
	}
	if strings.Contains(it.Content, "CODEBUDDY_SKILL_DIR") {
		t.Fatalf("占位符残留: %s", it.Content)
	}
}

const rootSkillMD = `---
name: 3d动画生成
description: 3D 工具技能目录，查名单读 skill-list.md。
---

# 3d动画生成
完整名单见 skill-list.md。
`

// TestAnalyzeSkillMDCollection 集合包：根 + 多个子 skill 合并为**单条父 skill**，
// 子能力清单并入 content，全部文件（含子 SKILL.md）落地同一目录。
func TestAnalyzeSkillMDCollection(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{
		"3d动画生成/SKILL.md":                       rootSkillMD,
		"3d动画生成/skill-list.md":                  "名单",
		"3d动画生成/demo/gen-360/SKILL.md":       skillMD,
		"3d动画生成/demo/gen-360/scripts/run.py": "print('1')",
		"3d动画生成/demo/gen-270/SKILL.md":        "---\nname: gen-uv\ndescription: 展开 UV。\n---\n\n# gen_uv\n",
		"3d动画生成/demo/gen-270/scripts/run.py":  "print('2')",
	})
	items, err := AnalyzeSkillZip(zipPath)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("item 数 = %d, want 1（集合包应合并为单条父 skill）", len(items))
	}
	it := items[0]
	if it.Name != "3d动画生成" || it.Key != "3d" {
		t.Fatalf("父 skill 错误: key=%q name=%q", it.Key, it.Name)
	}
	// content = 父正文 + 子能力清单
	for _, want := range []string{"子能力清单", "gen-360", "gen-uv", "skills/user/3d/demo/gen-360/", "skills/user/3d/demo/gen-270/"} {
		if !strings.Contains(it.Content, want) {
			t.Fatalf("content 缺少 %q:\n%s", want, it.Content)
		}
	}
	if strings.Contains(it.Content, "CODEBUDDY_SKILL_DIR") {
		t.Fatalf("占位符残留: %s", it.Content)
	}
	// files：除父 SKILL.md 外全部落地（skill-list + 2 子 SKILL.md + 2 run.py = 5）
	if len(it.Files) != 5 {
		t.Fatalf("files 数 = %d, want 5: %+v", len(it.Files), it.Files)
	}
	for _, f := range it.Files {
		if f.name == "SKILL.md" {
			t.Fatalf("父 SKILL.md 不应落地: %+v", it.Files)
		}
	}

	// 落地后目录结构保持包内原样
	dest := t.TempDir()
	n, err := ExtractItemFiles(it, dest)
	if err != nil || n != 5 {
		t.Fatalf("extract: n=%d err=%v", n, err)
	}
	for _, p := range []string{
		"skill-list.md",
		"demo/gen-360/SKILL.md",
		"demo/gen-360/scripts/run.py",
		"demo/gen-270/scripts/run.py",
	} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(p))); err != nil {
			t.Fatalf("落地文件缺失 %s: %v", p, err)
		}
	}
}

// TestAnalyzeSkillMDCollectionNoRoot 无根 SKILL.md 的纯子目录集合：
// 用顶层目录名充当父 skill，子能力清单并入 content。
func TestAnalyzeSkillMDCollectionNoRoot(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{
		"mytools/toolA/SKILL.md":       "---\nname: toolA\ndescription: 工具 A。\n---\n\n# A\n",
		"mytools/toolA/scripts/run.py": "print('a')",
		"mytools/toolB/SKILL.md":       "---\nname: toolB\ndescription: 工具 B。\n---\n\n# B\n",
		"mytools/toolB/scripts/run.py": "print('b')",
	})
	items, err := AnalyzeSkillZip(zipPath)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("item 数 = %d, want 1", len(items))
	}
	it := items[0]
	if it.Name != "mytools" {
		t.Fatalf("父名应为顶层目录名 mytools, got %q", it.Name)
	}
	for _, want := range []string{"toolA", "toolB", "子能力清单"} {
		if !strings.Contains(it.Content, want) {
			t.Fatalf("content 缺少 %q:\n%s", want, it.Content)
		}
	}
}

// TestAnalyzeZipSlipRejected 路径穿越拒收。
func TestAnalyzeZipSlipRejected(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{
		"skill.json":  jsonManifest,
		"../evil.txt": "pwned",
	})
	items, err := AnalyzeSkillZip(zipPath)
	if err != nil {
		t.Fatalf("analyze 不应失败: %v", err)
	}
	if _, err := ExtractItemFiles(items[0], t.TempDir()); err == nil {
		t.Fatalf("zip-slip 未被拒绝")
	}
}

// TestAnalyzeMissingManifest 既无 skill.json 也无 SKILL.md 报错。
func TestAnalyzeMissingManifest(t *testing.T) {
	zipPath := buildTestZip(t, map[string]string{"scripts/run.py": "x"})
	if _, err := AnalyzeSkillZip(zipPath); err == nil {
		t.Fatalf("未报错")
	} else if !strings.Contains(err.Error(), "skill.json") && !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("错误信息未指明清单: %v", err)
	}
}
