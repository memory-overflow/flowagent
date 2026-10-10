// Package mcp 提供 MCP (Model Context Protocol) 客户端能力的统一封装。
//
// 职责：
//   - 把 model.MCPServer 的 DB 配置解析成可用的连接配置
//   - ListTools：拉取某个 server 暴露的工具列表
//   - CallTool：调用某个 server 上的具体工具
//
// 设计说明：
// MCP 连接是「用完即走」的短连接模式 —— 每次操作独立建连、握手、执行、关闭。
// 相比维护长连接池，这样做的好处是无状态、不需要处理连接失效重连，
// 代价是每次调用多一次握手开销。对当前调用频次（秒级）完全可接受。
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/memory-overflow/flowagent/model"
)

// DefaultTimeout 单次 MCP 操作的默认超时
const DefaultTimeout = 30 * time.Second

// clientName / clientVersion MCP 握手时上报的客户端标识
const (
	clientName    = "flowagent"
	clientVersion = "1.0.0"
)

// ============================================================
// ServerConfig — 连接配置
// ============================================================

// ServerConfig 一个 MCP server 的连接配置（从 model.MCPServer 解析而来）
type ServerConfig struct {
	// ServerID DB 主键，用于日志定位
	ServerID string
	// ServerName 人类可读名称，同时作为工具名的命名空间前缀
	ServerName string
	// Transport 传输类型：stdio / sse / streamable_http
	Transport string
	// Command / Args / Env  stdio 模式参数
	Command string
	Args    []string
	Env     []string
	// URL / Headers  sse 与 streamable_http 模式参数
	URL     string
	Headers map[string]string
}

// ConfigFromModel 把 DB 里的 model.MCPServer 解析成连接配置。
// Args / Env / Headers 在 DB 里是 JSON 字符串，这里统一解析。
func ConfigFromModel(s *model.MCPServer) (*ServerConfig, error) {
	if s == nil {
		return nil, fmt.Errorf("mcp: nil server model")
	}
	if s.Transport == "" {
		return nil, fmt.Errorf("mcp: server %s has empty transport", s.ID)
	}
	return &ServerConfig{
		ServerID:   s.ID,
		ServerName: s.Name,
		Transport:  s.Transport,
		Command:    s.Command,
		Args:       parseStringSlice(s.Args),
		Env:        envMapToSlice(parseStringMap(s.Env)),
		URL:        s.URL,
		Headers:    parseStringMap(s.Headers),
	}, nil
}

// ============================================================
// ToolInfo — 工具元信息
// ============================================================

// ToolInfo 一个 MCP 工具的元信息
type ToolInfo struct {
	// Name 工具原始名（server 内唯一，跨 server 可能重名）
	Name string
	// Description 工具用途说明
	Description string
	// InputSchema 入参 JSON Schema（object 类型）
	InputSchema map[string]interface{}
	// ServerID / ServerName 所属 server
	ServerID   string
	ServerName string
}

// ============================================================
// ToolResult — 工具调用结果
// ============================================================

// ToolResult 一次工具调用的返回
type ToolResult struct {
	// Content 文本化的返回内容（多个 content block 用换行拼接）。
	//
	// 图片/音频等非文本 content 不会被 base64 原样塞进来 —— 那会撑爆上下文。
	// 它们被提取到 Images，Content 里只留一个 [image:<mime>] / [audio:<mime>]
	// 占位标记，由调用方（actuator）决定落盘成文件还是丢弃。
	Content string
	// Images 工具返回的图片内容（base64 已解码成字节）。按 content 出现顺序。
	Images []ToolImage
	// IsError 工具是否返回业务错误（协议层面成功，但工具本身报错）
	IsError bool
	// Raw 原始返回，保留完整结构化信息便于排查
	Raw interface{}
}

// ToolImage 工具返回的一张图片。
type ToolImage struct {
	// Data 图片原始字节（已从 base64 解码）。
	Data []byte
	// MIMEType 图片 MIME 类型（如 image/jpeg / image/png）。
	MIMEType string
}

// ============================================================
// ListTools — 拉取工具列表
// ============================================================

// ListTools 连接指定 server 并拉取其工具列表。
func ListTools(ctx context.Context, cfg *ServerConfig) ([]ToolInfo, error) {
	if cfg == nil {
		return nil, fmt.Errorf("mcp: nil server config")
	}

	cli, closeFn, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	resp, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools on %s: %w", cfg.ServerName, err)
	}

	tools := make([]ToolInfo, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		tools = append(tools, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schemaToMap(t.InputSchema),
			ServerID:    cfg.ServerID,
			ServerName:  cfg.ServerName,
		})
	}
	return tools, nil
}

// ============================================================
// CallTool — 调用工具
// ============================================================

// CallTool 连接指定 server 并调用其上的某个工具。
//
// 返回 error 只代表协议层失败（连不上 / 握手失败 / 调用异常）。
// 工具自身返回的业务错误通过 ToolResult.IsError 表达，此时 error 为 nil。
func CallTool(
	ctx context.Context, cfg *ServerConfig,
	toolName string, args map[string]interface{},
) (*ToolResult, error) {
	if cfg == nil {
		return nil, fmt.Errorf("mcp: nil server config")
	}
	if toolName == "" {
		return nil, fmt.Errorf("mcp: empty tool name")
	}

	cli, closeFn, err := connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args

	resp, err := cli.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %s on %s: %w", toolName, cfg.ServerName, err)
	}

	content, images := flattenContent(resp.Content)
	return &ToolResult{
		Content: content,
		Images:  images,
		IsError: resp.IsError,
		Raw:     resp,
	}, nil
}

// ============================================================
// 内部：连接与握手
// ============================================================

// connect 建连并完成 MCP 握手，返回 client 与清理函数。
func connect(ctx context.Context, cfg *ServerConfig) (*client.Client, func(), error) {
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: build transport for %s: %w", cfg.ServerName, err)
	}

	cli := client.NewClient(tr)
	if err := cli.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("mcp: start client for %s: %w", cfg.ServerName, err)
	}

	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    clientName,
				Version: clientVersion,
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	}); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("mcp: initialize %s: %w", cfg.ServerName, err)
	}

	return cli, func() { cli.Close() }, nil
}

// buildTransport 根据 transport 类型构造对应的传输层。
func buildTransport(cfg *ServerConfig) (transport.Interface, error) {
	switch strings.ToLower(cfg.Transport) {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio transport requires command")
		}
		return transport.NewStdio(cfg.Command, cfg.Env, cfg.Args...), nil

	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("sse transport requires url")
		}
		var opts []transport.ClientOption
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(cfg.Headers))
		}
		return transport.NewSSE(cfg.URL, opts...)

	case "streamable_http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("streamable_http transport requires url")
		}
		var opts []transport.StreamableHTTPCOption
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
		}
		return transport.NewStreamableHTTP(cfg.URL, opts...)

	default:
		return nil, fmt.Errorf("unsupported transport: %s", cfg.Transport)
	}
}

// ============================================================
// 内部：数据转换
// ============================================================

// schemaToMap 把 mcp 的 InputSchema 转成通用 map，便于喂给 LLM tool schema。
func schemaToMap(schema interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// flattenContent 把工具返回的多个 content block 拼成一段文本，同时把图片
// 单独提取出来。
//
// 为什么图片要单独提取而不是 json.Marshal 塞进文本：
// MCP 的 image content 携带 base64 图片数据（截图动辄上百 KB），json.Marshal
// 后整段进 Content，会原样灌进后续 LLM 调用的上下文 —— 既撑爆窗口，对模型
// 也没有任何可读信息（模型拿到的是 base64 字符串，不是图片本体）。正确做法
// 是：Content 里只留 [image:<mime>] 占位，图片字节交给调用方落盘成文件，
// 再由模型用图像理解工具（如 analyze_image）按路径读取。
//
// 返回的 Images 与 Content 里的占位标记按出现顺序一一对应（同 mime 的多个
// 图片也按序），调用方用 strings.Replace(..., 1) 逐个替换即可对上号。
func flattenContent(contents []mcp.Content) (string, []ToolImage) {
	if len(contents) == 0 {
		return "", nil
	}
	var parts []string
	var images []ToolImage
	for _, c := range contents {
		if tc, ok := mcp.AsTextContent(c); ok {
			parts = append(parts, tc.Text)
			continue
		}
		if ic, ok := mcp.AsImageContent(c); ok {
			data, err := base64.StdEncoding.DecodeString(ic.Data)
			if err != nil {
				// base64 损坏：图片拿不到，占位标记也要标出来，不能静默消失。
				parts = append(parts, "[image:"+ic.MIMEType+"(解码失败)]")
				continue
			}
			images = append(images, ToolImage{Data: data, MIMEType: ic.MIMEType})
			parts = append(parts, "[image:"+ic.MIMEType+"]")
			continue
		}
		// 其余非文本类型（音频/资源链接等）：保留 JSON 表示，但不含大块图片。
		if b, err := json.Marshal(c); err == nil {
			parts = append(parts, string(b))
		}
	}
	return strings.Join(parts, "\n"), images
}

// SanitizeRaw 把 MCP 原始返回（*mcp.CallToolResult）里的图片 base64 数据剥离，
// 替换成可读占位，供调试页落库使用。
//
// 图片已由 flattenContent 提取进 ToolResult.Images 并（由上层）落盘成工作区文件，
// raw 里的 base64 对排查毫无意义，却动辄上百 KB —— 不剥会撑爆 turn.output 的
// 落库体量与调试页传输。这里只清洗图片 data、不改语义：文本与其它 content 原样保留。
//
// 非 *mcp.CallToolResult 类型（理论上不会发生）时原样返回，不 panic。
func SanitizeRaw(raw interface{}) interface{} {
	resp, ok := raw.(*mcp.CallToolResult)
	if !ok || resp == nil {
		return raw
	}
	for i, c := range resp.Content {
		ic, ok := mcp.AsImageContent(c)
		if !ok {
			continue
		}
		// 用占位替换 base64：Data 字段是 string，直接换成可读说明即可。
		// 注意 asType 返回的是副本，必须写回切片元素才会生效。
		resp.Content[i] = mcp.NewImageContent(
			fmt.Sprintf("<base64 图片数据（%d 字符）已省略，图片已落盘成工作区文件>", len(ic.Data)),
			ic.MIMEType,
		)
	}
	return raw
}

// parseStringSlice 解析 JSON 数组字符串
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

// parseStringMap 解析 JSON 对象字符串
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

// envMapToSlice 把 env map 转成 ["KEY=VAL", ...] 供 stdio transport 使用
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
