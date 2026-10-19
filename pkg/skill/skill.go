// Package skill 提供 Skill（可复用能力包）的统一封装。
//
// 对齐行业通行的 Agent Skills 模式：skill = 一段指令（content）+ 可选的
// 随包脚本，不区分类型、不注册为独立工具。职责：
//   - 把 model.Skill 的 DB 配置解析成运行时 Config
//   - 把已启用的 skill 渲染为 system prompt 片段（BuildPromptSection）
//   - 把指令里的随包脚本相对路径绝对化（AbsolutizePaths）
//   - 内置清单（builtins）加载、zip / SKILL.md 导入
//   - 危险命令检测（DetectDangerousCommand，供内置 run_command 复用）
//
// 模型读完指令后自行拼命令，通过内置 run_command 执行脚本 —— 执行与
// 环境注入（venv PATH、超时、进程组、输出解码）统一在 pkg/workspace。
//
// 与 MCP 的差异：MCP 是「外部工具服务」，通过协议握手 + ListTools +
// CallTool 交互，有结构化 schema；Skill 是「本地知识 + 脚本」，只进 prompt。
package skill

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/memory-overflow/flowagent/model"
)

// ============================================================
// Config — 运行时配置
// ============================================================

// Config 一个 Skill 的运行时配置（从 model.Skill 解析而来）。
type Config struct {
	SkillID     string
	Name        string
	Description string
	Content     string
}

// ConfigFromModel 把 DB 里的 model.Skill 解析成运行时配置。
func ConfigFromModel(s *model.Skill) (*Config, error) {
	if s == nil {
		return nil, fmt.Errorf("skill: nil skill model")
	}
	return &Config{
		SkillID:     s.ID,
		Name:        s.Name,
		Description: s.Description,
		Content:     s.Content,
	}, nil
}

// ============================================================
// 安全策略 — 危险命令拦截
// ============================================================

// rmCmdRe 匹配 rm 命令及其参数（到换行 / ; / && / || / | 等分隔符为止），
// 用于在 shell 脚本里定位 rm 调用，做危险删除拦截。
var rmCmdRe = regexp.MustCompile(`(?i)\brm\b\s+([^\n;&|]+)`)

// DetectDangerousCommand 检测 input 中的危险删除命令（导出封装）。
//
// 供 workspace 内置工具的 run_command 使用：执行任意 shell 命令前先做
// 危险删除拦截，避免 agent 误执行 rm -rf / 等破坏性命令。
func DetectDangerousCommand(input string) string {
	return detectDangerousCommand(input)
}

// detectDangerousCommand 检测 input 中的危险删除命令。
// 命中返回拦截原因，否则返回空串。当前拦截两类：
//  1. rm 递归强制删除（rm -rf / -fr / -Rf / -r -f / --recursive --force 等变体）
//  2. rm 删除系统根 / 家 / 关键目录等危险路径
func detectDangerousCommand(input string) string {
	for _, m := range rmCmdRe.FindAllStringSubmatch(input, -1) {
		if len(m) < 2 {
			continue
		}
		args := m[1]
		if rmHasRecursiveForce(args) {
			return fmt.Sprintf("检测到 rm 递归强制删除（%s），防止误删系统/关键文件", strings.TrimSpace(m[0]))
		}
		if rmTargetsDangerousPath(args) {
			return fmt.Sprintf("检测到 rm 删除危险路径（%s），防止误删系统/关键文件", strings.TrimSpace(m[0]))
		}
	}
	return ""
}

// rmHasRecursiveForce 判断 rm 参数里是否同时出现递归(r/R/--recursive)与强制(f/F/--force)标志。
func rmHasRecursiveForce(args string) bool {
	hasRecursive, hasForce := false, false
	for _, tok := range strings.Fields(args) {
		if tok == "--recursive" {
			hasRecursive = true
			continue
		}
		if tok == "--force" {
			hasForce = true
			continue
		}
		if strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "--") {
			for _, ch := range tok[1:] {
				switch ch {
				case 'r', 'R':
					hasRecursive = true
				case 'f', 'F':
					hasForce = true
				}
			}
		}
	}
	return hasRecursive && hasForce
}

// dangerousPaths 危险删除目标路径前缀（防止误删系统/关键目录）。
var dangerousPaths = []string{
	"/", "/*", "~", "$HOME", "~/",
	"/home", "/root", "/etc", "/usr", "/var",
	"/bin", "/sbin", "/lib", "/boot", "/System",
	"/Applications", "/opt", "/dev", "/proc", "/sys",
}

// rmTargetsDangerousPath 判断 rm 参数里是否包含危险路径目标。
func rmTargetsDangerousPath(args string) bool {
	for _, tok := range strings.Fields(args) {
		tok = strings.Trim(tok, `"'=`)
		for _, p := range dangerousPaths {
			if tok == p || strings.HasPrefix(tok, p+"/") {
				return true
			}
		}
	}
	return false
}

// ============================================================
// 路径绝对化
// ============================================================

// AbsolutizePaths 把 content 里的框架脚本相对路径（skills/ 前缀）替换成绝对路径。
//
// 背景：skill 的 content 里写的脚本路径是相对「项目根」的（如
// skills/pdfkit-py/scripts/pdfkit.py），而模型通过内置 run_command 执行，
// 其 cwd = 任务工作区 workspace/<chat_id>/，相对路径 `skills/...` 找不到 ——
// 实测 PDF 用例里 `bash skills/pdfkit-py/scripts/setup.sh` 报
// "No such file or directory"，模型只能退回 strings + pip install 硬解。
//
// 替换成绝对路径后与 cwd 无关都能正确落地。projectRoot 为项目根绝对路径；
// 为空则不替换。
//
// 只替换 `skills/` 这个带尾斜杠的前缀（路径形式），不误伤中文描述里的
// 裸 "skills" 字样。
func AbsolutizePaths(content, projectRoot string) string {
	if content == "" || projectRoot == "" {
		return content
	}
	root := filepath.Clean(projectRoot)
	// 用系统分隔符拼前缀：之前硬编码 "/" 在 Windows 上会生成
	// `C:\...\flowagent/skills/` 这种混合分隔符路径。模型照抄进命令后
	// 看着像「模型自己拼装错了」，实则是这里拼错 —— 统一走 filepath.Join，
	// Windows 下得到 `C:\...\flowagent\skills\`（干净的反斜杠前缀）。
	prefix := filepath.Join(root, "skills") + string(filepath.Separator)
	return strings.ReplaceAll(content, "skills/", prefix)
}

// ============================================================
// Prompt 渲染 — 指令注入
// ============================================================

// BuildPromptSection 把一组已启用的 skill 渲染成 system prompt 片段：
// 每个 skill 注入名称 + 适用场景 + 完整指令正文。
func BuildPromptSection(skills []*Config) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【可用技能 (Skills)】\n")
	b.WriteString("以下是当前对话已启用的技能。当用户需求匹配某个技能的适用场景时，请遵循该技能的指令来完成任务。\n")
	b.WriteString("技能里的脚本与命令一律通过内置 run_command 工具执行。\n")
	b.WriteString("技能及其运行环境（CLI、Python 依赖、虚拟环境）均已由安装脚本预先装好，直接按指令调用即可；不要用命令去检查技能目录、确认依赖或虚拟环境是否安装。\n")

	for _, s := range skills {
		if s == nil {
			continue
		}
		fmt.Fprintf(&b, "\n### 技能: %s\n", s.Name)
		if strings.TrimSpace(s.Description) != "" {
			fmt.Fprintf(&b, "适用场景：%s\n", s.Description)
		}
		if strings.TrimSpace(s.Content) != "" {
			fmt.Fprintf(&b, "%s\n", s.Content)
		}
	}
	return b.String()
}
