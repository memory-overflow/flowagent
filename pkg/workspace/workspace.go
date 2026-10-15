// Package workspace 提供工作流落地执行的工作区管理与内置 native 工具。
//
// 背景：框架此前只有「对话绑定的 MCP server」作为工具来源，没有内置的
// 文件/工作区操作能力，导致工作流子任务在裸 LLM 空转下只能产出文本
// 「虚假回复」，无法真正创建文件。
//
// 本包补上这类能力：Go 原生实现的文件/命令/时间/抓取工具，默认对
// 所有对话可用（在 toolProvider 里作为 toolKindBuiltin 注入），且路径被
// 强制约束在「工作区根目录 / <工作流任务ID> /」之下，杜绝越界写系统目录。
// Skill 的随包脚本也统一通过这里的 run_command 执行（skill 本身只是 prompt 指令）。
//
// 与 MCP 工具的差异：
//   - MCP 是外部工具服务（协议握手 + 子进程）；
//   - 这里的 native 工具直接在当前进程内执行文件 IO / shell / HTTP，
//     无握手、无子进程协议开销，且能做内核级的路径沙箱校验。
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot 工作区根目录缺省值（相对进程工作目录）。
const DefaultRoot = "./workspace"

// Manager 工作区管理器。持有根目录，负责按任务解析工作目录并做路径沙箱校验。
//
// 并发安全：Manager 本身不可变（root 只读），Resolve / Dir 均为纯函数，
// 天然可被多个调度 goroutine 共享。
type Manager struct {
	root string // 工作区根目录（绝对路径）
}

// NewManager 创建工作区管理器。root 为空时回落到 DefaultRoot。
func NewManager(root string) *Manager {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &Manager{root: filepath.Clean(abs)}
}

// Root 返回工作区根目录（绝对路径）。
func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.root
}

// Dir 返回指定 workspace key（工作流任务 ID / 对话任务 ID）的工作目录绝对路径。
// 只拼接，不创建目录（创建由具体工具在需要时 MkdirAll）。
func (m *Manager) Dir(key string) string {
	if m == nil || m.root == "" {
		return ""
	}
	if key == "" {
		return m.root
	}
	return filepath.Join(m.root, key)
}

// Resolve 把 agent 传入的相对路径解析成工作区内的绝对路径，做沙箱校验。
//
// 校验规则：
//   - 拒绝绝对路径（含 Windows 盘符 C:\ 与 ~ 家目录写法）
//   - filepath.Clean 后仍以 .. 开头（或等于 ..）视为越界
//   - 最终绝对路径必须落在 wsDir 前缀内（二次兜底）
//
// 返回的路径保证不会逃出 wsDir。
func (m *Manager) Resolve(wsDir, relPath string) (string, error) {
	if strings.TrimSpace(wsDir) == "" {
		return "", fmt.Errorf("当前任务没有工作区目录，无法执行文件操作")
	}

	rel := strings.TrimSpace(relPath)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "~") || isDrivePath(rel) {
		return "", fmt.Errorf("拒绝绝对路径 %q：请使用工作区内的相对路径", relPath)
	}

	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界 %q：不允许访问工作区之外", relPath)
	}

	abs := filepath.Join(wsDir, clean)
	if !withinDir(wsDir, abs) {
		return "", fmt.Errorf("路径越界 %q：不允许访问工作区之外", relPath)
	}
	return abs, nil
}

// withinDir 判断 path 是否在 dir 内（或等于 dir）。
func withinDir(dir, path string) bool {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// isDrivePath 判断是否为 Windows 盘符绝对路径（如 C:\、D:/）。
// filepath.IsAbs 在类 Unix 上不识别盘符写法，需单独拦截。
func isDrivePath(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if p[1] != ':' {
		return false
	}
	return p[2] == '\\' || p[2] == '/'
}

// ensureDir 确保 wsDir 存在（MkdirAll），返回 error。
func ensureDir(wsDir string) error {
	return os.MkdirAll(wsDir, 0o755)
}
