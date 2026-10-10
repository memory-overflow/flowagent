package actuator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// ============================================================
// MCP 图片结果落盘
// ============================================================
//
// 背景：computer-use 这类 MCP 的截图工具会返回 MCP image content（base64 图片，
// 动辄上百 KB）。pkg/mcp 的 flattenContent 已经把图片从文本里剥离，只留
// [image:<mime>] 占位，图片字节放在 ToolResult.Images 里。本文件负责在工具
// 执行侧把图片落盘成工作区文件，并把占位替换成「图片已保存到 xxx」的引用，
// 让模型后续用 analyze_image 按路径读取。
//
// 为什么不在 pkg/mcp 里落盘：pkg/mcp 是纯协议层，不该知道「工作区」「产出目录」
// 这些 agent 侧概念。落盘是 actuator 的职责（它持有任务工作区路径）。

// materializeMCPImages 把 MCP 工具返回的图片落盘，并把占位标记替换成文件引用。
//
// content 里每个 [image:<mime>] 标记与 images 里的图片按出现顺序一一对应。
// 同 mime 的多个图片也用 strings.Replace(..., 1) 逐个替换，顺序不会错位。
// 无图片时原样返回 content（零开销）。
func materializeMCPImages(content string, images []mcppkg.ToolImage, wsDir string) string {
	if len(images) == 0 {
		return content
	}
	for i, img := range images {
		ref := dumpMCPImage(img, wsDir, i)
		content = strings.Replace(content, "[image:"+img.MIMEType+"]", ref, 1)
	}
	return content
}

// dumpMCPImage 把一张图片落盘到工作区 outputs/ 目录，返回给模型的引用文本。
//
// 落盘失败或没有工作区时降级为「图片未保存」的提示，绝不把 base64 塞回文本 ——
// 那正是这个 bug 的根源（base64 对模型没有任何可读信息，只会撑爆上下文）。
func dumpMCPImage(img mcppkg.ToolImage, wsDir string, idx int) string {
	size := fmt.Sprintf("%d 字节", len(img.Data))
	if strings.TrimSpace(wsDir) == "" {
		return fmt.Sprintf("[图片未保存：当前任务无工作区]（%s，%s）", img.MIMEType, size)
	}

	dir := filepath.Join(wsDir, workspacepkg.OutputsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Sprintf("[图片保存失败：创建目录出错]（%s，%s）", img.MIMEType, size)
	}

	name := fmt.Sprintf("image-%d-%d%s", time.Now().UnixMilli(), idx, imageExt(img.MIMEType))
	if err := os.WriteFile(filepath.Join(dir, name), img.Data, 0o644); err != nil {
		return fmt.Sprintf("[图片保存失败：写入出错]（%s，%s）", img.MIMEType, size)
	}

	rel := filepath.Join(workspacepkg.OutputsDirName, name)
	return fmt.Sprintf(
		"图片已保存到 %s（%s，%s）。如需理解图片内容，请用 analyze_image 读取该文件。",
		rel, img.MIMEType, size)
}

// imageExt 从 MIME 类型映射出文件扩展名。
//
// 常见位图类型给标准扩展名；其余退化为 MIME 后半段（去点、去首字母大写），
// 兜底 .img。扩展名只影响用户可读性，analyze_image 实际靠 MIME 探测，不依赖它。
func imageExt(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/tiff":
		return ".tiff"
	case "image/svg+xml":
		return ".svg"
	}
	if i := strings.LastIndex(mime, "/"); i >= 0 && i+1 < len(mime) {
		if ext := strings.ToLower(mime[i+1:]); ext != "" && len(ext) <= 5 {
			return "." + strings.TrimPrefix(ext, ".")
		}
	}
	return ".img"
}
