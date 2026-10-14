// Package actuator — 工具加载
//
// functional_call 与 tool_call 都需要「对话可用的工具清单」：
//   - functional_call 用它生成 LLM tool schema
//   - tool_call 用它把 LLM 返回的工具名反查回真实执行方式
//
// 两者共用本组件，保证「暴露给 LLM 的名字」与「反查时用的名字」始终一致。
//
// 工具来源有三类，统一抽象成 ToolEntry：
//   - 内置 native 工具：框架内核能力（文件/命令/时间），默认对全部对话可用，对用户隐藏
//   - MCP 工具：跨 server 命名空间 <server>__<tool>，执行时走 MCP 协议
//   - Skill 工具：command 型 skill，命名空间 skill__<name>，执行时本地 shell 子进程
//
// 联网抓取（web_fetch）归入 MCP 一类：框架内置 mcp-server-fetch，仅在
// deep_search 子任务里注入（配合 web_search 做「定位 → 精读」两段式检索），
// 以取得 HTML→Markdown 结构化、charset 转码、robots 合规等完整抓取效果。
//
// 关键设计：ToolSet 自带 lookup 索引，不依赖 Registry 的内部缓存状态。
// 因为 tool_call 阶段可能在另一个调度器副本上执行，或缓存已过期，
// 此时直接查 Registry 内部缓存会 miss；而重新 Load 后从返回值建索引总是可靠的。
package actuator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	mcppkg "github.com/memory-overflow/flowagent/pkg/mcp"
	skillpkg "github.com/memory-overflow/flowagent/pkg/skill"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// summaryParamKey summary 参数的 schema key。
//
// B 方案：把「本次调用目的说明」作为每个工具的必填参数注入到 schema 里，
// 由 schema 强制模型填写 —— 相比依赖模型「伴随文本」（多数模型在 tool_call
// 阶段默认把 content 留空）更稳定。执行层会从 args 里提取该值作为思考过程的
// 折叠摘要，并在转发给真实工具前剥离它（MCP server 会因未知字段报错）。
const summaryParamKey = "summary"

// summaryParamDesc summary 参数在 schema 里的描述。
const summaryParamDesc = "本次调用的目的说明（一句话，用中文告诉用户你接下来要做什么、为什么）"

// ============================================================
// ToolEntry — 统一工具条目
// ============================================================

// toolKind 工具来源类型
type toolKind int

const (
	toolKindMCP     toolKind = 1 // MCP 协议工具
	toolKindSkill   toolKind = 2 // command 型 skill 本地执行
	toolKindBuiltin toolKind = 3 // 框架内置 native 工具（文件/命令/时间/抓取）
)

// ToolEntry 一个可被 LLM 调用的工具条目。MCP 与 Skill 二选一，
// 通过 Kind 区分，执行时按对应方式分发。
type ToolEntry struct {
	// ExposedName 暴露给 LLM 的名字（已加命名空间前缀并清洗）
	ExposedName string
	// Description 工具说明
	Description string
	// InputSchema 入参 JSON Schema
	InputSchema map[string]interface{}
	// Kind 来源类型
	Kind toolKind
	// MCP 当 Kind == toolKindMCP 时非空
	MCP *mcppkg.RegisteredTool
	// Skill 当 Kind == toolKindSkill 时非空
	Skill *skillpkg.Config
	// Builtin 当 Kind == toolKindBuiltin 时非空
	Builtin *workspacepkg.ToolDef
}

// ============================================================
// ToolSet — 一个对话可用的工具集合
// ============================================================

// ToolSet 对话可用工具的集合，同时提供 LLM schema 与反查能力
type ToolSet struct {
	tools  []ToolEntry
	byName map[string]*ToolEntry
}

// newToolSet 从统一工具条目构建工具集
func newToolSet(entries []ToolEntry) *ToolSet {
	ts := &ToolSet{
		tools:  entries,
		byName: make(map[string]*ToolEntry, len(entries)),
	}
	for i := range ts.tools {
		ts.byName[ts.tools[i].ExposedName] = &ts.tools[i]
	}
	return ts
}

// LLMTools 转成可直接传给 llm.ChatWithTools 的 schema 列表。
//
// 每个工具都会注入 summary 必填参数（B 方案）：让模型在调用工具时必填
// 一句目的说明，供思考过程默认折叠展示。注入采用深拷贝，不污染原始
// InputSchema（该 schema 还会被 buildLLMTurnInput 用于 turn 快照落库）。
func (s *ToolSet) LLMTools() []llm.Tool {
	if s == nil {
		return nil
	}
	tools := make([]llm.Tool, 0, len(s.tools))
	for _, e := range s.tools {
		tools = append(tools, llm.Tool{
			Name:        e.ExposedName,
			Description: e.Description,
			Parameters:  injectSummaryParam(e.InputSchema),
		})
	}
	return tools
}

// injectSummaryParam 深拷贝工具入参 schema，并注入 summary 必填参数。
//
// 深拷贝（JSON round-trip）确保不污染原始 InputSchema。容错处理非标准
// schema（缺 properties / required / type 字段），保证所有工具都能安全注入。
func injectSummaryParam(params map[string]interface{}) map[string]interface{} {
	cp := map[string]interface{}{}
	if params != nil {
		if b, err := json.Marshal(params); err == nil {
			_ = json.Unmarshal(b, &cp)
		} else {
			// marshal 失败（几乎不可能）时退化为浅拷贝
			for k, v := range params {
				cp[k] = v
			}
		}
	}

	props, _ := cp["properties"].(map[string]interface{})
	if props == nil {
		props = map[string]interface{}{}
		cp["properties"] = props
	}
	props[summaryParamKey] = map[string]interface{}{
		"type":        "string",
		"description": summaryParamDesc,
	}

	// 追加 required：兼容 []interface{}（JSON round-trip 后）与 []string（浅拷贝）两种形态
	var required []interface{}
	if r, ok := cp["required"].([]interface{}); ok {
		required = r
	} else if r, ok := cp["required"].([]string); ok {
		for _, s := range r {
			required = append(required, s)
		}
	}
	has := false
	for _, r := range required {
		if str, ok := r.(string); ok && str == summaryParamKey {
			has = true
			break
		}
	}
	if !has {
		required = append(required, summaryParamKey)
	}
	cp["required"] = required

	// 个别工具 schema 缺 type（如 get_current_time 传了空 properties 但有 type）
	// 统一兜底为 object，保证 provider 翻译不出错。
	if _, ok := cp["type"]; !ok {
		cp["type"] = "object"
	}
	return cp
}

// Lookup 按 LLM 返回的工具名反查工具条目
func (s *ToolSet) Lookup(exposedName string) (*ToolEntry, bool) {
	if s == nil {
		return nil, false
	}
	e, ok := s.byName[exposedName]
	return e, ok
}

// Names 返回所有暴露名（日志用）
func (s *ToolSet) Names() []string {
	if s == nil {
		return nil
	}
	names := make([]string, 0, len(s.tools))
	for _, e := range s.tools {
		names = append(names, e.ExposedName)
	}
	return names
}

// Len 工具数量
func (s *ToolSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.tools)
}

// ============================================================
// toolProvider — 工具加载器
// ============================================================

// toolProvider 负责从 DB + MCP server 加载对话可用的工具，
// 同时从 DB 加载对话启用的 Skill（prompt 型用于注入指令，command 型暴露为工具）。
type toolProvider struct {
	convMCPDAO   *dao.ConversationMCPDAO
	mcpServerDAO *dao.MCPServerDAO
	registry     *mcppkg.Registry

	// defaultMCP 框架级 MCP server（如 fetch 精读工具）。
	// 与对话绑定无关，对用户隐藏，加载失败只降级不阻断。
	// 注意：不注入主 agent，仅在 deep_search 子任务里随 web_search 一起注入
	// （见 applyDeepSearchSubTaskTools），主 agent 用 deep_search 工具做联网调研。
	defaultMCP []*mcppkg.ServerConfig

	convSkillDAO *dao.ConversationSkillDAO
	skillDAO     *dao.SkillDAO

	// convDAO / modelConfigDAO 用于判断「当前对话所选模型是否支持多模态」，
	// 决定是否注入 analyze_image 图片理解工具。
	convDAO        *dao.ConversationDAO
	modelConfigDAO *dao.ModelConfigDAO

	// searchConfigDAO 用于运行时读取全局检索引擎配置（search_config 表），
	// 决定 deep_search 子任务里注入的 web_search 用哪个引擎 + 哪把 key。
	searchConfigDAO *dao.SearchConfigDAO

	// projectRoot 项目根绝对路径（= filepath.Dir(workspace.Root())）。
	// 用于把 skill content 里的 `skills/` 相对脚本路径替换成绝对路径，
	// 保证 prompt 型 skill 的指令在 run_command（cwd=任务工作区）下也能落地。
	projectRoot string
}

// defaultMCPCacheKey 默认注入 MCP 工具在 registry 中的缓存 key。
// 这些工具是框架级全局能力，与具体对话无关，用固定 key 缓存，
// 避免每个对话各自冷启动一次 stdio 子进程（uvx/npx 下载 + 握手）。
const defaultMCPCacheKey = "__builtin_default_mcp__"

// newToolProvider 创建工具加载器
func newToolProvider(
	convMCPDAO *dao.ConversationMCPDAO,
	mcpServerDAO *dao.MCPServerDAO,
	registry *mcppkg.Registry,
	convSkillDAO *dao.ConversationSkillDAO,
	skillDAO *dao.SkillDAO,
	defaultMCP []*mcppkg.ServerConfig,
	convDAO *dao.ConversationDAO,
	modelConfigDAO *dao.ModelConfigDAO,
	searchConfigDAO *dao.SearchConfigDAO,
	ws *workspacepkg.Manager,
) *toolProvider {
	p := &toolProvider{
		convMCPDAO:      convMCPDAO,
		mcpServerDAO:    mcpServerDAO,
		registry:        registry,
		defaultMCP:      defaultMCP,
		convSkillDAO:    convSkillDAO,
		skillDAO:        skillDAO,
		convDAO:         convDAO,
		modelConfigDAO:  modelConfigDAO,
		searchConfigDAO: searchConfigDAO,
	}
	// 项目根 = 工作区根（绝对路径）的上一级。workspace.Root() 默认 <项目根>/workspace，
	// filepath.Dir 即项目根。ws 为 nil（单测等）时留空，跳过路径绝对化。
	if ws != nil {
		p.projectRoot = filepath.Dir(ws.Root())
	}
	return p
}

// Load 加载指定对话可用的工具集（内置 native 工具 + 对话绑定 MCP + command 型 skill）。
//
// 内置 native 工具（文件/命令/时间）默认对所有对话可用，不依赖任何绑定，
// 也不进 skill/mcp 配置列表 —— 它们属于框架内核能力，对用户隐藏。
// 框架级 MCP（fetch 精读）不在本方法注入 —— 它只在 deep_search 子任务里
// 随 web_search 注入（见 applyDeepSearchSubTaskTools），主 agent 不暴露 fetch。
// 对话未绑定任何 MCP/skill 时，仍会返回内置工具，保证工作流子任务有工具可落地。
func (p *toolProvider) Load(ctx context.Context, chatID string) (*ToolSet, error) {
	if p == nil {
		return newToolSet(nil), nil
	}

	entries := make([]ToolEntry, 0, 8+len(workspacepkg.BuiltinTools()))

	// 0. 内置 native 工具（框架级默认注入，优先级最高）
	for _, def := range workspacepkg.BuiltinTools() {
		if def == nil {
			continue
		}
		entries = append(entries, ToolEntry{
			ExposedName: def.Name,
			Description: def.Description,
			InputSchema: def.InputSchema,
			Kind:        toolKindBuiltin,
			Builtin:     def,
		})
	}

	// 0.1 图片理解工具（条件注入）：仅当前模型 supports_image=1 时暴露。
	// 非多模态模型不注入该工具，配合 system prompt 让模型直接告知用户无法看图，
	// 避免它退回 read_file / run_command 跑 OCR（效果差）。
	if p.resolveModelSupportsImage(ctx, chatID) {
		if def := workspacepkg.AnalyzeImageTool(); def != nil {
			entries = append(entries, ToolEntry{
				ExposedName: def.Name,
				Description: def.Description,
				InputSchema: def.InputSchema,
				Kind:        toolKindBuiltin,
				Builtin:     def,
			})
		}
	}

	// 1. 框架级 MCP（fetch 精读）对主 agent 一并注入。
	//
	// 这里曾经「只给 deep_search 子任务、不给主 agent」，理由是怕模型在主循环
	// 里反复抓网页撑爆上下文。实测结论相反：主 agent 需要某个具体 URL 的原文时
	// 没有正当工具可用，就会退回 run_command + curl 自己抓（一个调研对话里
	// run_command 115 次有 110 次是 curl）—— 抓取量更大、完全不受控、
	// 也没有 HTML→Markdown 转换与截断。
	//
	// 因此改为「给工具、但上约束」：
	//   · 配额：每个任务最多 fetchQuotaPerTask 次，超配额返回引导文本
	//     （不是报错，模型可据此改用 deep_search 或基于已有信息收口）
	//   · 长度：max_length 被 clamp 到 fetchMaxLength，避免单次抓取灌满上下文
	//   · 提示词：明确「调研走 deep_search，fetch 只用于已知 URL 的原文精读」
	// 约束实现见 fetch_quota.go 与 tool_call_actuator 的执行前拦截。
	entries = append(entries, p.loadDefaultMCPTools(ctx)...)

	// 2. 对话绑定的 MCP 工具
	mcpEntries, err := p.loadMCPTools(ctx, chatID)
	if err != nil {
		return nil, err
	}
	entries = append(entries, mcpEntries...)

	// 3. command 型 skill 工具
	skillConfigs, err := p.LoadSkills(ctx, chatID)
	if err != nil {
		// skill 加载失败不阻断 MCP 工具 —— 记日志降级
		log.Printf("[tool-provider] chat=%s load skills failed: %v", chatID, err)
	} else {
		for _, cfg := range skillConfigs {
			if !cfg.IsCommand() {
				continue
			}
			entries = append(entries, ToolEntry{
				ExposedName: cfg.ToolName(),
				Description: cfg.Description,
				InputSchema: cfg.ToolSchema(),
				Kind:        toolKindSkill,
				Skill:       cfg,
			})
		}
	}

	return newToolSet(entries), nil
}

// resolveModelSupportsImage 判断当前对话所选模型是否支持多模态（图片理解）。
//
// 解析链：conversation.model_id → model_config；model_id 为空（未显式指定）
// 时回落到默认模型（is_default=1）。任一环节查不到都保守返回 false，
// 即「不注入图片理解工具」——宁可让模型告知无法看图，也不误暴露一个
// 实际调不通的能力。
func (p *toolProvider) resolveModelSupportsImage(ctx context.Context, chatID string) bool {
	if p == nil || p.convDAO == nil || p.modelConfigDAO == nil {
		return false
	}
	modelID := ""
	if conv, err := p.convDAO.GetConversationByID(ctx, chatID); err == nil && conv != nil {
		modelID = conv.ModelID
	}
	var cfg *model.ModelConfig
	if modelID != "" {
		if m, err := p.modelConfigDAO.GetByID(ctx, modelID); err == nil {
			cfg = m
		}
	} else {
		if m, err := p.modelConfigDAO.GetDefault(ctx); err == nil {
			cfg = m
		}
	}
	return cfg != nil && cfg.SupportsImage
}

// loadDefaultMCPTools 加载框架级 MCP 工具（如 fetch 精读），仅供 deep_search
// 子任务注入使用（见 applyDeepSearchSubTaskTools）。
//
// 与对话绑定的 MCP 不同：这类工具是框架级能力，加载失败
// （如 uvx/npx 未安装、fetch server 启动超时）只降级记日志、返回空，
// 不阻断整个 agent —— 子任务仍有 web_search + 内置 native 工具可用。
func (p *toolProvider) loadDefaultMCPTools(ctx context.Context) []ToolEntry {
	if p == nil || p.registry == nil || len(p.defaultMCP) == 0 {
		return nil
	}
	registered, err := p.registry.Load(ctx, defaultMCPCacheKey, p.defaultMCP)
	if err != nil {
		log.Printf("[tool-provider] default mcp tools load failed (degrade): %v", err)
		return nil
	}
	entries := make([]ToolEntry, 0, len(registered))
	for i := range registered {
		entries = append(entries, ToolEntry{
			ExposedName: registered[i].ExposedName,
			Description: registered[i].Description,
			InputSchema: registered[i].InputSchema,
			Kind:        toolKindMCP,
			MCP:         &registered[i],
		})
	}
	return entries
}

// loadMCPTools 加载对话绑定且已启用的 MCP server 暴露的工具。
func (p *toolProvider) loadMCPTools(ctx context.Context, chatID string) ([]ToolEntry, error) {
	if p.convMCPDAO == nil || p.mcpServerDAO == nil || p.registry == nil {
		return nil, nil
	}

	// 1. 查对话绑定且已启用的 MCP server
	bindings, err := p.convMCPDAO.ListConversationMCP(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("list conversation mcp: %w", err)
	}
	serverIDs := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if b.Enabled {
			serverIDs = append(serverIDs, b.ServerID)
		}
	}
	if len(serverIDs) == 0 {
		return nil, nil
	}

	// 2. 取 server 配置
	servers, err := p.mcpServerDAO.GetServersByIDs(ctx, serverIDs)
	if err != nil {
		return nil, fmt.Errorf("get mcp servers: %w", err)
	}
	configs := make([]*mcppkg.ServerConfig, 0, len(servers))
	for _, s := range servers {
		cfg, cErr := mcppkg.ConfigFromModel(s)
		if cErr != nil {
			log.Printf("[tool-provider] skip invalid mcp server %s: %v", s.ID, cErr)
			continue
		}
		configs = append(configs, cfg)
	}
	if len(configs) == 0 {
		return nil, nil
	}

	// 3. 通过注册表拉工具（内部按 chatID 缓存）
	registered, err := p.registry.Load(ctx, chatID, configs)
	if err != nil {
		return nil, err
	}

	entries := make([]ToolEntry, 0, len(registered))
	for i := range registered {
		entries = append(entries, ToolEntry{
			ExposedName: registered[i].ExposedName,
			Description: registered[i].Description,
			InputSchema: registered[i].InputSchema,
			Kind:        toolKindMCP,
			MCP:         &registered[i],
		})
	}
	return entries, nil
}

// LoadSkills 加载对话启用（enabled）的 Skill 配置（含 prompt 型与 command 型）。
//
// 返回 nil 表示无可用 skill；调用方据此决定是否注入 system prompt 片段。
func (p *toolProvider) LoadSkills(ctx context.Context, chatID string) ([]*skillpkg.Config, error) {
	if p == nil || p.convSkillDAO == nil || p.skillDAO == nil {
		return nil, nil
	}

	bindings, err := p.convSkillDAO.ListConversationSkill(ctx, chatID)
	if err != nil {
		return nil, fmt.Errorf("list conversation skill: %w", err)
	}
	skillIDs := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if b.Enabled {
			skillIDs = append(skillIDs, b.SkillID)
		}
	}
	if len(skillIDs) == 0 {
		return nil, nil
	}

	skills, err := p.skillDAO.GetSkillsByIDs(ctx, skillIDs)
	if err != nil {
		return nil, fmt.Errorf("get skills: %w", err)
	}

	configs := make([]*skillpkg.Config, 0, len(skills))
	for _, s := range skills {
		cfg, cErr := skillpkg.ConfigFromModel(s)
		if cErr != nil {
			log.Printf("[tool-provider] skip invalid skill %s: %v", s.ID, cErr)
			continue
		}
		// 绝对化 content 里的框架脚本相对路径：skill 的指令（如 pdfkit 的
		// `bash skills/.../setup.sh`）假设 cwd=项目根，但模型常用 run_command
		// （cwd=任务工作区）落地，相对路径找不到。替换成绝对路径后两者皆可用。
		if p.projectRoot != "" {
			cfg.Content = skillpkg.AbsolutizePaths(cfg.Content, p.projectRoot)
		}
		// 社区 SKILL.md 惯用「相对 skill 落地目录」的路径（如 `scripts/calc_plan.py`），
		// AbsolutizePaths 只认 skills/ 前缀，这类路径会原样进 prompt —— 模型解析
		// 不了只能 `find /` 全盘搜，还会先命中 /tmp 里 zip 校验残留的同名文件，
		// 拿错脚本位置。这里定位落地目录，注入根目录说明并绝对化。
		if dir := locateSkillDir(p.projectRoot, cfg.Name); dir != "" {
			cfg.Content = injectSkillDir(cfg.Content, dir)
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

// locateSkillDir 尝试定位 skill 的落地目录（绝对路径）。
//
// zip 导入的社区 skill 落在 skills/user/<name>/，内置导入落在 skills/<name>/；
// DB 记录未存落地目录，用 name 作为 key 近似定位（zip 导入时二者一致）。
// 都找不到返回空串（调用方跳过注入）。
func locateSkillDir(projectRoot, name string) string {
	if projectRoot == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	for _, rel := range []string{
		filepath.Join("skills", "user", name),
		filepath.Join("skills", name),
	} {
		dir := filepath.Join(projectRoot, rel)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir
		}
	}
	return ""
}

// injectSkillDir 在 content 前注入「脚本根目录」说明行，并把 content 里
// 反引号包裹的 skill 目录相对路径（`scripts/...`、`references/...`、
// `assets/...`）替换成绝对路径。只动反引号内的路径引用，不误伤正文文本。
func injectSkillDir(content, dir string) string {
	if strings.TrimSpace(content) == "" || dir == "" {
		return content
	}
	var b strings.Builder
	fmt.Fprintf(&b, "本技能文件根目录：%s\n（下文提到的 scripts/、references/、assets/ 等相对路径均相对此目录）\n\n", dir)
	b.WriteString(content)
	out := b.String()
	for _, prefix := range []string{"scripts/", "references/", "assets/"} {
		out = strings.ReplaceAll(out, "`"+prefix, "`"+dir+string(filepath.Separator)+prefix)
	}
	return out
}

// ============================================================
// 工具集过滤（deep_search 子任务专用）
// ============================================================

// excludeTool 从工具集里移除指定名字的工具，返回一个新工具集（不修改原集）。
//
// 用于 deep_search 子任务：它内部只应使用原生 fetch 抓取，若继续暴露
// deep_search 工具，会让搜索子任务递归嵌套搜索子任务。functional_call 与
// tool_call 两个 actuator 加载工具集后都据此过滤，保证「暴露给 LLM 的名字」
// 与「反查执行时用的名字」一致地不含 deep_search。
func excludeTool(ts *ToolSet, names ...string) *ToolSet {
	if ts == nil || len(names) == 0 {
		return ts
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	entries := make([]ToolEntry, 0, len(ts.tools))
	for _, e := range ts.tools {
		if drop[e.ExposedName] {
			continue
		}
		entries = append(entries, e)
	}
	return newToolSet(entries)
}

// isDeepSearchSubTask 判断任务是否是 deep_search 搜索子任务。
//
// deep_search 子任务与 workflow DAG 节点子任务同为 TaskTypeWorkflowSubTask，
// 靠 TaskInput.SubTaskKind 区分（见 model.SubTaskKindDeepSearch）。只有前者需要
// 隐藏 deep_search 工具 —— DAG 节点子任务仍保留 deep_search，便于节点内做联网调研。
func isDeepSearchSubTask(temp *model.TaskTemp) bool {
	return temp != nil &&
		temp.TaskType == model.TaskTypeWorkflowSubTask &&
		temp.TaskInput.Data != nil &&
		temp.TaskInput.Data.SubTaskKind == model.SubTaskKindDeepSearch
}

// applyDeepSearchSubTaskTools 定制 deep_search 搜索子任务的工具集。
//
// 四件事：
//  1. 隐藏 deep_search 工具 —— 从根上切断「搜索子任务再嵌套搜索子任务」的递归；
//  2. 隐藏 recall_context 工具 —— 搜索子任务是 Isolated 的，压根看不到主对话
//     的历史摘要与那些 task_id / turn_id 句柄，给它取回能力纯是噪声；更要紧的是
//     它自己的历史全是抓回来的网页正文，一旦取回就会把巨量原文搬回上下文，
//     正好违背「用子任务隔离脏上下文」的设计意图。DAG 节点子任务不在此列 ——
//     它需要取回自己被压掉的 turn（见 task_context_summary.compacted_turn_ids）；
//  3. 注入 web_search 工具 —— 子任务专用的联网搜索，按全局检索引擎配置
//     （search_config 表）决定用哪个引擎 + 哪把 key。这里用的是「搜 3 条 +
//     自动抓前 3 条正文」的定制版（见 deepSearchMaxResults /
//     deepSearchAutoFetchTopN），把模型「先读摘要、下轮再逐个 fetch」的往返
//     压缩掉 —— 4 轮预算经不起逐条抓取；
//  4. 注入 fetch 工具 —— 网页抓取（精读），与 web_search 组成
//     「web_search 定位 → fetch 抓正文」的两段式检索。
//
// fetch 只在 deep_search 子任务里暴露：主 agent 与 DAG 节点子任务不直接看到
// fetch，避免模型在主循环里抓网页陷入「多轮抓取 + 上下文爆炸」——它们要做
// 联网调研时走 deep_search 工具即可。
//
// functional_call 与 tool_call 两个 actuator 加载工具集后都调用它，保证
// 「暴露给 LLM 的名字」与「反查执行时用的名字」一致地同时不含 deep_search /
// recall_context、且含 web_search + fetch。
func (p *toolProvider) applyDeepSearchSubTaskTools(ctx context.Context, ts *ToolSet) *ToolSet {
	ts = excludeTool(ts, model.DeepSearchToolName, model.RecallContextToolName)
	ts = injectTool(ts, webSearchToolEntry(p.resolveSearchSpec(ctx)))
	ts = injectTools(ts, p.loadDefaultMCPTools(ctx))
	return ts
}

// resolveSearchSpec 从全局配置解析当前检索引擎与 key，构造 SearchSpec。
//
// 读 search_config 表（单行，主键 global），按当前 engine 取对应 key：
//   - baidu / ddg 零 key，APIKey 留空即可；
//   - bocha / tavily / serper 取各自 key 列。
//
// 任何环节失败（DAO 未注入 / 读库失败）都保守回落 baidu —— 保证
// deep_search 子任务始终有一个零依赖可用的检索引擎，不因配置读取失败而
// 丢掉 web_search 能力。
func (p *toolProvider) resolveSearchSpec(ctx context.Context) workspacepkg.SearchSpec {
	spec := workspacepkg.SearchSpec{Engine: model.SearchEngineBaidu}
	if p == nil || p.searchConfigDAO == nil {
		return spec
	}
	cfg, err := p.searchConfigDAO.GetOrCreate(ctx)
	if err != nil || cfg == nil {
		log.Printf("[tool-provider] resolve search config failed, fallback baidu: %v", err)
		return spec
	}
	spec.Engine = cfg.Engine
	if !spec.Engine.Valid() {
		spec.Engine = model.SearchEngineBaidu
	}
	switch spec.Engine {
	case model.SearchEngineBocha:
		spec.APIKey = cfg.BochaKey
	case model.SearchEngineTavily:
		spec.APIKey = cfg.TavilyKey
	case model.SearchEngineSerper:
		spec.APIKey = cfg.SerperKey
	}
	return spec
}

// injectTool 向工具集追加一个工具，返回一个新工具集（不修改原集）。
//
// 同名工具已存在时跳过：主 agent 的工具集里已带框架级 fetch（见 Load），
// deep_search 子任务又会调 injectTools 注入同一批 —— 不去重就会把两个同名
// fetch 一起发给 LLM，白白撑长工具列表、还可能让模型在重复项里挑错。
func injectTool(ts *ToolSet, entry ToolEntry) *ToolSet {
	if ts == nil {
		return newToolSet([]ToolEntry{entry})
	}
	if _, exists := ts.byName[entry.ExposedName]; exists {
		return ts
	}
	entries := make([]ToolEntry, 0, len(ts.tools)+1)
	entries = append(entries, ts.tools...)
	entries = append(entries, entry)
	return newToolSet(entries)
}

// injectTools 向工具集追加一批工具，返回一个新工具集（不修改原集）。
// 逐个走 injectTool，复用它的同名去重；entries 为空时原样返回 ts。
func injectTools(ts *ToolSet, entries []ToolEntry) *ToolSet {
	if len(entries) == 0 {
		return ts
	}
	out := ts
	for _, e := range entries {
		out = injectTool(out, e)
	}
	return out
}

// deepSearchAutoFetchTopN 搜索子任务里 web_search 自动抓取正文的条数。
//
// 实测（任务 baffb17c）搜索子任务的典型轨迹是「一轮 web_search → 接下来连续
// 数轮把结果逐个 fetch」，8 轮里 7 轮在做抓取。轮次预算砍到 4 之后这种
// 「分批改判」的节奏根本跑不完，因此改为框架在搜索返回时直接抓前 3 条正文，
// 让模型第一轮就有实质内容可读。
const deepSearchAutoFetchTopN = 3

// deepSearchMaxResults 搜索子任务里 web_search 的默认返回条数。
//
// 比默认值 5 少：既然前 3 条会被自动抓取正文，多返回 2 条摘要只会稀释注意力，
// 且搜索子任务的上下文预算要留给后续交叉验证。模型仍可显式传 max_results 放宽。
const deepSearchMaxResults = 3

// webSearchToolEntry 构造 web_search 工具的 ToolEntry（仅 deep_search 子任务注入）。
func webSearchToolEntry(spec workspacepkg.SearchSpec) ToolEntry {
	def := workspacepkg.WebSearchTool(spec, workspacepkg.WebSearchOptions{
		DefaultMaxResults: deepSearchMaxResults,
		AutoFetchTopN:     deepSearchAutoFetchTopN,
	})
	return ToolEntry{
		ExposedName: def.Name,
		Description: def.Description,
		InputSchema: def.InputSchema,
		Kind:        toolKindBuiltin,
		Builtin:     def,
	}
}
