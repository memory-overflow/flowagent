// Package mcp — 内置 MCP 清单加载
//
// 从项目 mcp/builtins/*.json 加载预置的 MCP 清单，供「一键导入」使用。
// 清单只是模板，导入时会为每条记录生成新的 UUID 并写入 mcp_server 表。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// BuiltinManifest 一个内置 MCP 的清单
type BuiltinManifest struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// DescriptionEn 英文展示描述（仅前端英文界面用；给 LLM 的工具描述始终用中文 Description）。
	DescriptionEn string            `json:"description_en,omitempty"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	URL           string            `json:"url,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	// Hidden 为 true 时不在「内置 MCP 列表」里展示，也不参与预热。
	//
	// 用于标记「能力已内置进代码」的清单（如 fetch → 内置 web_search 抓取、
	// filesystem → 内置 read_file/list_dir、terminal → 内置 run_command）——
	// 保留 json 文件供代码加载（如 stage_scheduler 显式加载 fetch），
	// 但不让用户在前端重复手动启用。
	Hidden bool `json:"hidden,omitempty"`
}

// LoadBuiltins 读取指定目录下的所有 *.json 内置清单。
// 目录不存在返回空切片；单个文件解析失败会跳过并在日志中提示。
func LoadBuiltins(dir string) ([]*BuiltinManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mcp: read builtins dir %s: %w", dir, err)
	}

	var list []*BuiltinManifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("mcp: read %s: %w", p, err)
		}
		var m BuiltinManifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("mcp: parse %s: %w", p, err)
		}
		if m.Key == "" {
			// 用文件名作 fallback key
			m.Key = strings.TrimSuffix(e.Name(), ".json")
		}
		list = append(list, &m)
	}

	// key 排序稳定输出
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	return list, nil
}

// ServerConfig 把内置清单转成连接配置，供预热或直接连接使用。
// 内置清单的 Env 是 map，连接配置需要的是 ["KEY=VAL", ...] 切片。
func (m *BuiltinManifest) ServerConfig() *ServerConfig {
	if m == nil {
		return nil
	}
	e := m.ExpandEnv()
	return &ServerConfig{
		ServerName: m.Name,
		Transport:  e.Transport,
		Command:    normalizeCommand(e.Command),
		Args:       e.Args,
		Env:        envMapToSlice(e.Env),
		URL:        e.URL,
		Headers:    e.Headers,
	}
}

// normalizeCommand 对内置清单的 command 做跨平台适配。
//
// 内置清单（如 fetch）的 command 写的是 Unix 相对路径 .venv/bin/python，
// 而 Windows 上 venv 的可执行文件在 .venv\Scripts\python.exe。若不替换，
// Windows 上启动 stdio 子进程会报「executable file not found」导致该 MCP 直接不可用。
//
// 仅作用于内置清单（BuiltinManifest.ServerConfig），用户自定义导入的 MCP
// 配置不经过这里，不受影响。
func normalizeCommand(cmd string) string {
	if runtime.GOOS != "windows" {
		return cmd
	}
	switch filepath.ToSlash(filepath.Clean(cmd)) {
	case ".venv/bin/python", ".venv/bin/python3", ".venv/bin/pip":
		return filepath.FromSlash(".venv/Scripts/" + filepath.Base(cmd) + ".exe")
	}
	return cmd
}

// LoadBuiltinServer 从内置清单目录加载指定 key 的 server 连接配置。
// 目录不存在、读取失败或找不到指定 key 时返回 nil（由调用方决定是否降级）。
//
// 典型用途：框架默认注入某个内置 MCP（如 fetch）作为全局能力，
// 不依赖用户「一键导入」，也不进 mcp_server 表。
func LoadBuiltinServer(dir, key string) *ServerConfig {
	manifests, err := LoadBuiltins(dir)
	if err != nil {
		log.Printf("[mcp] load builtins dir %s failed: %v", dir, err)
		return nil
	}
	for _, m := range manifests {
		if m.Key == key {
			return m.ServerConfig()
		}
	}
	return nil
}

// PrewarmBuiltins 预热内置 MCP：对每个 stdio 清单做一次 ListTools，
// 把 npx 首次下载包 / Node 冷启动 / Playwright 加载内核等昂贵开销
// 摊到进程启动期，避免用户首个请求吃到几十秒的冷启动。
//
// 单个清单失败只记日志不中断，也不影响后续清单。应由调用方放进 goroutine。
func PrewarmBuiltins(dir string) {
	manifests, err := LoadBuiltins(dir)
	if err != nil {
		log.Printf("[mcp-prewarm] load builtins failed: %v", err)
		return
	}
	if len(manifests) == 0 {
		return
	}

	for _, m := range manifests {
		if m.Hidden {
			continue // 隐藏清单（能力已内置）不预热
		}
		cfg := m.ServerConfig()
		if cfg == nil || cfg.Transport == "" || cfg.Command == "" {
			continue // 非 stdio 或缺失命令的清单跳过
		}
		ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
		_, lerr := ListTools(ctx, cfg)
		cancel()
		if lerr != nil {
			log.Printf("[mcp-prewarm] %s failed: %v", m.Name, lerr)
			continue
		}
		log.Printf("[mcp-prewarm] %s ready", m.Name)
	}
}

// ExpandEnv 展开 args / env / url / headers 中的 ${VAR} 占位符（用 os.ExpandEnv）。
// 返回新副本，不修改入参。
func (m *BuiltinManifest) ExpandEnv() *BuiltinManifest {
	out := *m
	if len(m.Args) > 0 {
		out.Args = make([]string, len(m.Args))
		for i, a := range m.Args {
			out.Args[i] = os.ExpandEnv(a)
		}
	}
	if len(m.Env) > 0 {
		out.Env = make(map[string]string, len(m.Env))
		for k, v := range m.Env {
			out.Env[k] = os.ExpandEnv(v)
		}
	}
	out.URL = os.ExpandEnv(m.URL)
	// 英文描述不参与 ${VAR} 展开（纯展示文案），直接透传
	out.DescriptionEn = m.DescriptionEn
	if len(m.Headers) > 0 {
		out.Headers = make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			out.Headers[k] = os.ExpandEnv(v)
		}
	}
	return &out
}
