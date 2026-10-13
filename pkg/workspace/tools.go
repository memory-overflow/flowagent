// Package workspace — 内置 native 工具集。
//
// 提供 8 个默认对全部对话可用的本地落地工具（Go 原生实现）：
//
//	write_file / read_file / list_dir / create_dir / file_exists / run_command /
//	get_current_time / deep_search
//
// 单页联网抓取（web_fetch）不在此列 —— 它走 MCP（mcp-server-fetch），以获得
// HTML→Markdown 结构化、charset 转码、robots 合规等更完整的抓取效果。
// deep_search 则是对「联网搜索」的框架级封装：它不自己抓网页，而是把检索目标
// 交给一个搜索子任务（由 actuator 注入的 DeepSearchRunner 回调执行），
// 让子任务内部跑「模型 + fetch」多轮循环，最后汇总成一份结论返回主对话。
//
// 设计原则：
//   - 文件工具只接受相对路径，统一经 Manager.Resolve 校验，杜绝越界
//   - run_command 执行前复用 skill.DetectDangerousCommand 做危险删除拦截
//   - read_file 返回内容做大小截断，避免撑爆 LLM 上下文
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/skill"
)

// DefaultTimeout 内置工具单次调用的默认超时。
// 文件操作秒级完成，run_command / web_fetch 给到 60s 兜底。
const DefaultTimeout = 60 * time.Second

// maxReadBytes read_file 返回的最大字节数（约 200KB），防止超大文件撑爆上下文。
const maxReadBytes = 200 * 1024

// maxListEntries list_dir 返回的最大条目数。
const maxListEntries = 500

// maxCommandTimeout run_command 允许的最长超时（模型可显式指定，但不能超过它）。
//
// 与执行期工具调用阶段的外层看门狗 schedule.toolCallTimeout（15 分钟）对齐：
// 再长就会被看门狗连任务一起判失败，工具自己返回超时错误反而更有信息量
// （能带上已产出的输出）。因此这里就是模型能支配的全部时间预算。
const maxCommandTimeout = 15 * time.Minute

// maxCommandOutputBytes run_command 单个输出流（stdout/stderr）最多保留的字节数。
// read_file 限 200KB，但命令输出常需更大些才能定位问题；这里设 64KB，
// 与落库层 maxToolOutputStoreChars(64KB) 对齐，确保源头就不会撑爆内存与 DB。
const maxCommandOutputBytes = 64 * 1024

// ============================================================
// Context / ToolDef — 工具上下文与定义
// ============================================================

// Context 一次 native 工具调用的上下文。
type Context struct {
	// WorkspaceDir 当前任务的工作区绝对目录（已按任务 key 解析）。
	// 空表示当前任务没有工作区，文件工具会拒绝执行。
	WorkspaceDir string
	// ProjectRoot 项目根绝对路径（= 工作区根的上一级）。
	// run_command 执行 shell 命令前据此激活项目级 venv（.venv），
	// 让 python / python3 / pip 指向项目 venv 而非本机环境。空则不激活。
	ProjectRoot string
	// ArtifactDir 本任务的产出目录（相对工作区，如 "outputs/wf-ac049a2f/n1_search"）。
	//
	// 仅子任务设置（见 model.TaskInputData.ArtifactDir）。write_file 收到
	// 「纯文件名」时把它落在该目录下，于是产物归属变成按目录这件确定的事，
	// 不再依赖 mtime 判定 —— 工作区是对话级的，并行节点的执行窗口互相重叠，
	// mtime 判定会把同一时刻其它节点写的文件一并算进来。
	//
	// 空表示本任务没有独立产出目录（顶层对话任务），write_file 按工作区根解析。
	ArtifactDir string
	// VisionAnalyzer 图片理解回调。由上层（tool_call actuator）注入：当模型支持
	// 多模态时，analyze_image 工具据此调用当前模型生成图片描述。
	// 为 nil 表示当前模型不支持图片理解，analyze_image 会直接报错。
	VisionAnalyzer VisionAnalyzerFunc
	// DeepSearchRunner 深度搜索回调。由上层（tool_call actuator）注入：deep_search
	// 工具执行时，据此创建一个专门的搜索子任务并同步等待其完成、返回汇总结论。
	// 为 nil 表示未接入搜索执行器，deep_search 会直接报错。
	DeepSearchRunner DeepSearchRunnerFunc
	// PageFetcher 网页正文抓取回调。由上层（tool_call actuator）注入：web_search
	// 在 deep_search 子任务里据此自动抓取前若干条结果的正文，省掉「模型先读
	// 摘要、下一轮再逐个 fetch」的往返。
	//
	// 为 nil 表示当前没有可用的抓取通道（如 MCP fetch 未加载），web_search
	// 只返回摘要，行为与之前一致。
	PageFetcher PageFetcherFunc
	// ContextRecaller 历史取回回调。由上层（tool_call actuator）注入：
	// recall_context 工具据此按 turn_id / task_id 把被压缩掉的历史原文取回。
	// 为 nil 表示未接入取回执行器，recall_context 会直接报错。
	ContextRecaller ContextRecallerFunc
}

// ContextRecallRequest 一次历史取回请求。
//
// TurnID 与 TaskID 互斥、必须恰好给一个：前者取回某一轮的原始记录，
// 后者取回某个任务的整段交互。Query 仅在 TaskID 模式且素材超预算时生效
// （执行器据它做按需提取）。
type ContextRecallRequest struct {
	TurnID int64
	TaskID string
	Query  string
}

// ContextRecallerFunc 历史取回回调签名。
type ContextRecallerFunc func(ctx context.Context, req ContextRecallRequest) (string, error)

// PageFetcherFunc 网页正文抓取回调签名。
//
// url 为待抓取的页面地址；maxChars 为期望的正文字符上限，实现方应按此截断
// （调用方也会兜底再截一次）。返回页面正文文本。
type PageFetcherFunc func(ctx context.Context, url string, maxChars int) (string, error)

// VisionAnalyzerFunc 图片理解回调签名。
//
// imageData 为图片原始字节；mimeType 为图片 MIME 类型（image/png 等）；
// question 为用户希望重点回答的问题，空串表示返回图片的通用结构化描述。
type VisionAnalyzerFunc func(ctx context.Context, imageData []byte, mimeType string, question string) (string, error)

// DeepSearchRunnerFunc 深度搜索回调签名。
//
// query 为检索目标；maxRounds 为最大检索轮次，<=0 表示由执行器取默认值
// （model.DefaultDeepSearchMaxRound）。返回值为子任务汇总后的最终结论。
type DeepSearchRunnerFunc func(ctx context.Context, query string, maxRounds int) (string, error)

// ToolDef 一个内置工具的完整定义：暴露给 LLM 的名字、说明、入参 schema
// 与执行函数。
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	// Timeout 单次调用的超时上限。<=0 表示用默认值 DefaultTimeout。
	// deep_search 这类内部要跑一个完整子任务的「重活」工具需要更长超时，
	// 普通文件/命令工具保持默认 60s。
	Timeout time.Duration
	// Label 来源展示名（可选）。为空时上层按 builtin 展示；非空时覆盖 turn 记录
	// 里的 server_name，用于 web_search 这类需要在调试页显示具体检索引擎
	// （百度 / 博查 / Serper 等）的工具。
	Label string
	// Handler 执行工具，返回给 LLM 的文本结果。error 表示执行失败
	// （会被上层回喂给 LLM，由它决定重试或换工具）。
	Handler func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error)
}

// BuiltinTools 返回全部内置 native 工具（顺序稳定）。
func BuiltinTools() []*ToolDef {
	return []*ToolDef{
		writeFileTool(),
		readFileTool(),
		listDirTool(),
		createDirTool(),
		fileExistsTool(),
		runCommandTool(),
		getCurrentTimeTool(),
		deepSearchTool(),
		recallContextTool(),
	}
}

// ============================================================
// 参数解析辅助
// ============================================================

// strArg 从工具入参里取字符串参数。
func strArg(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// objectSchema 构造一个 object 型 JSON Schema。
func objectSchema(props map[string]interface{}, required []string) map[string]interface{} {
	schema := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// strProp 构造一个 string 型属性定义。
func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "string",
		"description": desc,
	}
}

// ============================================================
// 1. write_file
// ============================================================

func writeFileTool() *ToolDef {
	return &ToolDef{
		Name:        "write_file",
		Description: "在工作区内创建或覆盖一个文本文件。path 为相对工作区的路径（如 src/main.go）；若只给文件名（如 report.md），会自动落到本任务的产出目录下。content 为完整文件内容。",
		InputSchema: objectSchema(map[string]interface{}{
			"path":    strProp("文件路径，相对工作区（禁止绝对路径与 .. 越界）。只给文件名时落在当前任务的产出目录下"),
			"content": strProp("要写入的完整文件内容"),
		}, []string{"path", "content"}),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			path := strArg(args, "path")
			content := strArg(args, "content")
			if strings.TrimSpace(path) == "" {
				return "", fmt.Errorf("write_file 缺少 path 参数")
			}
			path = resolveArtifactPath(wc, path)
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			if err := ensureDir(filepath.Dir(abs)); err != nil {
				return "", fmt.Errorf("创建父目录失败: %w", err)
			}
			if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
				return "", fmt.Errorf("写入文件失败: %w", err)
			}
			return fmt.Sprintf("已写入文件 %s（%d 字节）", path, len(content)), nil
		},
	}
}

// resolveArtifactPath 把模型给的相对路径映射到本任务的产出目录。
//
// 只处理「纯文件名」（不含目录分隔符、不以 . 开头）：模型写了显式目录路径时
// 一律尊重原样 —— 那是它的明确意图（如 src/main.go、outputs/foo.md），
// 强行改到产出目录会破坏项目结构。
//
// 纯文件名是「我不知道该放哪」的信号，落到本任务的产出目录是合理默认，
// 且让产物归属确定（见 Context.ArtifactDir）。
func resolveArtifactPath(wc *Context, path string) string {
	if wc == nil {
		return path
	}
	dir := strings.TrimSpace(wc.ArtifactDir)
	if dir == "" {
		return path
	}
	p := strings.TrimSpace(path)
	if p == "" || strings.HasPrefix(p, ".") || strings.ContainsAny(p, "/\\") {
		return path
	}
	return strings.Trim(dir, "/") + "/" + p
}

// ============================================================
// 2. read_file
// ============================================================

func readFileTool() *ToolDef {
	return &ToolDef{
		Name:        "read_file",
		Description: "读取工作区内的文本文件全文。path 为相对工作区的路径。超过 200KB 只返回前 200KB。",
		InputSchema: objectSchema(map[string]interface{}{
			"path": strProp("文件路径，相对工作区"),
		}, []string{"path"}),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			path := strArg(args, "path")
			if strings.TrimSpace(path) == "" {
				return "", fmt.Errorf("read_file 缺少 path 参数")
			}
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			f, err := os.Open(abs)
			if err != nil {
				return "", fmt.Errorf("读取文件失败: %w", err)
			}
			defer f.Close()

			data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
			if err != nil {
				return "", fmt.Errorf("读取文件失败: %w", err)
			}
			truncated := len(data) > maxReadBytes
			if truncated {
				data = data[:maxReadBytes]
			}
			out := string(data)
			if truncated {
				out += "\n\n[内容过长，已截断到前 200KB]"
			}
			return out, nil
		},
	}
}

// ============================================================
// 3. list_dir
// ============================================================

func listDirTool() *ToolDef {
	return &ToolDef{
		Name:        "list_dir",
		Description: "列出工作区内指定目录下的条目（名称、类型、大小）。path 为相对工作区的目录路径。",
		InputSchema: objectSchema(map[string]interface{}{
			"path": strProp("目录路径，相对工作区（留空或 . 表示工作区根）"),
		}, nil),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			path := strArg(args, "path")
			if strings.TrimSpace(path) == "" {
				path = "."
			}
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(abs)
			if err != nil {
				return "", fmt.Errorf("列出目录失败: %w", err)
			}

			type item struct {
				name  string
				isDir bool
				size  int64
			}
			var items []item
			for _, e := range entries {
				info, ierr := e.Info()
				if ierr != nil {
					continue
				}
				items = append(items, item{name: e.Name(), isDir: e.IsDir(), size: info.Size()})
				if len(items) >= maxListEntries {
					break
				}
			}
			sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

			var b strings.Builder
			fmt.Fprintf(&b, "目录 %s 下共 %d 个条目：\n", path, len(items))
			for _, it := range items {
				kind := "文件"
				size := fmt.Sprintf("%d B", it.size)
				if it.isDir {
					kind = "目录"
					size = "-"
				}
				fmt.Fprintf(&b, "  [%s] %s (%s)\n", kind, it.name, size)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// ============================================================
// 4. create_dir
// ============================================================

func createDirTool() *ToolDef {
	return &ToolDef{
		Name:        "create_dir",
		Description: "在工作区内创建目录（自动创建父目录）。path 为相对工作区的目录路径。",
		InputSchema: objectSchema(map[string]interface{}{
			"path": strProp("目录路径，相对工作区"),
		}, []string{"path"}),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			path := strArg(args, "path")
			if strings.TrimSpace(path) == "" {
				return "", fmt.Errorf("create_dir 缺少 path 参数")
			}
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(abs, 0o755); err != nil {
				return "", fmt.Errorf("创建目录失败: %w", err)
			}
			return fmt.Sprintf("已创建目录 %s", path), nil
		},
	}
}

// ============================================================
// 5. file_exists
// ============================================================

func fileExistsTool() *ToolDef {
	return &ToolDef{
		Name:        "file_exists",
		Description: "检查工作区内某路径（文件或目录）是否存在。返回 true/false。",
		InputSchema: objectSchema(map[string]interface{}{
			"path": strProp("路径，相对工作区"),
		}, []string{"path"}),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			path := strArg(args, "path")
			if strings.TrimSpace(path) == "" {
				return "", fmt.Errorf("file_exists 缺少 path 参数")
			}
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			if _, err := os.Stat(abs); err != nil {
				if os.IsNotExist(err) {
					return "false", nil
				}
				return "", fmt.Errorf("检查路径失败: %w", err)
			}
			return "true", nil
		},
	}
}

// ============================================================
// 6. run_command
// ============================================================

// resolveCommandTimeout 解析模型指定的命令超时，落到 [DefaultTimeout, maxCommandTimeout]。
//
// 模型比框架更清楚这条命令该跑多久：grep 一个目录是秒级，全盘 find 或全量编译
// 是分钟级。把决定权给它，比统一 60s 一刀切（长命令必被截断）或统一放宽
// （短命令卡死时干等）都合理；上限仍然卡死，防止它瞎填一个 86400 把任务拖到看门狗。
//
// 纯函数（不碰进程 / 不落库），便于单测。
func resolveCommandTimeout(args map[string]interface{}) time.Duration {
	timeout := DefaultTimeout
	if v, ok := args["timeout_seconds"]; ok {
		if n, ok2 := intArg(v); ok2 && n > 0 {
			timeout = time.Duration(n) * time.Second
			if timeout > maxCommandTimeout {
				timeout = maxCommandTimeout
			}
		}
	}
	return timeout
}

// shellCommand 返回当前平台用于执行「一段 shell 命令」的解释器与 -c 等价标志。
//
// Windows 用 cmd /c（无 sh）；linux/darwin 等用 sh -c。模型生成的命令文本
// 必须与所选 shell 匹配 —— 平台信息通过 ShellHint 注入 system prompt（见下）。
func shellCommand() (name, flag string) {
	if runtime.GOOS == "windows" {
		return "cmd", "/c"
	}
	return "sh", "-c"
}

// ShellHint 返回注入 system prompt 的「当前运行环境」提示，让模型生成对应
// 平台的命令（cmd 与 sh 的语法差异很大：dir/ls、type/cat、copy/cp、del/rm）。
// 由 actuator 层在执行期与规划期分别注入。
func ShellHint() string {
	switch runtime.GOOS {
	case "windows":
		return "当前运行环境是 Windows，请在 run_command / shell 等工具里使用 Windows cmd 兼容的命令（例如 dir、type、copy、del、mkdir，而不是 ls、cat、cp、rm、chmod），路径用反斜杠 \\。"
	case "darwin":
		return "当前运行环境是 macOS，请使用 Unix shell 兼容的命令（bash/zsh，例如 ls、cat、cp、rm）。"
	default:
		return "当前运行环境是 Linux，请使用 Unix shell 兼容的命令（bash/sh，例如 ls、cat、cp、rm）。"
	}
}

// venvActivateFor 生成在 shell 命令前注入的 venv 激活片段。
//
// projectRoot 为项目根绝对路径，据此定位 venv 的 activate 脚本：
//   - unix:    <projectRoot>/.venv/bin/activate，用 [ -f ] && source 激活；
//   - windows: <projectRoot>/.venv/Scripts/activate.bat，用 if exist call 激活。
//
// 返回空串表示 projectRoot 为空（不激活）。
func venvActivateFor(projectRoot string) string {
	if strings.TrimSpace(projectRoot) == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		activate := filepath.Join(projectRoot, ".venv", "Scripts", "activate.bat")
		return "if exist \"" + activate + "\" call \"" + activate + "\"\n"
	}
	activate := filepath.Join(projectRoot, ".venv", "bin", "activate")
	return "[ -f \"" + activate + "\" ] && . \"" + activate + "\"\n"
}

func runCommandTool() *ToolDef {
	shellName, shellFlag := shellCommand()
	return &ToolDef{
		Name: "run_command",
		Description: fmt.Sprintf("在工作区目录内执行一条 shell 命令（%s %s），返回 stdout。适合编译、运行脚本、git 等。危险删除命令会被拦截。"+
			"可选 timeout_seconds 指定超时（默认 %d 秒，上限 %d 秒）：预计耗时较长的命令"+
			"（全盘搜索、全量编译、大批量文件处理）请主动放宽，否则会被默认超时打断。",
			shellName, shellFlag, int(DefaultTimeout.Seconds()), int(maxCommandTimeout.Seconds())),
		InputSchema: objectSchema(map[string]interface{}{
			"cmd": strProp("要执行的 shell 命令，工作目录为当前任务的工作区"),
			"timeout_seconds": map[string]interface{}{
				"type": "integer",
				"description": fmt.Sprintf("（可选）命令超时秒数，默认 %d，上限 %d",
					int(DefaultTimeout.Seconds()), int(maxCommandTimeout.Seconds())),
			},
		}, []string{"cmd"}),
		// 外层（runToolEntry）给的时间必须覆盖模型能指定的上限，否则模型
		// 要的 900s 会在外层先被 60s 截断，参数形同虚设。真正的执行超时由
		// Handler 内部的 execCtx 按 timeout_seconds 控制。
		Timeout: maxCommandTimeout,
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			cmd := strArg(args, "cmd")
			if strings.TrimSpace(cmd) == "" {
				return "", fmt.Errorf("run_command 缺少 cmd 参数")
			}
			if wc.WorkspaceDir == "" {
				return "", fmt.Errorf("当前任务没有工作区目录，无法执行命令")
			}
			// 危险删除拦截（复用 skill 的检测逻辑）
			if reason := skill.DetectDangerousCommand(cmd); reason != "" {
				return "", fmt.Errorf("命令被安全策略拦截: %s", reason)
			}
			// 激活项目级 venv：run_command 的 cwd 是任务工作区，需用项目根
			// 绝对路径定位 .venv；激活后 python/python3/pip 指向项目 venv。
			if prefix := venvActivateFor(wc.ProjectRoot); prefix != "" {
				cmd = prefix + cmd
			}
			if err := ensureDir(wc.WorkspaceDir); err != nil {
				return "", fmt.Errorf("创建工作区目录失败: %w", err)
			}

			timeout := resolveCommandTimeout(args)
			execCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			// 不用 exec.CommandContext：它只 kill 直接子进程，而 shell fork 出的
			// 孙进程（管道里的 find/head、后台任务）会活下来并继续持有 stdout
			// pipe 写端，导致下面的 Wait 一直等不到 EOF、超时形同虚设
			// （实测 ctx=2s 的 `sh -c "sleep 15 & wait"` 要 15s 才返回）。
			// 改为自己起进程组 + select 等超时 + kill 整组。
			shellName, shellFlag := shellCommand()
			ec := exec.Command(shellName, shellFlag, cmd)
			ec.Dir = wc.WorkspaceDir
			stdout := newLimitedBuffer(maxCommandOutputBytes)
			stderr := newLimitedBuffer(maxCommandOutputBytes)
			ec.Stdout = stdout
			ec.Stderr = stderr
			setProcessGroup(ec)

			if err := ec.Start(); err != nil {
				return "", fmt.Errorf("命令启动失败: %w", err)
			}
			done := make(chan error, 1)
			go func() { done <- ec.Wait() }()

			var runErr error
			select {
			case runErr = <-done:
				// 正常结束（成功或命令自身非零退出）
			case <-execCtx.Done():
				// 超时 / 上层取消：杀整组，孙进程一起死 → pipe 关闭 → Wait 返回。
				killProcessGroup(ec)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					// 兜底：极端情况下（如某个孙进程无视 SIGKILL）也不能把
					// 整个工具调用永久挂住，那会一路拖到任务看门狗。
				}
				detail := commandOutputDetail(stdout, stderr)
				if detail != "" {
					detail = "，终止前输出: " + truncateRunes(detail, 2000)
				}
				return "", fmt.Errorf("命令执行超过 %v 被强制终止（已终止命令及其子进程）%s",
					timeout, detail)
			}

			if runErr != nil {
				detail := commandOutputDetail(stdout, stderr)
				if detail != "" {
					return "", fmt.Errorf("命令执行失败: %v (输出: %s)", runErr, truncateRunes(detail, 2000))
				}
				return "", fmt.Errorf("命令执行失败: %v", runErr)
			}

			out := strings.TrimSpace(stdout.String())
			if stdout.truncated {
				out += "\n…(stdout 输出过大，已截断)…"
			}
			if out == "" {
				out = "(命令执行成功，无 stdout 输出)"
			}
			return out, nil
		},
	}
}

// commandOutputDetail 取命令已产出的输出用于报错：优先 stderr，回退 stdout。
// 超时场景尤其需要它 —— 模型能据此判断命令是「扫到一半」还是「一步没动」。
func commandOutputDetail(stdout, stderr *limitedBuffer) string {
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = strings.TrimSpace(stdout.String())
	}
	if detail != "" && (stderr.truncated || stdout.truncated) {
		detail += "\n…(输出过大，已截断)…"
	}
	return detail
}

// ============================================================
// 7. get_current_time
// ============================================================

func getCurrentTimeTool() *ToolDef {
	return &ToolDef{
		Name:        "get_current_time",
		Description: "获取当前时间（本地时区 + UTC），用于文件名时间戳、日期相关任务。",
		InputSchema: objectSchema(map[string]interface{}{}, nil),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			now := time.Now()
			return fmt.Sprintf(
				"本地时间: %s\nUTC 时间: %s\nUnix 时间戳: %d\n时区: %s",
				now.Format("2006-01-02 15:04:05 MST"),
				now.UTC().Format("2006-01-02 15:04:05 MST"),
				now.Unix(),
				now.Format("MST"),
			), nil
		},
	}
}

// ============================================================
// 8. deep_search（联网搜索，内置）
// ============================================================

// deepSearchTool 返回深度网络搜索工具定义。
//
// 与其它文件/命令工具不同，deep_search 本身不执行抓取逻辑 —— 它把检索目标
// 交给 Context.DeepSearchRunner 回调（由 tool_call actuator 注入）。执行器据此
// 创建一个搜索子任务（复用 workflow 子任务机制），同步等待子任务跑完
// 「模型 + fetch」多轮循环后返回汇总结论。这样主对话的上下文不会被搜索过程
// 中反复抓取的超长网页文本撑爆，看到的只是一份收口后的结论。
func deepSearchTool() *ToolDef {
	return &ToolDef{
		Name:        model.DeepSearchToolName,
		Description: "深度网络搜索：针对一个检索目标，启动一个专门的搜索子任务（内部可多次抓取网页并交叉综合），最后返回一份结构化的汇总结论。适用于需要查证事实、搜集多方资料、追踪最新动态等复杂检索，比单次抓取更全面。",
		InputSchema: objectSchema(map[string]interface{}{
			"query": strProp("检索目标/问题，尽量具体（如「2026 年 AIGC 视频生成开源模型对比」）"),
			"max_rounds": map[string]interface{}{
				"type":        "integer",
				"description": "（可选）最大检索轮次，默认 8，范围 1~20。轮次越多检索越充分但越耗时",
			},
		}, []string{"query"}),
		// 内部要等一个子任务跑完，单次调用远超普通工具的 60s 兜底。
		Timeout: 15 * time.Minute,
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			if wc == nil || wc.DeepSearchRunner == nil {
				return "", fmt.Errorf("deep_search 不可用：未注入搜索执行器")
			}
			query := strArg(args, "query")
			if strings.TrimSpace(query) == "" {
				return "", fmt.Errorf("deep_search 缺少 query 参数")
			}
			maxRounds := 0 // 0 表示交给执行器取默认值
			if v, ok := args["max_rounds"]; ok {
				if n, ok := intArg(v); ok {
					maxRounds = n
				}
			}
			return wc.DeepSearchRunner(ctx, query, maxRounds)
		},
	}
}

// intArg 从工具入参里取整数参数。JSON 反序列化后整数可能是 float64 或 int，
// 两者都兼容；其它类型返回 ok=false。
func intArg(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	default:
		return 0, false
	}
}

// ============================================================
// 8.5 recall_context（历史取回，内置）
// ============================================================

// recallContextTool 返回历史取回工具定义。
//
// 它是上下文压缩的逆动作。压缩有损且不可逆：一段历史被摘要化后，原始的
// 工具输出、报错栈、代码片段就永久离开了上下文。压缩产物旁挂了句柄
// （历史摘要列出 task_id、当前任务摘要列出被压掉的 turn_id、工作流摘要
// 每个节点带 task_id），本工具让模型拿句柄换回原文。
//
// 与 deep_search 一样不在本包执行实际逻辑 —— 取回要读 task_turn 表、
// 可能还要调 LLM 做按需提取，都由 Context.ContextRecaller 回调完成，
// 保持 workspace 包不反向依赖 dao 与 llm。
func recallContextTool() *ToolDef {
	return &ToolDef{
		Name: model.RecallContextToolName,
		Description: "取回被上下文压缩掉的历史原文。当摘要里的信息不足以支撑判断（例如摘要只说「命令执行失败」但你需要看原始报错、" +
			"摘要提到某个结论但你需要核对当时的原始数据）时使用。摘要中形如 task_id=xxx、turn_id=123 的标识就是本工具的入参。" +
			"二选一：给 turn_id 取回某一轮的原始记录（最精确）；给 task_id 取回某轮对话的整段交互记录（较长时需配合 query 按需提取）。",
		InputSchema: objectSchema(map[string]interface{}{
			"turn_id": map[string]interface{}{
				"type":        "integer",
				"description": "要取回的 turn 编号，来自摘要末尾「以上摘要覆盖的 turn_id：…」列表。与 task_id 二选一，优先用它（返回原文，最精确）",
			},
			"task_id": strProp("要取回的任务 ID，来自「已压缩历史索引」或工作流摘要里的 task_id。与 turn_id 二选一"),
			"query":   strProp("（task_id 模式强烈建议填写）你要从这段历史里查什么。填了就只返回与之相关的内容并保留原始细节；不填则返回整段记录全文，包含大量无关的过程噪声，白占上下文。记录本身很短时无论填不填都直接给全文"),
		}, nil),
		// 内部可能要跑 map-reduce 若干次 LLM 调用，远超普通工具的 60s 兜底。
		Timeout: 5 * time.Minute,
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			if wc == nil || wc.ContextRecaller == nil {
				return "", fmt.Errorf("recall_context 不可用：未注入取回执行器")
			}
			req := ContextRecallRequest{
				TaskID: strings.TrimSpace(strArg(args, "task_id")),
				Query:  strArg(args, "query"),
			}
			if v, ok := args["turn_id"]; ok {
				if n, ok := intArg(v); ok && n > 0 {
					req.TurnID = int64(n)
				}
			}
			// 两个都不给无法定位目标；两个都给则意图不明（取一轮还是取整段），
			// 都直接报错让模型重新选，而不是替它猜一个。
			if req.TurnID <= 0 && req.TaskID == "" {
				return "", fmt.Errorf("recall_context 需要 turn_id 或 task_id 之一")
			}
			if req.TurnID > 0 && req.TaskID != "" {
				return "", fmt.Errorf("recall_context 的 turn_id 与 task_id 只能给一个")
			}
			return wc.ContextRecaller(ctx, req)
		},
	}
}

// ============================================================
// 9. analyze_image（条件注入，不进 BuiltinTools）
// ============================================================

// maxImageBytes analyze_image 允许读取的图片体积上限（约 20MB）。
// 图片会 base64 后内联进多模态请求，过大既拖慢传输又容易触发网关体积限制。
const maxImageBytes = 20 * 1024 * 1024

// AnalyzeImageTool 返回图片理解工具定义。
//
// 与 BuiltinTools() 里的工具不同，本工具不默认对所有对话可用：只有当前模型
// supports_image=1（多模态）时才由 toolProvider 动态注入（见 actuator 包）。
// 非多模态模型下不存在该工具，模型应直接告知用户无法查看图像，而不是退回
// read_file / run_command 跑 OCR。
//
// 执行依赖 Context.VisionAnalyzer 回调：真正调模型生成描述的动作由上层注入，
// 本工具只负责读图、判断 MIME、把字节交给回调，保持 workspace 包不反向依赖 LLM。
func AnalyzeImageTool() *ToolDef {
	return &ToolDef{
		Name:        "analyze_image",
		Description: "理解/分析工作区内的一张图片，返回图片的结构化描述（含图中文字、物体、布局、图表数据等）。当用户提供图片、需要识别或理解图中内容时优先使用本工具，不要用 read_file（图片是二进制会乱码）或 run_command 跑 OCR。",
		InputSchema: objectSchema(map[string]interface{}{
			"path":     strProp("图片路径，相对工作区（如 images/流程图.png）"),
			"question": strProp("（可选）希望重点回答的关于图片的问题；不填则返回图片的通用结构化描述"),
		}, []string{"path"}),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			if wc == nil || wc.VisionAnalyzer == nil {
				return "", fmt.Errorf("当前模型不支持图片理解，无法分析图片；请让用户改用文字描述图片内容")
			}
			path := strArg(args, "path")
			if strings.TrimSpace(path) == "" {
				return "", fmt.Errorf("analyze_image 缺少 path 参数")
			}
			abs, err := resolveIn(wc, path)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(abs)
			if err != nil {
				return "", fmt.Errorf("读取图片失败: %w", err)
			}
			if info.Size() > maxImageBytes {
				return "", fmt.Errorf("图片过大（%.1fMB），超过 20MB 上限", float64(info.Size())/(1024*1024))
			}
			data, err := os.ReadFile(abs)
			if err != nil {
				return "", fmt.Errorf("读取图片失败: %w", err)
			}
			mime := imageMimeType(filepath.Ext(abs))
			question := strArg(args, "question")
			return wc.VisionAnalyzer(ctx, data, mime, question)
		},
	}
}

// imageMimeType 按扩展名推断图片 MIME 类型；无法识别时回落为通用 image/*。
func imageMimeType(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return "image/*"
	}
}

// ============================================================
// 内部辅助
// ============================================================

// resolveIn 把相对路径解析到当前任务工作区内（沙箱校验）。
func resolveIn(wc *Context, relPath string) (string, error) {
	if wc == nil || strings.TrimSpace(wc.WorkspaceDir) == "" {
		return "", fmt.Errorf("当前任务没有工作区目录，无法执行文件操作")
	}
	m := &Manager{root: wc.WorkspaceDir}
	return m.Resolve(wc.WorkspaceDir, relPath)
}

// limitedBuffer 一个带上限的输出缓冲：超过上限后丢弃多余字节，但 Write 仍返回
// len(p) 假装全部写入，避免命令因 stdout/stderr 写不下被 SIGPIPE 杀掉。
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newLimitedBuffer(max int) *limitedBuffer {
	return &limitedBuffer{max: max}
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.max {
		b.truncated = true
		return len(p), nil
	}
	room := b.max - b.buf.Len()
	if len(p) > room {
		b.buf.Write(p[:room])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
