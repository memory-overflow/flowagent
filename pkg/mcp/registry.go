// Package mcp — 工具注册表
//
// Registry 负责把「一个对话绑定的多个 MCP server」聚合成一份可直接喂给 LLM 的工具清单，
// 并解决两个问题：
//
//  1. 跨 server 工具重名 —— 对外暴露的名字加上 server 前缀（<server>__<tool>），
//     LLM 返回该名字后能反查回具体 server + 原始工具名。
//  2. 每次 LLM 调用都重连拉工具列表开销大 —— 注册表按对话缓存一段时间。
package mcp

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

// nameSeparator LLM 侧工具名的 server 前缀分隔符。
// 用双下划线，避免与工具名本身常见的单下划线冲突。
const nameSeparator = "__"

// DefaultCacheTTL 工具清单默认缓存时长。
//
// 工具列表基本不随请求变化，冷启动一次 npx 子进程却要十几到几十秒
// （下载包 + 加载 Playwright 等内核），太频繁地过期缓存会让用户反复吃到
// 这个冷启动。因此把默认值从 5 分钟拉长到 30 分钟。
//
// 代价：MCP server 配置变更后，scheduler 进程内的缓存最多 30 分钟才刷新。
// 由于 server 与 scheduler 是独立进程（跨进程无法直接失效缓存），
// 如需立即生效可重启 scheduler 进程。
const DefaultCacheTTL = 30 * time.Minute

// invalidNameChars LLM function name 通常只允许字母数字下划线连字符，
// server 名可能含中文或空格，这里统一清洗。
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ============================================================
// RegisteredTool — 注册表中的一个工具
// ============================================================

// RegisteredTool 注册表中的一个工具条目
type RegisteredTool struct {
	// ExposedName 暴露给 LLM 的名字（已加 server 前缀并清洗）
	ExposedName string
	// OriginalName MCP server 上的原始工具名，实际调用时用这个
	OriginalName string
	// Description 工具说明
	Description string
	// InputSchema 入参 JSON Schema
	InputSchema map[string]interface{}
	// Server 所属 server 的连接配置，调用时直接复用
	Server *ServerConfig
}

// ============================================================
// Registry — 按对话维度的工具注册表
// ============================================================

// Registry 工具注册表。并发安全，可作为进程级单例长期持有。
type Registry struct {
	mu       sync.RWMutex
	cache    map[string]*registryEntry // key: chatID
	cacheTTL time.Duration
}

type registryEntry struct {
	tools     []RegisteredTool
	byName    map[string]*RegisteredTool // key: ExposedName
	expiresAt time.Time
}

// NewRegistry 创建工具注册表。ttl <= 0 时使用 DefaultCacheTTL。
func NewRegistry(ttl time.Duration) *Registry {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Registry{
		cache:    make(map[string]*registryEntry),
		cacheTTL: ttl,
	}
}

// Load 加载（或复用缓存）指定对话可用的工具清单。
//
// servers 为该对话已启用的 MCP server 配置列表。
// 单个 server 拉取失败不会中断整体 —— 记日志跳过，其余 server 正常可用，
// 因为个别工具不可用不应导致整个 agent 无法工作。
func (r *Registry) Load(ctx context.Context, chatID string, servers []*ServerConfig) ([]RegisteredTool, error) {
	// 先查缓存
	r.mu.RLock()
	if entry, ok := r.cache[chatID]; ok && time.Now().Before(entry.expiresAt) {
		tools := entry.tools
		r.mu.RUnlock()
		return tools, nil
	}
	r.mu.RUnlock()

	if len(servers) == 0 {
		r.store(chatID, nil)
		return nil, nil
	}

	// 逐个 server 拉工具
	var (
		all      []RegisteredTool
		failures []string
	)
	for _, srv := range servers {
		listCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
		tools, err := ListTools(listCtx, srv)
		cancel()
		if err != nil {
			log.Printf("[mcp-registry] chat=%s server=%s list tools failed: %v", chatID, srv.ServerName, err)
			failures = append(failures, srv.ServerName)
			continue
		}
		for _, t := range tools {
			all = append(all, RegisteredTool{
				ExposedName:  buildExposedName(srv.ServerName, t.Name),
				OriginalName: t.Name,
				Description:  buildDescription(srv.ServerName, t.Description),
				InputSchema:  normalizeSchema(t.InputSchema),
				Server:       srv,
			})
		}
	}

	// 处理清洗/截断后仍可能出现的重名，保证 ExposedName 全局唯一
	all = dedupExposedNames(all)

	r.store(chatID, all)

	if len(all) == 0 && len(failures) > 0 {
		return nil, fmt.Errorf("mcp: all %d servers failed to list tools: %s",
			len(failures), strings.Join(failures, ", "))
	}
	log.Printf("[mcp-registry] chat=%s loaded %d tools from %d servers (%d failed)",
		chatID, len(all), len(servers), len(failures))
	return all, nil
}

// Lookup 按 LLM 返回的 ExposedName 反查工具条目。
func (r *Registry) Lookup(chatID, exposedName string) (*RegisteredTool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.cache[chatID]
	if !ok {
		return nil, false
	}
	t, ok := entry.byName[exposedName]
	return t, ok
}

// Invalidate 主动失效某对话的工具缓存（server 配置变更后调用）。
func (r *Registry) Invalidate(chatID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, chatID)
}

// store 写入缓存
func (r *Registry) store(chatID string, tools []RegisteredTool) {
	byName := make(map[string]*RegisteredTool, len(tools))
	for i := range tools {
		byName[tools[i].ExposedName] = &tools[i]
	}
	r.mu.Lock()
	r.cache[chatID] = &registryEntry{
		tools:     tools,
		byName:    byName,
		expiresAt: time.Now().Add(r.cacheTTL),
	}
	r.mu.Unlock()
}

// ============================================================
// 命名处理
// ============================================================

// buildExposedName 生成暴露给 LLM 的工具名：<sanitized_server>__<tool>
//
// server 名可能是纯中文等全部被清洗掉的字符，此时退化为按 serverID 生成短前缀，
// 避免不同 server 的同名工具在 LLM 侧撞车。
func buildExposedName(serverName, toolName string) string {
	prefix := sanitizeName(serverName)
	if prefix == "" {
		prefix = fallbackPrefix(serverName)
	}
	tool := sanitizeName(toolName)
	if tool == "" {
		tool = "tool"
	}
	if prefix == "" {
		return tool
	}
	return prefix + nameSeparator + tool
}

// fallbackPrefix 当 server 名清洗后为空时，用其内容哈希生成稳定的短前缀。
// 同一个 server 名每次得到相同前缀，保证跨轮次工具名一致。
func fallbackPrefix(serverName string) string {
	if serverName == "" {
		return ""
	}
	var h uint32 = 2166136261
	for _, b := range []byte(serverName) {
		h ^= uint32(b)
		h *= 16777619
	}
	return fmt.Sprintf("srv%08x", h)
}

// sanitizeName 清洗名称，只保留 LLM function name 允许的字符
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = invalidNameChars.ReplaceAllString(s, "")
	return strings.Trim(s, "_-")
}

// buildDescription 在工具说明前标注来源 server，帮助 LLM 在同类工具间区分
func buildDescription(serverName, desc string) string {
	if serverName == "" {
		return desc
	}
	if desc == "" {
		return fmt.Sprintf("[来自 %s]", serverName)
	}
	return fmt.Sprintf("[来自 %s] %s", serverName, desc)
}

// dedupExposedNames 为重名的 ExposedName 追加序号后缀，保证唯一
func dedupExposedNames(tools []RegisteredTool) []RegisteredTool {
	seen := make(map[string]int, len(tools))
	for i := range tools {
		name := tools[i].ExposedName
		if name == "" {
			name = fmt.Sprintf("tool_%d", i)
			tools[i].ExposedName = name
		}
		if n, dup := seen[name]; dup {
			seen[name] = n + 1
			tools[i].ExposedName = fmt.Sprintf("%s_%d", name, n+1)
		} else {
			seen[name] = 1
		}
	}
	return tools
}

// normalizeSchema 保证 schema 是合法的 object 类型 JSON Schema。
// 部分 MCP server 返回的 schema 缺 properties，直接喂给 LLM 会被拒。
func normalizeSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]interface{}{}
	}
	return schema
}
