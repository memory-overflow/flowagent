// Package llmreg 提供「模型配置 → llm.Client」的构建与解析。
//
// 它把原先散落在 stage_scheduler 里的 protocol→provider switch 抽到这里，
// 让 server（模型 CRUD + 连通性测试）与 scheduler（按对话动态选模型）
// 共用同一套构建逻辑，避免两处各自维护一份 switch 而漂移。
//
// 为什么独立成包而非放进 pkg/llm：pkg/llm 是 provider 共同依赖的接口包，
// 若在它内部 import 子包 openai/gemini/azureopenai 会形成 import cycle，
// 因此构建逻辑必须放在 pkg/llm 之外、又能同时 import 三者的一层 —— 即本包。
package llmreg

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llm/azureopenai"
	"github.com/memory-overflow/flowagent/pkg/llm/claude"
	"github.com/memory-overflow/flowagent/pkg/llm/gemini"
	"github.com/memory-overflow/flowagent/pkg/llm/openai"
)

// 协议常量，与 model.ModelProtocol 取值一一对应（避免 llmreg 反向依赖 model）。
const (
	ProtocolOpenAI = "openai"
	ProtocolGemini = "gemini"
	ProtocolGPT    = "gpt"    // Azure OpenAI（gpt-5.6 系列）
	ProtocolClaude = "claude" // Anthropic Messages API
)

// GeminiTimeout Gemini provider 的单次调用超时。
//
// Gemini 网关把整段生成聚合后才一次性返回（headers 也要等生成完才到），
// provider 默认 60s 的 http.Client.Timeout 会在「等待响应头」阶段就短路。
// 单测实测生成整份 HTML 动画需 ~2.5min，故对齐到最长任务超时（15m），
// 让调度器的分级超时成为真正的上限，provider 不再提前抢跑。
const GeminiTimeout = 15 * time.Minute

// ClaudeTimeout Claude provider 的单次调用超时。
//
// 与 Gemini 同理：claude 走原生 Anthropic Messages API（lkeap /plan/anthropic
// 或官方 api.anthropic.com），非流式聚合返回，工作流子任务承接「一次性生成
// 大文件」这类重活时，单次 completion 可能远超 provider 默认 60s。
// 对齐调度器最长任务超时（15m，见 schedule 的 functionalCallTimeout /
// workflowSubTaskTimeout），让分级超时成为真正上限，provider 不提前抢跑。
const ClaudeTimeout = 15 * time.Minute

// ModelSpec 一条与协议无关的模型连接规格。
type ModelSpec struct {
	ID         string // 模型配置 ID（仅用于标识，构建客户端时不用）
	Protocol   string // openai / gemini / gpt / claude
	BaseURL    string
	APIKey     string
	Model      string
	APIVersion string // 仅 gpt(azure) 使用
	// Timeout 单次阻塞调用超时；<=0 时走各 provider 默认值（gemini 例外，见 BuildClient）。
	// 连通性测试（test）用它传短超时，避免不可达网关长时间挂起。
	Timeout time.Duration
	// ContextLimit 模型最大输入 token 数。0 表示未配置，由
	// contextmgr.ResolveContextLimit 按模型名查表兜底。
	// 构建客户端时不使用，仅供上下文管理层读取水位上限。
	ContextLimit int
}

// BuildClient 按 ModelSpec 构建对应的 llm.Client。
func BuildClient(spec ModelSpec) llm.Client {
	switch spec.Protocol {
	case ProtocolOpenAI:
		return openai.New(openai.Config{
			BaseURL: spec.BaseURL,
			APIKey:  spec.APIKey,
			Model:   spec.Model,
			Timeout: spec.Timeout,
		})
	case ProtocolGPT:
		return azureopenai.New(azureopenai.Config{
			BaseURL:    spec.BaseURL,
			APIKey:     spec.APIKey,
			Model:      spec.Model,
			APIVersion: spec.APIVersion,
			Timeout:    spec.Timeout,
		})
	case ProtocolClaude:
		timeout := spec.Timeout
		if timeout <= 0 {
			timeout = ClaudeTimeout
		}
		return claude.New(claude.Config{
			BaseURL: spec.BaseURL,
			APIKey:  spec.APIKey,
			Model:   spec.Model,
			Timeout: timeout,
		})
	default: // gemini
		timeout := spec.Timeout
		if timeout <= 0 {
			timeout = GeminiTimeout
		}
		return gemini.New(gemini.Config{
			BaseURL: spec.BaseURL,
			APIKey:  spec.APIKey,
			Model:   spec.Model,
			Timeout: timeout,
		})
	}
}

// ModelLoader 从存储里加载模型规格。
// 由 dao 层（或其适配器）实现，避免 Resolver 反向依赖 model/dao。
type ModelLoader interface {
	// LoadSpec 按 ID 加载；未找到返回 (nil, nil)。
	LoadSpec(ctx context.Context, id string) (*ModelSpec, error)
	// LoadDefaultSpec 加载默认且启用的模型；无默认返回 (nil, nil)。
	LoadDefaultSpec(ctx context.Context) (*ModelSpec, error)
}

// resolverCacheTTL 模型客户端缓存的存活时间。
//
// server 与 scheduler 是独立进程：模型 CRUD 发生在 server，而 Resolver 跑在
// scheduler。跨进程没有失效通知，若按 ID 永久缓存，server 改了模型配置后
// scheduler 仍会命中旧客户端（旧 key / 旧 base_url），必须重启才生效。
// 因此缓存带 TTL：命中仅在窗口内生效，过期后重新从 DB 加载并重建客户端，
// 保证模型配置改动在 TTL 内自动生效，无需重启 scheduler。
const resolverCacheTTL = 10 * time.Second

// cacheEntry 带过期时间的客户端缓存项。
type cacheEntry struct {
	client  llm.Client
	expires time.Time
}

// Resolver 按模型 ID 解析出 llm.Client，带 TTL 缓存与三档回退。
//
// 缓存命中避免「同一模型每轮 turn 都重建 http.Client」浪费连接池；TTL 保证
// server 侧的模型配置改动能在窗口内传播到 scheduler（见 resolverCacheTTL）。
type Resolver struct {
	loader   ModelLoader
	fallback llm.Client
	mu       sync.RWMutex
	cache    map[string]cacheEntry
}

// NewResolver 创建解析器。fallback 在「无默认模型 / 未配置任何模型」时兜底，
// 通常由 config.LLM 构建，保证未配置时系统仍可用。
func NewResolver(loader ModelLoader, fallback llm.Client) *Resolver {
	return &Resolver{
		loader:   loader,
		fallback: fallback,
		cache:    make(map[string]cacheEntry),
	}
}

// Resolve 解析模型 ID → llm.Client。
//
//   - id 非空：优先命中未过期的缓存，未命中则从 loader 加载并构建缓存；
//   - id 为空：加载默认模型；
//   - 都拿不到：回退 fallback（可能为 nil，此时返回错误）。
func (r *Resolver) Resolve(ctx context.Context, id string) (llm.Client, error) {
	if id != "" {
		r.mu.RLock()
		if e, ok := r.cache[id]; ok && time.Now().Before(e.expires) {
			r.mu.RUnlock()
			return e.client, nil
		}
		r.mu.RUnlock()
	}

	var spec *ModelSpec
	var err error
	if id != "" {
		spec, err = r.loader.LoadSpec(ctx, id)
	} else {
		spec, err = r.loader.LoadDefaultSpec(ctx)
	}
	if err != nil {
		return nil, err
	}
	if spec == nil {
		if r.fallback != nil {
			return r.fallback, nil
		}
		return nil, fmt.Errorf("llmreg: no model configured (id=%q)", id)
	}

	client := BuildClient(*spec)
	if id != "" {
		r.mu.Lock()
		r.cache[id] = cacheEntry{client: client, expires: time.Now().Add(resolverCacheTTL)}
		r.mu.Unlock()
	}
	return client, nil
}

// Invalidate 使指定模型的缓存立即失效。
// 当前跨进程（server 改 / scheduler 读）场景下不依赖它 —— TTL 兜底；保留此方法
// 供未来 server 与 scheduler 同进程合并时，模型 CRUD 后主动失效缓存。
func (r *Resolver) Invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// ResolveSpec 解析模型 ID → ModelSpec（而非客户端）。
//
// 上下文管理需要的是 spec 里的 ContextLimit 与 Model 名，不是客户端。
// 回退链与 Resolve 一致（指定 ID → 默认模型 → nil）。
//
// 刻意不复用 Resolve 的客户端缓存：那个缓存存的是 llm.Client，拿不回
// spec；而本方法每轮 turn 至多调用一次、只做一次主键查询，直接落 DB
// 的成本可以接受，换来的是「配置改动立刻生效」而非等 TTL。
//
// 返回 (nil, nil) 表示没有任何模型配置（此时调用方应回落到默认上限），
// 与 Resolve 走 fallback 客户端的语义对齐 —— fallback 来自 config.yaml，
// 本身就没有上下文上限信息。
func (r *Resolver) ResolveSpec(ctx context.Context, id string) (*ModelSpec, error) {
	if r.loader == nil {
		return nil, nil
	}
	if id != "" {
		return r.loader.LoadSpec(ctx, id)
	}
	return r.loader.LoadDefaultSpec(ctx)
}
