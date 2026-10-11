// Package workspace — 工作区产物扫描。
//
// 用途：任务结束时，确定性地识别「本次任务产出了哪些文件」，填充
// AgentOutputData.OutputFiles，无需依赖模型自报（模型会漏报、错报文件名）。
//
// 判定方式：扫描工作区内 mtime 晚于任务开始时间的文件。
//
// 为什么用 mtime 而不是「任务开始时打快照、结束时 diff」：
//   - 无状态：不必把快照持久化到 DB 或内存，进程重启/多副本都不受影响
//   - 天然正确：上一轮任务写完就没再动的文件 mtime 早于本次 start_time，
//     不会被重复计入；本次真正写过的文件必然被计入
//
// 排除项见 ignoredDirs / ignoredExts。其中 inputs/ 必须排除 ——
// 用户上传的附件落在那里，上传时间紧邻任务创建时间，
// 不排除会被误报成「任务的产出」。
package workspace

import (
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InputsDirName 用户上传附件的存放目录名（相对工作区）。
// 产物扫描会跳过它 —— 那是输入而非产出。
const InputsDirName = "inputs"

// OutputsDirName 框架自动落盘长文本产物的目录名（相对工作区）。
// 与模型自己 write_file 选择的路径无关，仅用于「回复过长自动转存」。
const OutputsDirName = "outputs"

// maxArtifacts 单个任务最多上报的产物数量。
// 超出后截断 —— 产物列表是给人看的交付清单，不是完整文件索引；
// 某些任务（如 npm install）会瞬间产生上万文件，全量上报毫无意义。
const maxArtifacts = 200

// maxArtifactScanEntries 扫描过程中最多遍历的目录项数量，防御性上限。
// 命中后停止遍历，避免超大工作区把任务收口卡在文件遍历上。
const maxArtifactScanEntries = 50000

// ignoredDirs 扫描产物时整棵跳过的目录名。
//
// 分四类：
//   - 版本控制与编辑器元数据：.git / .idea / .vscode
//   - 依赖与缓存目录：node_modules / __pycache__ / .venv / .cache 等
//   - 框架自身目录：inputs（用户上传，属于输入侧）
//   - 约定中间产物目录：.scratch（system prompt 引导模型把探测脚本、调试图片
//     等过程文件写到这里，不进交付清单）
//
// 注意不排除 dist / build —— 前端构建产物往往正是用户要的交付物。
var ignoredDirs = map[string]bool{
	".git":          true,
	".svn":          true,
	".hg":           true,
	".idea":         true,
	".vscode":       true,
	"node_modules":  true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	".venv":         true,
	"venv":          true,
	".cache":        true,
	".scratch":      true,
	InputsDirName:   true,
}

// ignoredExts 忽略的文件扩展名（编译中间产物、锁文件等）。
var ignoredExts = map[string]bool{
	".pyc":   true,
	".pyo":   true,
	".o":     true,
	".obj":   true,
	".so":    true,
	".a":     true,
	".class": true,
	".swp":   true,
	".tmp":   true,
}

// ignoredNames 按文件名忽略。
var ignoredNames = map[string]bool{
	".DS_Store": true,
	"Thumbs.db": true,
}

// Artifact 工作区内的一个产物文件。
type Artifact struct {
	// RelPath 相对工作区根的路径，使用 / 分隔（跨平台稳定，可直接给前端拼下载链接）
	RelPath string
	// Name 文件名（不含目录）
	Name string
	// Size 字节数
	Size int64
	// ModTime 最后修改时间
	ModTime time.Time
	// MimeType 按扩展名推断的 MIME 类型，推断不出时为空
	MimeType string
	// FileType 归一化的文件大类，对应 model.FileInfo.FileType：
	// 1-image 2-video 3-text 4-other
	FileType int
}

// 文件大类枚举。与 model.FileInfo.FileType 保持同一套取值。
//
// 之所以在此重复定义而不引用 model 包：pkg/ 下的包不反向依赖业务 model，
// 避免 model ←→ pkg 形成循环。调用方按同一取值转换即可。
const (
	FileTypeImage = 1
	FileTypeVideo = 2
	FileTypeText  = 3
	FileTypeOther = 4
)

// ScanArtifacts 扫描 wsDir 下 mtime 晚于 since 的文件，作为本次任务的产物。
//
// since 为零值时返回 nil —— 调用方拿不到任务开始时间时，宁可不报产物，
// 也不要把工作区里的历史文件全量当成本次产出。
//
// 返回结果按 ModTime 倒序（最新产出在前），最多 maxArtifacts 条。
// 目录本身不计入，只返回普通文件。
func ScanArtifacts(wsDir string, since time.Time) []Artifact {
	if strings.TrimSpace(wsDir) == "" || since.IsZero() {
		return nil
	}
	if st, err := os.Stat(wsDir); err != nil || !st.IsDir() {
		return nil
	}
	return scanDir(wsDir, wsDir, since)
}

// ScanArtifactsIn 扫描 wsDir 下 subDir 子目录内的全部文件，不按 mtime 过滤。
//
// 用途：任务持有**独立产出目录**时（见 model.TaskInputData.ArtifactDir），
// 目录内的文件在归属上是确定的，不需要再用 mtime 判定 —— 而 mtime 判定在
// 并行场景下根本判不准：工作流多个节点的执行窗口互相重叠，每个节点收口时
// 都会把同一时刻其它节点（及其 deep_search 子任务）写的文件一并算成自己的
// 产物（实测三个并行节点的产物清单都是同样的 24 个文件）。
//
// subDir 为空表示扫描整个 wsDir。返回的 RelPath 始终相对 wsDir。
func ScanArtifactsIn(wsDir, subDir string) []Artifact {
	if strings.TrimSpace(wsDir) == "" {
		return nil
	}
	root := wsDir
	if subDir != "" {
		root = filepath.Join(wsDir, filepath.FromSlash(subDir))
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	// since 传零值：scanDir 据此跳过 mtime 判定（本函数语义就是全量收）
	return scanDir(wsDir, root, time.Time{})
}

// scanDir 从 root 开始遍历，收集产物。
//
// wsDir 是工作区根，用于计算 RelPath；root 是本次遍历的起点，可与 wsDir 相同
// （全量扫描）或是它的子目录（归属明确的定向扫描）。
//
// since 为零值时不做 mtime 过滤 —— 只有调用方明确给出「任务开始时间」时才
// 按时间判定归属，避免拿不到时间基准时把历史文件全量误报。
func scanDir(wsDir, root string, since time.Time) []Artifact {
	var (
		out     []Artifact
		visited int
	)

	// 忽略 Walk 过程中的单点错误：产物列表是尽力而为的展示信息，
	// 不能因为某个文件权限异常就让整个任务收口失败。
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 目录不可读时跳过整棵子树，其余继续
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		visited++
		if visited > maxArtifactScanEntries {
			return filepath.SkipAll
		}

		if d.IsDir() {
			// 遍历起点自身不判断名字，否则起点若恰好叫 build 会被整棵跳过
			if path == root {
				return nil
			}
			if ignoredDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != "." {
				return fs.SkipDir
			}
			return nil
		}

		name := d.Name()
		if ignoredNames[name] || strings.HasPrefix(name, ".") {
			return nil
		}
		if ignoredExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		// mtime 判定：只收本次任务开始之后被写过的文件。
		// since 为零值表示调用方不要求按时间判定（目录归属已明确），全收。
		if !since.IsZero() && !info.ModTime().After(since) {
			return nil
		}

		rel, rerr := filepath.Rel(wsDir, path)
		if rerr != nil {
			return nil
		}

		out = append(out, Artifact{
			RelPath:  filepath.ToSlash(rel),
			Name:     name,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			MimeType: DetectMimeType(name),
			FileType: DetectFileType(name),
		})
		return nil
	})

	// 最新产出排前面：用户最关心刚生成的那份交付物
	sort.Slice(out, func(i, j int) bool {
		if out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].RelPath < out[j].RelPath
		}
		return out[i].ModTime.After(out[j].ModTime)
	})

	if len(out) > maxArtifacts {
		out = out[:maxArtifacts]
	}
	return out
}

// DetectMimeType 按扩展名推断 MIME 类型，推断不出时返回空字符串。
//
// 对标准库未覆盖但在 agent 产物里高频出现的类型做补充（md / yaml / go 等）。
func DetectMimeType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if m, ok := extraMimeTypes[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		// 去掉 charset 参数，只保留主类型，便于前端按前缀判断
		if i := strings.Index(m, ";"); i > 0 {
			return strings.TrimSpace(m[:i])
		}
		return m
	}
	return ""
}

// extraMimeTypes 标准库 mime 包未覆盖 / 覆盖不准的扩展名补充表。
var extraMimeTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".yaml":     "text/yaml",
	".yml":      "text/yaml",
	".toml":     "text/toml",
	".go":       "text/x-go",
	".py":       "text/x-python",
	".ts":       "text/typescript",
	".tsx":      "text/typescript",
	".jsx":      "text/javascript",
	".sh":       "text/x-shellscript",
	".sql":      "text/x-sql",
	".log":      "text/plain",
	".env":      "text/plain",
	".csv":      "text/csv",
	".tsv":      "text/tab-separated-values",
}

// textExts 视为文本类的扩展名。
//
// 单独维护而不只靠 MIME 前缀判断：application/json、application/xml
// 这类 MIME 主类型不是 text，但内容是文本、模型能直接读。
var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".json": true, ".jsonl": true,
	".xml": true, ".html": true, ".htm": true, ".css": true, ".csv": true, ".tsv": true,
	".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".conf": true, ".env": true,
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".java": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true, ".cc": true,
	".rs": true, ".rb": true, ".php": true, ".sh": true, ".bash": true, ".zsh": true,
	".sql": true, ".log": true, ".proto": true, ".graphql": true, ".vue": true, ".svelte": true,
}

// DetectFileType 按扩展名/MIME 推断归一化文件大类（1-image 2-video 3-text 4-other）。
func DetectFileType(name string) int {
	ext := strings.ToLower(filepath.Ext(name))
	if textExts[ext] {
		return FileTypeText
	}
	m := DetectMimeType(name)
	switch {
	case strings.HasPrefix(m, "image/"):
		return FileTypeImage
	case strings.HasPrefix(m, "video/"):
		return FileTypeVideo
	case strings.HasPrefix(m, "text/"):
		return FileTypeText
	}
	return FileTypeOther
}

// IsTextFile 判断文件是否为文本类（可直接读进 prompt / 用 read_file 处理）。
func IsTextFile(name string) bool {
	return DetectFileType(name) == FileTypeText
}
