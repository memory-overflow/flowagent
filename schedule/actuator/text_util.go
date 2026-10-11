// Package actuator — 通用文本辅助
//
// 各 actuator 共用的输入渲染与截断函数，独立成文件避免与具体阶段逻辑耦合。
package actuator

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// 输入附件预览的尺寸约束。
//
// 为什么要做预览而不是只给路径让模型自己 read_file：
// 小体积文本文件（配置、CSV 片段、简短需求文档）直接内联能省掉一整轮
// tool_call 往返（一次 LLM 调用 + 一次工具调用）。大文件仍然只给路径，
// 由模型按需读取，避免撑爆上下文。
const (
	// inputPreviewMaxBytes 允许内联预览的文本文件体积上限（16KB）
	inputPreviewMaxBytes = 16 * 1024
	// inputPreviewMaxRunes 单个文件预览注入 prompt 的字符上限
	inputPreviewMaxRunes = 3000
	// inputPreviewTotalRunes 所有附件预览合计的字符上限，防止多文件叠加撑爆上下文
	inputPreviewTotalRunes = 12000
)

// renderUserInput 把 TaskInputData 展平成给 LLM 看的一段文本。
//
// wsDir 为当前任务的工作区目录，用于读取附件做内联预览；为空时只渲染清单不预览。
//
// 附件渲染成结构化清单而非一行文件名：模型需要知道「文件在哪、是什么类型、
// 多大、能用什么工具处理」才可能真正用上附件。只给个文件名的话，
// 模型往往直接忽略附件，或者臆造内容。
func renderUserInput(in *model.TaskInputData, wsDir string) string {
	if in == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(in.InputText)

	if len(in.InputFiles) == 0 {
		return b.String()
	}

	b.WriteString("\n\n")
	b.WriteString(renderInputFileManifest(in.InputFiles, wsDir))
	return b.String()
}

// renderInputFileManifest 渲染附件清单 + 可内联的文本预览 + 处理指引。
func renderInputFileManifest(files []model.FileInfo, wsDir string) string {
	var b strings.Builder
	b.WriteString("## 用户附件\n\n")
	b.WriteString("用户随本次输入提供了以下文件，已存放在你的工作区内。每个文件同时给出「相对工作区的路径」与「绝对路径」，两者都可直接用于 read_file / write_file / run_command：\n\n")

	var (
		hasImage    bool
		hasBinary   bool
		hasText     bool
		previewUsed int
	)

	for i, f := range files {
		switch f.FileType {
		case workspacepkg.FileTypeImage:
			hasImage = true
		case workspacepkg.FileTypeText:
			hasText = true
		default:
			hasBinary = true
		}

		path := f.Path
		if path == "" {
			path = f.Name
		}
		fmt.Fprintf(&b, "%d. `%s`\n", i+1, path)
		// 额外给绝对路径：弱模型对「相对工作区」这个抽象易混淆，容易自己脑补成
		// workspace/xxx 猜错（缺 chat_id 层），直接给可照抄的绝对路径省掉试错。
		if wsDir != "" {
			fmt.Fprintf(&b, "   - 绝对路径：`%s`\n", filepath.Join(wsDir, path))
		}
		fmt.Fprintf(&b, "   - 文件名：%s\n", f.Name)
		fmt.Fprintf(&b, "   - 类型：%s", fileKindLabel(f))
		if f.MimeType != "" {
			fmt.Fprintf(&b, "（%s）", f.MimeType)
		}
		b.WriteString("\n")
		if f.Size > 0 {
			fmt.Fprintf(&b, "   - 大小：%s\n", humanSize(f.Size))
		}
	}

	// 文本类小文件内联预览，省掉 read_file 往返
	if wsDir != "" && hasText {
		for _, f := range files {
			if previewUsed >= inputPreviewTotalRunes {
				break
			}
			if f.FileType != workspacepkg.FileTypeText || f.Path == "" {
				continue
			}
			if f.Size > inputPreviewMaxBytes {
				continue // 大文件让模型按需 read_file
			}
			preview, truncated := readFilePreview(wsDir, f.Path, inputPreviewMaxRunes)
			if preview == "" {
				continue
			}
			if remaining := inputPreviewTotalRunes - previewUsed; len([]rune(preview)) > remaining {
				preview = string([]rune(preview)[:remaining])
				truncated = true
			}
			previewUsed += len([]rune(preview))

			fmt.Fprintf(&b, "\n### 附件内容预览：`%s`\n\n```%s\n%s\n```\n",
				f.Path, codeFenceLang(f.Name), preview)
			if truncated {
				b.WriteString("（以上为节选，完整内容请用 read_file 读取该路径）\n")
			}
		}
	}

	// 处理指引：告诉模型这些文件能怎么用。
	// 不写这段的话，模型经常明明有工具却不去读文件，直接臆造内容。
	b.WriteString("\n处理方式：\n")
	if hasText {
		b.WriteString("- 文本类文件：用 read_file 读取上述路径获取完整内容。\n")
	}
	if hasBinary {
		b.WriteString("- PDF / Excel / 压缩包等二进制文件：不要直接 read_file（会得到乱码），" +
			"改用对应的 Skill 或 run_command 处理（如提取 PDF 文本、解析 xlsx 表格）。\n")
	}
	if hasImage {
		b.WriteString("- 图片文件：不要用 read_file 直接读取（会得到乱码）。图片的具体处理方式" +
			"（能否理解图像、能否提取图中文字）请严格遵循系统提示中的「图片理解」策略。\n")
	}
	b.WriteString("- 相对路径与绝对路径指向同一文件，任选其一传给 read_file / write_file / run_command 均可。\n")

	return b.String()
}

// fileKindLabel 返回文件大类的中文标签。
func fileKindLabel(f model.FileInfo) string {
	switch f.FileType {
	case workspacepkg.FileTypeImage:
		return "图片"
	case workspacepkg.FileTypeVideo:
		return "视频"
	case workspacepkg.FileTypeText:
		return "文本"
	default:
		return "二进制/其他"
	}
}

// codeFenceLang 按扩展名给出 markdown 代码块的语言标注，便于模型识别结构。
func codeFenceLang(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".json", ".jsonl":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".sql":
		return "sql"
	case ".sh", ".bash", ".zsh":
		return "bash"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".xml":
		return "xml"
	case ".md", ".markdown":
		return "markdown"
	case ".csv":
		return "csv"
	default:
		return ""
	}
}

// readFilePreview 读取工作区内文件的前 maxRunes 个字符。
//
// 路径经沙箱校验；读取失败一律返回空串（附件预览是增值信息，
// 不能因为某个文件读不出来就让整个任务失败）。
func readFilePreview(wsDir, relPath string, maxRunes int) (preview string, truncated bool) {
	if wsDir == "" || relPath == "" {
		return "", false
	}
	m := workspacepkg.NewManager(wsDir)
	abs, err := m.Resolve(wsDir, relPath)
	if err != nil {
		return "", false
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", false
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, inputPreviewMaxBytes+1))
	if err != nil {
		return "", false
	}
	over := len(data) > inputPreviewMaxBytes
	if over {
		data = data[:inputPreviewMaxBytes]
	}
	s := strings.TrimRight(string(data), "\n")
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes]), true
	}
	return s, over
}

// truncate 按字节截断，用于日志场景（内容多为 JSON，不担心多字节截断）
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateRunes 按 rune 截断字符串，避免截断在 UTF-8 多字节中间导致乱码。
// 用于会进入 prompt 或展示给用户的内容。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
