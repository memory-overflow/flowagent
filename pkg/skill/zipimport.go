// Package skill — Skill 包 zip 整体导入
//
// 支持三种 zip 布局：
//
//  1. 本项目 skill.json 单包（根直放或唯一顶层目录）：
//     skill.json（key/name/description/type/content/command/args/env）+ scripts/...
//  2. 社区 SKILL.md 单包：
//     SKILL.md（YAML frontmatter: name/description）+ scripts/...
//  3. 社区 SKILL.md 集合包：
//     根 SKILL.md（目录索引）+ 多个子目录各自 SKILL.md + scripts/run.py
//     → 合并为**单个父 skill**：子能力不单独建记录，而是作为「子能力清单」
//     并入父 skill 的 content，全部文件落地 skills/<key>/ 保持原结构。
//
// 落地规则：脚本/资源解压到 <skills_root>/<key>/ 下；SKILL.md 正文里的
// `${CODEBUDDY_SKILL_DIR}` 映射为 `skills/<key>`（加载时会被绝对化，见 AbsolutizePaths）。
package skill

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/memory-overflow/flowagent/model"
)

// zip 导入的安全上限
const (
	zipMaxSize     = 50 << 20 // 单个 zip 最大 50MB
	zipMaxFiles    = 500      // 最多 500 个条目
	zipMaxFilesize = 20 << 20 // 单个文件解压后最大 20MB
)

// UserSkillsRelDir 用户自定义（zip 导入）skill 相对项目根的落地目录。
//
// zip 导入的都是「用户自己上传」的 skill，统一落到 skills/user/ 下，
// 该目录已被 .gitignore 忽略，不进 git 仓库；预置 skill（skills/3d、
// skills/builtins 等）仍留在 skills/ 下被跟踪。这样用户自定义与预置
// 清晰分层，且上传内容不会污染仓库。
const UserSkillsRelDir = "skills/user"

// zipEntry zip 里的一个普通文件条目（目录已跳过）。name 是规范斜杠路径；
// data 是完整内容 —— 解析阶段一次性读入内存，避免 *zip.File 惰性引用底层
// 句柄（zip 文件关闭后 Open 会报 file already closed）。
type zipEntry struct {
	name string
	data []byte
}

// ImportItem 一条待导入的 skill（可能是单包，也可能是集合包里的一个子 skill）。
type ImportItem struct {
	Key         string
	Name        string
	Description string
	Type        model.SkillType
	Content     string
	Command     string
	Args        []string
	Env         map[string]string
	// Files 待落地文件（相对该 skill 的 scripts 根目录）
	Files []zipEntry
}

// readZipEntries 打开 zip、把所有普通文件读进内存，剥掉唯一顶层目录前缀。
// 返回顶层目录名（无顶层目录时为空）与条目列表。
func readZipEntries(zipPath string) (string, []zipEntry, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return "", nil, fmt.Errorf("skill: open zip: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("skill: stat zip: %w", err)
	}
	if st.Size() > zipMaxSize {
		return "", nil, fmt.Errorf("skill: zip 超过大小上限 %dMB", zipMaxSize>>20)
	}

	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return "", nil, fmt.Errorf("skill: 解析 zip 失败: %w", err)
	}

	var entries []zipEntry
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		if len(entries) >= zipMaxFiles {
			return "", nil, fmt.Errorf("skill: zip 内文件数超过上限 %d", zipMaxFiles)
		}
		if zf.UncompressedSize64 > zipMaxFilesize {
			return "", nil, fmt.Errorf("skill: zip 内单文件 %s 超过大小上限", zf.Name)
		}
		name := strings.TrimPrefix(filepath.ToSlash(zf.Name), "./")
		if name == "" || strings.HasPrefix(name, "__MACOSX/") || strings.HasSuffix(name, "/.DS_Store") || name == ".DS_Store" {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return "", nil, fmt.Errorf("skill: 打开 %s: %w", zf.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return "", nil, fmt.Errorf("skill: 读取 %s: %w", zf.Name, err)
		}
		entries = append(entries, zipEntry{name: name, data: data})
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("skill: zip 内没有文件")
	}

	// 剥唯一顶层目录（所有条目共享同一第一段才剥），返回顶层目录名
	top := topDirOf(entries)
	if top != "" {
		for i := range entries {
			entries[i].name = strings.TrimPrefix(entries[i].name, top)
		}
		top = strings.TrimSuffix(top, "/")
	}
	return top, entries, nil
}

// AnalyzeSkillZip 解析 zip，返回待导入的 skill 列表。
// 三种格式统一归为单条：skill.json 单包 / SKILL.md 单包 / SKILL.md 集合包
// （集合包合并为一个父 skill，子能力清单并入 content）。
func AnalyzeSkillZip(zipPath string) ([]*ImportItem, error) {
	top, entries, err := readZipEntries(zipPath)
	if err != nil {
		return nil, err
	}

	// 有 skill.json → 本项目单包
	if findEntryName(entries, "skill.json") != nil {
		item, err := jsonPackToItem(entries)
		if err != nil {
			return nil, err
		}
		return []*ImportItem{item}, nil
	}

	// 有 SKILL.md → 社区格式（单包或集合包）
	if hasSKILLMD(entries) {
		return skillmdPackToItems(entries, top)
	}

	return nil, fmt.Errorf("skill: zip 内既没有 skill.json 也没有 SKILL.md")
}

// ExtractItemFiles 把 item.Files 落地到 destDir（即 skills/<key>/）。
// 带 zip-slip 防护。返回落地文件数。
func ExtractItemFiles(item *ImportItem, destDir string) (int, error) {
	if item == nil || len(item.Files) == 0 {
		return 0, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, fmt.Errorf("skill: 创建目录 %s: %w", destDir, err)
	}
	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return 0, err
	}
	destAbs = filepath.Clean(destAbs) + string(filepath.Separator)

	n := 0
	for _, e := range item.Files {
		target, err := filepath.Abs(filepath.Join(destDir, filepath.FromSlash(e.name)))
		if err != nil || !strings.HasPrefix(target, destAbs) {
			return n, fmt.Errorf("skill: zip 内含非法路径 %q，已拒绝", e.name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return n, fmt.Errorf("skill: 创建子目录: %w", err)
		}
		if err := os.WriteFile(target, e.data, 0o755); err != nil {
			return n, fmt.Errorf("skill: 写入 %s: %w", target, err)
		}
		n++
	}
	return n, nil
}

// ============================================================
// skill.json 单包
// ============================================================

// jsonPackToItems 把含 skill.json 的包转成单条 ImportItem。
func jsonPackToItem(entries []zipEntry) (*ImportItem, error) {
	var mf BuiltinManifest
	var files []zipEntry
	for _, e := range entries {
		if e.name == "skill.json" {
			if err := json.Unmarshal(e.data, &mf); err != nil {
				return nil, fmt.Errorf("skill: 解析 skill.json: %w", err)
			}
			continue
		}
		files = append(files, e)
	}
	if strings.TrimSpace(mf.Name) == "" {
		return nil, fmt.Errorf("skill: skill.json 缺少 name 字段")
	}
	if mf.Type == 0 {
		mf.Type = model.SkillTypePrompt
	}
	if mf.Key == "" {
		mf.Key = sanitizeSkillKey(mf.Name)
	}
	return &ImportItem{
		Key:         mf.Key,
		Name:        mf.Name,
		Description: mf.Description,
		Type:        mf.Type,
		Content:     mf.Content,
		Command:     mf.Command,
		Args:        mf.Args,
		Env:         mf.Env,
		Files:       files,
	}, nil
}

// ============================================================
// SKILL.md（社区格式）
// ============================================================

func hasSKILLMD(entries []zipEntry) bool {
	return findEntryName(entries, "SKILL.md") != nil || func() bool {
		for _, e := range entries {
			if path.Base(e.name) == "SKILL.md" {
				return true
			}
		}
		return false
	}()
}

// skillmdPackToItems 把 SKILL.md 包（单包或集合包）转成 ImportItem。
// 集合包合并为单条父 skill（子能力清单并入 content），不再拆多条。
func skillmdPackToItems(entries []zipEntry, topName string) ([]*ImportItem, error) {
	u, err := mergeSkillPack(entries, topName)
	if err != nil {
		return nil, err
	}
	return []*ImportItem{{
		Key:         u.Key,
		Name:        u.Name,
		Description: u.Description,
		Type:        model.SkillTypePrompt,
		Content:     u.Content,
		Files:       u.Files,
	}}, nil
}

// ============================================================
// 工具函数
// ============================================================

func findEntryName(entries []zipEntry, name string) *zipEntry {
	for i := range entries {
		if entries[i].name == name {
			return &entries[i]
		}
	}
	return nil
}

// topDirOf 返回所有条目共享的第一段目录分量（含尾斜杠），否则空串。
// 只取首条目的第一段做全量前缀校验——不能用 path.Dir(首条目)：
// 首条目是深层文件时会拿深层目录做前缀，其他条目不匹配 → 误判根直放。
func topDirOf(entries []zipEntry) string {
	if len(entries) == 0 {
		return ""
	}
	first := strings.SplitN(entries[0].name, "/", 2)
	if len(first) < 2 {
		return "" // 首条目在根，无顶层目录
	}
	dir := first[0] + "/"
	for _, e := range entries {
		if !strings.HasPrefix(e.name, dir) {
			return ""
		}
	}
	return dir
}

// sanitizeSkillKey 把 key 清洗成安全的目录名（字母数字-_，其余转下划线）。
func sanitizeSkillKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "skill"
	}
	return out
}
