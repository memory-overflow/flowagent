package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	pkgmcp "github.com/memory-overflow/flowagent/pkg/mcp"
)

// builtinsDir 返回内置 MCP 清单目录。优先读 env MCP_BUILTINS_DIR，
// 否则回落到相对工作目录的 ./mcp/builtins。
func builtinsDir() string {
	if v := os.Getenv("MCP_BUILTINS_DIR"); v != "" {
		return v
	}
	return "./mcp/builtins"
}

// ============================================================
// 请求 / 响应结构体
// ============================================================

type ListMCPServersRequest struct {
	Keyword string `json:"keyword"`
}

type MCPServerItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Desc       string `json:"description"`
	Transport  string `json:"transport"`
	Command    string `json:"command,omitempty"`
	Args       string `json:"args,omitempty"`
	Env        string `json:"env,omitempty"`
	URL        string `json:"url,omitempty"`
	Headers    string `json:"headers,omitempty"`
	Status     int    `json:"status"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	ToolsCount int32  `json:"tools_count"`
	CreateTs   int64  `json:"create_ts"`
	UpdateTs   int64  `json:"update_ts"`
	// 展示与数据分离：name/description 永远是中文原文（编辑回填、英文界面下的回落值），
	// *_display 按请求语言折算（英文有翻译用翻译，否则回落中文）。列表渲染用 *_display。
	NameDisplay string `json:"name_display"`
	DescDisplay string `json:"description_display"`
}

type ListMCPServersResponse struct {
	List []*MCPServerItem `json:"list"`
}

type MCPServerDetailRequest struct {
	ServerID string `json:"server_id"`
}

type CreateMCPServerRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type UpdateMCPServerRequest struct {
	ServerID    string            `json:"server_id"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Transport   string            `json:"transport,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type DeleteMCPServerRequest struct {
	ServerID string `json:"server_id"`
}

type TestMCPServerRequest struct {
	ServerID    string            `json:"server_id,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type MCPToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema string `json:"input_schema"`
}

type TestMCPServerResponse struct {
	Success  bool           `json:"success"`
	ErrorMsg string         `json:"error_msg,omitempty"`
	Tools    []*MCPToolInfo `json:"tools,omitempty"`
}

// ============================================================
// Handler 函数
// ============================================================

// ListMCPServers 获取 MCP 服务器列表
func ListMCPServers(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListMCPServersRequest)

	serverDAO := &dao.MCPServerDAO{}
	servers, err := serverDAO.ListServers(ctx, r.Keyword)
	if err != nil {
		return nil, err
	}

	list := make([]*MCPServerItem, 0, len(servers))
	for _, s := range servers {
		list = append(list, buildMCPServerItemLocalized(ctx, s))
	}

	return &ListMCPServersResponse{List: list}, nil
}

func ListMCPServersBinder() interface{} { return &ListMCPServersRequest{} }

// GetMCPServerDetail 获取 MCP 服务器详情
func GetMCPServerDetail(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*MCPServerDetailRequest)

	serverDAO := &dao.MCPServerDAO{}
	server, err := serverDAO.GetServerByID(ctx, r.ServerID)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "MCP 服务器不存在"}
	}

	return buildMCPServerItemLocalized(ctx, server), nil
}

func GetMCPServerDetailBinder() interface{} { return &MCPServerDetailRequest{} }

// CreateMCPServer 创建 MCP 服务器
func CreateMCPServer(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*CreateMCPServerRequest)

	now := time.Now()
	server := &model.MCPServer{
		ID:          uuid.New().String(),
		Name:        r.Name,
		Description: r.Description,
		Transport:   r.Transport,
		Command:     r.Command,
		Status:      model.MCPServerStatusDisconnected,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// 序列化数组/map 字段为 JSON 字符串
	if len(r.Args) > 0 {
		b, _ := json.Marshal(r.Args)
		server.Args = string(b)
	}
	if len(r.Env) > 0 {
		b, _ := json.Marshal(r.Env)
		server.Env = string(b)
	}
	if r.URL != "" {
		server.URL = r.URL
	}
	if len(r.Headers) > 0 {
		b, _ := json.Marshal(r.Headers)
		server.Headers = string(b)
	}

	serverDAO := &dao.MCPServerDAO{}
	if err := serverDAO.CreateServer(ctx, server); err != nil {
		return nil, err
	}

	return buildMCPServerItemLocalized(ctx, server), nil
}

func CreateMCPServerBinder() interface{} { return &CreateMCPServerRequest{} }

// UpdateMCPServer 更新 MCP 服务器
func UpdateMCPServer(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateMCPServerRequest)

	updates := map[string]interface{}{}
	if r.Name != "" {
		updates["name"] = r.Name
	}
	if r.Description != "" {
		updates["description"] = r.Description
	}
	if r.Transport != "" {
		updates["transport"] = r.Transport
	}
	if r.Command != "" {
		updates["command"] = r.Command
	}
	if r.Args != nil {
		b, _ := json.Marshal(r.Args)
		updates["args"] = string(b)
	}
	if r.Env != nil {
		b, _ := json.Marshal(r.Env)
		updates["env"] = string(b)
	}
	if r.URL != "" {
		updates["url"] = r.URL
	}
	if r.Headers != nil {
		b, _ := json.Marshal(r.Headers)
		updates["headers"] = string(b)
	}
	updates["updated_time"] = time.Now()

	serverDAO := &dao.MCPServerDAO{}
	if err := serverDAO.UpdateServer(ctx, r.ServerID, updates); err != nil {
		return nil, err
	}

	// 返回更新后的信息
	server, _ := serverDAO.GetServerByID(ctx, r.ServerID)
	if server != nil {
		return buildMCPServerItemLocalized(ctx, server), nil
	}
	return nil, nil
}

func UpdateMCPServerBinder() interface{} { return &UpdateMCPServerRequest{} }

// DeleteMCPServer 删除 MCP 服务器
func DeleteMCPServer(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*DeleteMCPServerRequest)

	serverDAO := &dao.MCPServerDAO{}
	if err := serverDAO.DeleteServer(ctx, r.ServerID); err != nil {
		return nil, err
	}
	return nil, nil
}

func DeleteMCPServerBinder() interface{} { return &DeleteMCPServerRequest{} }

// TestMCPServer 测试 MCP 服务器连接，并将状态写回数据库。
func TestMCPServer(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*TestMCPServerRequest)

	cfg, err := resolveTestConfig(ctx, r)
	if err != nil {
		return &TestMCPServerResponse{Success: false, ErrorMsg: err.Error()}, nil
	}

	result := doTestConnection(cfg)

	// 将测试结果写回数据库
	if r.ServerID != "" {
		serverDAO := &dao.MCPServerDAO{}
		var status model.MCPServerStatus
		var toolsCount int32
		var errorMsg string
		if result.Success {
			status = model.MCPServerStatusConnected
			toolsCount = int32(len(result.Tools))
		} else {
			status = model.MCPServerStatusError
			errorMsg = result.ErrorMsg
		}
		_ = serverDAO.UpdateServerStatus(ctx, r.ServerID, status, toolsCount, errorMsg)
	}

	return result, nil
}

func TestMCPServerBinder() interface{} { return &TestMCPServerRequest{} }

// resolveTestConfig 把请求解析为统一的 resolvedConfig：
// 优先用 server_id 查 DB，否则用请求体里的字段。
func resolveTestConfig(ctx context.Context, r *TestMCPServerRequest) (*resolvedConfig, error) {
	if r.ServerID != "" {
		s, err := (&dao.MCPServerDAO{}).GetServerByID(ctx, r.ServerID)
		if err != nil {
			return nil, fmt.Errorf("查询 MCP 服务器失败: %v", err)
		}
		if s == nil {
			return nil, fmt.Errorf("MCP 服务器不存在 (id=%s)", r.ServerID)
		}
		cfg := &resolvedConfig{
			Transport: s.Transport,
			Command:   s.Command,
			URL:       s.URL,
		}
		cfg.Args = parseStringSlice(s.Args)
		cfg.Env = envMapToSlice(parseStringMap(s.Env))
		cfg.Headers = parseStringMap(s.Headers)
		return cfg, nil
	}

	// 表单直传模式
	if r.Transport == "" {
		return nil, fmt.Errorf("transport 不能为空")
	}
	cfg := &resolvedConfig{
		Transport: r.Transport,
		Command:   r.Command,
		URL:       r.URL,
		Args:      r.Args,
		Env:       envMapToSlice(r.Env),
		Headers:   r.Headers,
	}
	return cfg, nil
}

type resolvedConfig struct {
	Transport string
	Command   string
	Args      []string
	Env       []string
	URL       string
	Headers   map[string]string
}

// doTestConnection 真正执行 MCP 握手和 list tools。
func doTestConnection(cfg *resolvedConfig) *TestMCPServerResponse {
	// 设置 10s 超时
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. 构造 transport
	tr, err := buildTransport(cfg)
	if err != nil {
		return &TestMCPServerResponse{Success: false, ErrorMsg: err.Error()}
	}

	// 2. 启动 client
	cli := client.NewClient(tr)
	if startErr := cli.Start(ctx); startErr != nil {
		return &TestMCPServerResponse{
			Success:  false,
			ErrorMsg: fmt.Sprintf("启动 MCP 客户端失败: %v", startErr),
		}
	}
	defer cli.Close()

	// 3. 握手 Initialize
	_, initErr := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "flowagent",
				Version: "1.0.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	})
	if initErr != nil {
		return &TestMCPServerResponse{
			Success:  false,
			ErrorMsg: fmt.Sprintf("Initialize 失败: %v", initErr),
		}
	}

	// 4. 拉取工具列表
	toolsResp, listErr := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if listErr != nil {
		return &TestMCPServerResponse{
			Success:  false,
			ErrorMsg: fmt.Sprintf("ListTools 失败: %v", listErr),
		}
	}

	tools := make([]*MCPToolInfo, 0, len(toolsResp.Tools))
	for _, t := range toolsResp.Tools {
		schemaBytes, _ := json.Marshal(t.InputSchema)
		tools = append(tools, &MCPToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: string(schemaBytes),
		})
	}

	return &TestMCPServerResponse{
		Success: true,
		Tools:   tools,
	}
}

// buildTransport 根据 transport 字符串构造对应的 transport。
func buildTransport(cfg *resolvedConfig) (transport.Interface, error) {
	switch strings.ToLower(cfg.Transport) {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio 模式必须指定 command")
		}
		return transport.NewStdio(cfg.Command, cfg.Env, cfg.Args...), nil
	case "sse":
		if cfg.URL == "" {
			return nil, fmt.Errorf("sse 模式必须指定 url")
		}
		opts := []transport.ClientOption{}
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHeaders(cfg.Headers))
		}
		return transport.NewSSE(cfg.URL, opts...)
	case "streamable_http":
		if cfg.URL == "" {
			return nil, fmt.Errorf("streamable_http 模式必须指定 url")
		}
		opts := []transport.StreamableHTTPCOption{}
		if len(cfg.Headers) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
		}
		return transport.NewStreamableHTTP(cfg.URL, opts...)
	default:
		return nil, fmt.Errorf("不支持的 transport 类型: %s", cfg.Transport)
	}
}

// envMapToSlice 把 map[string]string 转成 ["KEY=VAL", ...]，给 stdio transport 用。
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

// parseStringSlice 把可能为 JSON 数组字符串或 nil 的字段解析为 []string。
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

// parseStringMap 把可能为 JSON 对象字符串或 nil 的字段解析为 map[string]string。
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

// ============================================================
// 辅助函数
// ============================================================

func buildMCPServerItem(s *model.MCPServer) *MCPServerItem {
	return &MCPServerItem{
		ID:         s.ID,
		Name:       s.Name,
		Desc:       s.Description,
		Transport:  s.Transport,
		Command:    s.Command,
		Args:       s.Args,
		Env:        s.Env,
		URL:        s.URL,
		Headers:    s.Headers,
		Status:     int(s.Status),
		ErrorMsg:   s.ErrorMsg,
		ToolsCount: s.ToolsCount,
		CreateTs:   s.CreatedAt.UnixMilli(),
		UpdateTs:   s.UpdatedAt.UnixMilli(),
	}
}

// buildMCPServerItemLocalized 在 buildMCPServerItem 基础上按界面语言填展示字段。
// lang=en 且有英文翻译时 display 取英文，否则回落中文原文。
func buildMCPServerItemLocalized(c *gin.Context, s *model.MCPServer) *MCPServerItem {
	item := buildMCPServerItem(s)
	lang := LangFromContext(c)
	item.NameDisplay = PickDisplay(lang, s.Name, s.Name)
	item.DescDisplay = PickDisplay(lang, s.Description, s.DescriptionEn)
	return item
}

// ============================================================
// 内置 MCP 清单：列表 & 一键导入
// ============================================================

// BuiltinMCPItem 前端展示用的内置 MCP 条目
type BuiltinMCPItem struct {
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	// NameDisplay/DescDisplay 按请求语言折算的展示值（en 有翻译用翻译，否则回落中文）
	NameDisplay string            `json:"name_display"`
	DescDisplay string            `json:"description_display"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// AlreadyImported 数据库里已有同名记录（按 name 判重）
	AlreadyImported bool `json:"already_imported"`
}

type ListBuiltinMCPRequest struct{}

type ListBuiltinMCPResponse struct {
	Dir  string            `json:"dir"`
	List []*BuiltinMCPItem `json:"list"`
}

// ListBuiltinMCP 列出项目 mcp/builtins/ 目录下的所有内置 MCP 清单。
func ListBuiltinMCP(ctx *gin.Context, req interface{}) (interface{}, error) {
	dir := builtinsDir()
	manifests, err := pkgmcp.LoadBuiltins(dir)
	if err != nil {
		return nil, err
	}

	// 查一次已有 servers，用于标注 already_imported
	existing, _ := (&dao.MCPServerDAO{}).ListServers(ctx, "")
	nameSet := make(map[string]bool, len(existing))
	for _, s := range existing {
		nameSet[s.Name] = true
	}

	list := make([]*BuiltinMCPItem, 0, len(manifests))
	lang := LangFromContext(ctx)
	for _, m := range manifests {
		if m.Hidden {
			continue // 能力已内置进代码（fetch/filesystem/terminal），不展示给用户
		}
		e := m.ExpandEnv()
		list = append(list, &BuiltinMCPItem{
			Key:             m.Key,
			Name:            m.Name,
			Description:     m.Description,
			NameDisplay:     PickDisplay(lang, m.Name, m.Name),
			DescDisplay:     PickDisplay(lang, m.Description, m.DescriptionEn),
			Transport:       m.Transport,
			Command:         e.Command,
			Args:            e.Args,
			Env:             e.Env,
			URL:             e.URL,
			Headers:         e.Headers,
			AlreadyImported: nameSet[m.Name],
		})
	}
	return &ListBuiltinMCPResponse{Dir: dir, List: list}, nil
}

func ListBuiltinMCPBinder() interface{} { return &ListBuiltinMCPRequest{} }

type ImportBuiltinMCPRequest struct {
	// Keys 要导入的 key 列表；为空表示全部导入
	Keys []string `json:"keys"`
	// Overwrite=true 时，若已存在同名记录则覆盖（更新其配置）；否则跳过
	Overwrite bool `json:"overwrite"`
}

type ImportBuiltinMCPResponse struct {
	Imported []string `json:"imported"` // 新建的 name 列表
	Updated  []string `json:"updated"`  // 覆盖的 name 列表
	Skipped  []string `json:"skipped"`  // 因已存在被跳过的 name 列表
}

// ImportBuiltinMCP 一键把内置清单导入 mcp_server 表。
func ImportBuiltinMCP(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ImportBuiltinMCPRequest)

	manifests, err := pkgmcp.LoadBuiltins(builtinsDir())
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return &ImportBuiltinMCPResponse{
			Imported: []string{},
			Updated:  []string{},
			Skipped:  []string{},
		}, nil
	}

	// key 过滤
	keySet := map[string]bool{}
	for _, k := range r.Keys {
		keySet[k] = true
	}
	filter := func(m *pkgmcp.BuiltinManifest) bool {
		if m.Hidden {
			return false // 隐藏清单（能力已内置）不可导入
		}
		if len(keySet) == 0 {
			return true
		}
		return keySet[m.Key]
	}

	serverDAO := &dao.MCPServerDAO{}
	existing, err := serverDAO.ListServers(ctx, "")
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*model.MCPServer, len(existing))
	for _, s := range existing {
		byName[s.Name] = s
	}

	resp := &ImportBuiltinMCPResponse{
		Imported: []string{},
		Updated:  []string{},
		Skipped:  []string{},
	}
	for _, m := range manifests {
		if !filter(m) {
			continue
		}
		expanded := m.ExpandEnv()

		argsJSON, _ := json.Marshal(expanded.Args)
		envJSON, _ := json.Marshal(expanded.Env)
		headersJSON, _ := json.Marshal(expanded.Headers)

		if old, ok := byName[m.Name]; ok {
			if !r.Overwrite {
				resp.Skipped = append(resp.Skipped, m.Name)
				continue
			}
			updates := map[string]interface{}{
				"description":    m.Description,
				"description_en": m.DescriptionEn,
				"transport":      m.Transport,
				"command":        expanded.Command,
				"args":           string(argsJSON),
				"env":            string(envJSON),
				"url":            expanded.URL,
				"headers":        string(headersJSON),
				"updated_time":   time.Now(),
			}
			if err := serverDAO.UpdateServer(ctx, old.ID, updates); err != nil {
				return nil, fmt.Errorf("update %s: %w", m.Name, err)
			}
			resp.Updated = append(resp.Updated, m.Name)
			continue
		}

		now := time.Now()
		s := &model.MCPServer{
			ID:            uuid.New().String(),
			Name:          m.Name,
			Description:   m.Description,
			DescriptionEn: m.DescriptionEn,
			Transport:     m.Transport,
			Command:       expanded.Command,
			Args:          string(argsJSON),
			Env:           string(envJSON),
			URL:           expanded.URL,
			Headers:       string(headersJSON),
			Status:        model.MCPServerStatusDisconnected,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := serverDAO.CreateServer(ctx, s); err != nil {
			return nil, fmt.Errorf("create %s: %w", m.Name, err)
		}
		resp.Imported = append(resp.Imported, m.Name)
	}

	return resp, nil
}

func ImportBuiltinMCPBinder() interface{} { return &ImportBuiltinMCPRequest{} }
