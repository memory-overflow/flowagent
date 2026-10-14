// Package skill 提供 Skill（可复用能力包）的统一封装。
//
// 职责：
//   - 把 model.Skill 的 DB 配置解析成运行时 Config
//   - 指令型（prompt）skill：渲染为 system prompt 片段
//   - 可执行型（command）skill：作为工具执行 shell 命令
//   - 内置清单（builtins）加载与展开
//
// 与 MCP 的差异：
//
//	MCP 是「外部工具服务」，通过协议握手 + ListTools + CallTool 交互；
//	Skill 是「本地能力包」，prompt 型直接注入指令，command 型用本地
//	shell 子进程执行（stdin 传输入、stdout 取结果），无需协议握手。
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/model"
)

// DefaultTimeout 单次 command skill 执行的默认超时
const DefaultTimeout = 60 * time.Second

// nameSeparator 工具名命名空间分隔符（与 MCP 保持一致的双下划线）
const nameSeparator = "__"

// toolNamePrefix command skill 工具名的统一前缀
const toolNamePrefix = "skill"

// invalidNameChars LLM function name 不允许的字符
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ============================================================
// Config — 运行时配置
// ============================================================

// Config 一个 Skill 的运行时配置（从 model.Skill 解析而来）。
type Config struct {
	SkillID     string
	Name        string
	Description string
	Type        model.SkillType
	Content     string
	Command     string
	Args        []string
	Env         []string
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
		Type:        s.Type,
		Content:     s.Content,
		Command:     s.Command,
		Args:        parseStringSlice(s.Args),
		Env:         envMapToSlice(parseStringMap(s.Env)),
	}, nil
}

// IsCommand 是否为可执行型（command）skill
func (c *Config) IsCommand() bool {
	return c != nil && c.Type == model.SkillTypeCommand && strings.TrimSpace(c.Command) != ""
}

// ToolName 生成暴露给 LLM 的工具名：skill__<sanitized_name>。
// 前缀统一为 skill，避免与 MCP 工具的 <server>__<tool> 命名空间撞车。
func (c *Config) ToolName() string {
	return BuildToolName(c.Name)
}

// BuildToolName 按 skill 命名空间生成工具名。
func BuildToolName(name string) string {
	tool := sanitizeName(name)
	if tool == "" {
		tool = "skill"
	}
	return toolNamePrefix + nameSeparator + tool
}

// ToolSchema command skill 作为工具时的入参 schema。
// 契约：通过 stdin 接收一段自由文本，输出经 stdout 返回。
func (c *Config) ToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"input": map[string]interface{}{
				"type":        "string",
				"description": "传给该 skill 的输入文本（经 stdin 传入）",
			},
		},
		"required": []string{"input"},
	}
}

// ============================================================
// Execute — 执行 command skill
// ============================================================

// Execute 执行一个 command skill：运行 Command + Args，把 input 通过
// stdin 传入，返回 stdout（去首尾空白）。Env 注入子进程环境。
//
// 返回 error 表示进程启动失败 / 非零退出 / 超时。超时或非零退出时，
// 会把已有 stdout/stderr 一并带进错误信息，避免信息丢失。
func Execute(ctx context.Context, cfg *Config, input string) (string, error) {
	if cfg == nil || !cfg.IsCommand() {
		return "", fmt.Errorf("skill: %v 不是可执行的 command skill", nameOrEmpty(cfg))
	}

	// 安全拦截：执行前检测危险删除命令（rm -rf 等），防止误删系统/关键文件。
	if reason := detectDangerousCommand(input); reason != "" {
		return "", fmt.Errorf("skill %s 执行被安全策略拦截: %s", cfg.Name, reason)
	}

	execCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	// shell 型 skill 按平台选择解释器：
	//   - unix:    Command（如 /bin/sh）+ stdin 传入命令脚本；
	//   - windows: 没有 sh，改用 cmd /c，命令通过参数传入（cmd 不读 stdin）。
	command := cfg.Command
	args := cfg.Args
	stdinInput := input
	useStdin := true
	if isShellCommand(cfg) && runtime.GOOS == "windows" {
		command = "cmd"
		args = append([]string{"/c"}, cfg.Args...)
		args = append(args, input)
		useStdin = false
	}

	cmd := exec.CommandContext(execCtx, command, args...)

	// 环境注入：cfg.Env + 项目 venv。shell skill 子进程 cwd = 项目根，把
	// .venv 的可执行目录提到 PATH 最前（并设 VIRTUAL_ENV），子进程里的
	// python / pip 即指向项目 venv。相比在命令前拼接激活脚本，PATH 注入
	// 跨平台行为一致，也避开了 cmd /c 的引号/换行解析坑。
	env := os.Environ()
	if len(cfg.Env) > 0 {
		env = append(env, cfg.Env...)
	}
	if isShellCommand(cfg) {
		if venvBin := venvBinDirFor(runtime.GOOS, "."); venvBin != "" {
			if abs, aerr := filepath.Abs(venvBin); aerr == nil {
				env = append(env,
					"PATH="+abs+string(os.PathListSeparator)+os.Getenv("PATH"),
					"VIRTUAL_ENV="+filepath.Dir(abs))
			}
		}
	}
	cmd.Env = env

	if useStdin {
		cmd.Stdin = strings.NewReader(stdinInput)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail != "" {
			return "", fmt.Errorf("skill %s 执行失败: %v (输出: %s)", cfg.Name, err, truncate(detail, 500))
		}
		return "", fmt.Errorf("skill %s 执行失败: %v", cfg.Name, err)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// venvBinDirFor 返回项目 venv 的可执行目录（相对 projectRoot；shell skill
// 子进程 cwd = 项目根，传 "." 即可）：unix 为 .venv/bin，windows 为
// .venv\Scripts；目录不存在返回空串。调用方把它注入子进程 PATH（并设
// VIRTUAL_ENV）即可让 python / pip 命中 venv —— 不再往命令行拼接激活脚本，
// 规避 cmd /c 的引号与换行解析坑。可注入 GOOS 便于测试覆盖两个分支。
func venvBinDirFor(goos string, projectRoot string) string {
	if strings.TrimSpace(projectRoot) == "" {
		return ""
	}
	dir := filepath.Join(projectRoot, ".venv", "bin")
	if goos == "windows" {
		dir = filepath.Join(projectRoot, ".venv", "Scripts")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ""
	}
	return dir
}

// isShellCommand 判断 command skill 是否为 shell 型（/bin/sh、sh、bash 等）。
// 只有 shell 型 skill 的 stdin 是待执行的命令脚本，需要注入 venv 激活；
// 其它 command skill（如某个具体 CLI）的 stdin 是业务输入，不应注入。
func isShellCommand(cfg *Config) bool {
	if cfg == nil {
		return false
	}
	switch filepath.Base(cfg.Command) {
	case "sh", "bash", "zsh", "dash":
		return true
	}
	return false
}

// ============================================================
// 安全策略 — 危险命令拦截
// ============================================================

// rmCmdRe 匹配 rm 命令及其参数（到换行 / ; / && / || / | 等分隔符为止），
// 用于在 shell 脚本里定位 rm 调用，做危险删除拦截。
var rmCmdRe = regexp.MustCompile(`(?i)\brm\b\s+([^\n;&|]+)`)

// DetectDangerousCommand 检测 input 中的危险删除命令（导出封装）。
//
// 供 workspace 内置工具的 run_command 复用：执行任意 shell 命令前先做
// 同样的危险删除拦截，避免 agent 误执行 rm -rf / 等破坏性命令。
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

func nameOrEmpty(cfg *Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Name
}

// ============================================================
// 路径绝对化
// ============================================================

// AbsolutizePaths 把 content 里的框架脚本相对路径（skills/ 前缀）替换成绝对路径。
//
// 背景：skill 的 content 里写的脚本路径是相对「项目根」的（如
// skills/pdfkit-py/scripts/pdfkit.py），这是为配合 command 型 shell skill
// （其子进程 cwd = 项目根）设计的。但模型经常改用内置 run_command 落地
// （其 cwd = 任务工作区 workspace/<chat_id>/），此时相对路径 `skills/...`
// 找不到 —— 实测 PDF 用例里 `bash skills/pdfkit-py/scripts/setup.sh` 报
// "No such file or directory"，模型只能退回 strings + pip install 硬解。
//
// 替换成绝对路径后，无论 run_command（cwd=任务工作区）还是 shell skill
// （cwd=项目根）都能正确落地。projectRoot 为项目根绝对路径；为空则不替换。
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

// BuildPromptSection 把一组已启用的 skill 渲染成 system prompt 片段。
//
// 同时覆盖 prompt 型与 command 型：prompt 型注入完整指令正文，
// command 型只注入名称 + 说明 + 调用提示（其执行依赖工具调用，
// 具体入参由 tool schema 约束）。
func BuildPromptSection(skills []*Config) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("【可用技能 (Skills)】\n")
	b.WriteString("以下是当前对话已启用的技能。当用户需求匹配某个技能的适用场景时，请遵循该技能的指令来完成任务。\n")
	b.WriteString("技能及其运行环境（CLI、Python 依赖、虚拟环境）均已由安装脚本预先装好，直接按指令调用即可；不要用命令去检查技能目录、确认依赖或虚拟环境是否安装。\n")

	for _, s := range skills {
		if s == nil {
			continue
		}
		if s.IsCommand() {
			fmt.Fprintf(&b, "\n### 技能: %s（可执行）\n%s\n", s.Name, firstLine(s.Description))
			fmt.Fprintf(&b, "调用方式：使用工具 %s，把待处理文本作为 input 参数传入。\n", s.ToolName())
			if strings.TrimSpace(s.Content) != "" {
				fmt.Fprintf(&b, "%s\n", s.Content)
			}
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ============================================================
// 内部：解析与清洗
// ============================================================

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = invalidNameChars.ReplaceAllString(s, "")
	return strings.Trim(s, "_-")
}

func parseStringSlice(s string) []string {
	if s == "" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil
	}
	return arr
}

func parseStringMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func envMapToSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
