// Package skill — SKILL.md（CodeBuddy/社区格式）解析与集合包合并
//
// 社区 skill 包用 SKILL.md 而非本项目 skill.json，且常是「集合包」：
// 一个根 SKILL.md（目录索引）+ 多个子目录各自一个 SKILL.md + scripts/run.py。
// 本文件负责：
//   - 解析 SKILL.md 的 YAML frontmatter（name / description）
//   - 把整个包（单包或集合包）合并成一个父 skill 单元：
//     子能力不单独建记录，而是作为「子能力清单」并入父 skill 的 content，
//     全部文件（含各子 SKILL.md）原样落地到 skills/<key>/ 下供模型按需读取。
package skill

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// skillMDFrontmatter SKILL.md 头部的 YAML frontmatter。
type skillMDFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseSkillMD 解析 SKILL.md：拆出 frontmatter 与正文。
func parseSkillMD(content string) (name, description, body string, err error) {
	c := strings.TrimPrefix(content, "\ufeff") // 去 BOM
	c = strings.ReplaceAll(c, "\r\n", "\n")
	trimmed := strings.TrimSpace(c)
	if !strings.HasPrefix(trimmed, "---") {
		return "", "", "", fmt.Errorf("SKILL.md 缺少 YAML frontmatter（--- 开头）")
	}
	rest := trimmed[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", "", fmt.Errorf("SKILL.md frontmatter 未闭合（缺少第二个 ---）")
	}
	fmRaw := rest[:end]
	body = strings.TrimSpace(rest[end+4:])

	var fm skillMDFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return "", "", "", fmt.Errorf("SKILL.md frontmatter 解析失败: %w", err)
	}
	return strings.TrimSpace(fm.Name), strings.TrimSpace(fm.Description), body, nil
}

// skillMDInfo 一个 SKILL.md 的解析结果；dir 为包内目录（根为 "."）。
type skillMDInfo struct {
	dir  string
	name string
	desc string
	body string
}

// skillUnit 合并后的单个 skill 单元。
type skillUnit struct {
	Key         string
	Name        string
	Description string
	Content     string // 父 SKILL.md 正文 + 子能力清单（集合包时）
	// Files 待落地文件（剥顶层目录后的包相对路径），
	// 落地到 skills/<key>/ 下并保持原相对结构（含各子 SKILL.md）。
	Files []zipEntry
}

// mergeSkillPack 把 SKILL.md 包（单包或集合包）合并成单个父 skill 单元。
//
// 用户预期：导入集合包只产生一条 skill 记录（父 skill），子能力不单独建记录。
// 规则：
//   - 身份：根 SKILL.md（目录 "."）提供 name/description/正文；
//     没有根 SKILL.md 的纯子目录集合，用包顶层目录名充当父 skill 名。
//   - Key：优先包顶层目录名（topName），其次根 SKILL.md 的 name，sanitize 后落地。
//   - Content：父正文 + 「子能力清单」小节（每个子 SKILL.md 一条：名字、
//     说明、脚本目录），模型加载父 skill 即知全部子能力及调用位置。
//   - Files：除父 SKILL.md 本身外的全部文件原样落地（子 SKILL.md 保留，
//     供模型按需读取详细用法）。
func mergeSkillPack(entries []zipEntry, topName string) (*skillUnit, error) {
	var root *skillMDInfo
	var children []skillMDInfo
	seen := map[string]bool{}
	var rootEntry *zipEntry
	for i := range entries {
		e := &entries[i]
		if path.Base(e.name) != "SKILL.md" {
			continue
		}
		dir := path.Dir(e.name)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		n, d, b, err := parseSkillMD(string(e.data))
		if err != nil {
			return nil, fmt.Errorf("skill: %s/SKILL.md: %w", dir, err)
		}
		mi := skillMDInfo{dir: dir, name: n, desc: d, body: b}
		if dir == "." || dir == "/" {
			root = &mi
			rootEntry = e
		} else {
			children = append(children, mi)
		}
	}
	if root == nil && len(children) == 0 {
		return nil, fmt.Errorf("skill: zip 内没有 SKILL.md")
	}

	// 父 skill 身份与 key
	name := topName
	desc := ""
	var body string
	if root != nil {
		if root.name != "" {
			name = root.name
		}
		desc = root.desc
		body = root.body
	}
	if name == "" {
		// 无顶层目录、无根 SKILL.md、只有子目录的极端情况：取第一个子名兜底
		if len(children) > 0 {
			name = children[0].name
		}
		if name == "" {
			return nil, fmt.Errorf("skill: 无法确定父 skill 名称（缺少根 SKILL.md 与顶层目录名）")
		}
	}
	keySrc := topName
	if keySrc == "" {
		keySrc = name
	}
	key := sanitizeSkillKey(keySrc)

	content := strings.ReplaceAll(body, "${CODEBUDDY_SKILL_DIR}", UserSkillsRelDir+"/"+key)
	if len(children) > 0 {
		content += childIndexSection(key, children)
	}

	// 落地文件：除父 SKILL.md 外全部保留（含子 SKILL.md 与脚本）
	files := make([]zipEntry, 0, len(entries))
	for _, e := range entries {
		if rootEntry != nil && e.name == rootEntry.name {
			continue
		}
		files = append(files, e)
	}

	return &skillUnit{
		Key:         key,
		Name:        name,
		Description: desc,
		Content:     content,
		Files:       files,
	}, nil
}

// childIndexSection 把子能力清单并入父 skill content：模型加载父 skill 时
// 一次看到全部子能力的名字、说明与脚本目录，无需单独导入子 skill。
func childIndexSection(key string, children []skillMDInfo) string {
	sort.Slice(children, func(i, j int) bool { return children[i].dir < children[j].dir })
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n## 子能力清单（共 %d 项）\n\n", len(children))
	b.WriteString("以下子能力同属本 skill，脚本已随包落地，用 run_command 直接执行对应脚本即可；" +
		"每个子能力目录里的 SKILL.md 含详细用法与参数说明。\n\n")
	for _, c := range children {
		name := c.name
		if name == "" {
			name = path.Base(c.dir)
		}
		fmt.Fprintf(&b, "- **%s** — %s\n  脚本目录：`%s/%s/%s/`\n", name, oneLineDesc(c.desc), UserSkillsRelDir, key, c.dir)
	}
	return b.String()
}

// oneLineDesc 把多行描述压成一行（清单里不换行）。
func oneLineDesc(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
