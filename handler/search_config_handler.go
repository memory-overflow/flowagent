package handler

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// ============================================================
// 全局检索引擎配置 —— web_search 使用的搜索引擎与 API key
//
// 取代原先写死在 web_search.go 里的单一 DuckDuckGo 后端，开放到前端
// 「搜索」页切换。默认 baidu（零 key、零依赖，开箱可用）。
//
// 切换约束（服务端强制）：
//   - 已下线引擎（见 SearchEngine.Hidden，当前为 DuckDuckGo）直接拒绝切换；
//   - 切到需 key 的引擎（博查 / Tavily / Serper / 腾讯云联网搜索 / 百度千帆）前
//     校验对应 key 非空，并给申请链接。
// ============================================================

// SearchConfigItem 前端「搜索」页展示的配置项。
type SearchConfigItem struct {
	Engine string `json:"engine"`
	// 各需 key 引擎的 API key（为便于编辑，返回明文，与模型配置页口径一致）。
	BochaKey        string `json:"bocha_key,omitempty"`
	TavilyKey       string `json:"tavily_key,omitempty"`
	SerperKey       string `json:"serper_key,omitempty"`
	TencentWSAKey   string `json:"tencent_wsa_key,omitempty"`
	BaiduQianfanKey string `json:"baidu_qianfan_key,omitempty"`
	UpdateTs        int64  `json:"update_ts"`
}

// UpdateSearchConfigRequest 切换引擎 / 更新 key。
// 各 key 字段「非空才更新」，空串表示不改动（与模型配置的 update 口径一致）。
type UpdateSearchConfigRequest struct {
	Engine          string `json:"engine"`
	BochaKey        string `json:"bocha_key,omitempty"`
	TavilyKey       string `json:"tavily_key,omitempty"`
	SerperKey       string `json:"serper_key,omitempty"`
	TencentWSAKey   string `json:"tencent_wsa_key,omitempty"`
	BaiduQianfanKey string `json:"baidu_qianfan_key,omitempty"`
}

// TestSearchRequest 检索连通性测试：用指定引擎跑一次真实搜索。
// 需 key 引擎若未在请求里带 key，则回落到 DB 里已保存的 key。
type TestSearchRequest struct {
	Engine string `json:"engine"`
	Query  string `json:"query"`
	// 直连测试时可临时覆盖 key；留空则用 DB 已存 key。
	BochaKey        string `json:"bocha_key,omitempty"`
	TavilyKey       string `json:"tavily_key,omitempty"`
	SerperKey       string `json:"serper_key,omitempty"`
	TencentWSAKey   string `json:"tencent_wsa_key,omitempty"`
	BaiduQianfanKey string `json:"baidu_qianfan_key,omitempty"`
}

// TestSearchResponse 检索连通性测试结果。
type TestSearchResponse struct {
	Success  bool   `json:"success"`
	ErrorMsg string `json:"error_msg,omitempty"`
	// Results 真实搜索返回的文本（给用户看）。
	Results string `json:"results,omitempty"`
}

// GetSearchConfig 返回当前检索引擎配置（不存在则初始化默认 baidu）。
func GetSearchConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	cfg, err := (&dao.SearchConfigDAO{}).GetOrCreate(ctx)
	if err != nil {
		return nil, err
	}
	return buildSearchConfigItem(cfg), nil
}

func GetSearchConfigBinder() interface{} { return &struct{}{} }

// UpdateSearchConfig 切换检索引擎 / 更新 key。
//
// 服务端强校验：
//  1. engine 取值合法；
//  2. 已下线引擎（Hidden，当前为 DuckDuckGo）直接拒绝，不再探活（探活也必败）；
//  3. 切需 key 引擎前校验「请求里带的新 key 或 DB 里已存的 key」非空，否则拒绝并给申请链接。
func UpdateSearchConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateSearchConfigRequest)
	engine := model.SearchEngine(r.Engine)
	if !engine.Valid() {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "engine 必须为 baidu / ddg / bocha / tavily / serper / tencent_wsa / baidu_qianfan"}
	}
	// 已下线引擎直接拒绝：切过去必然检索失败，留着只会让用户白配一次。
	if engine.Hidden() {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: engine.DisplayName() + " 已下线（中国大陆出口 IP 会被判定为机器人，探活与检索均返回 202），请改用百度或博查 / Tavily / Serper / 腾讯云联网搜索 / 百度千帆"}
	}

	searchDAO := &dao.SearchConfigDAO{}
	existing, err := searchDAO.GetOrCreate(ctx)
	if err != nil {
		return nil, err
	}

	// 切需 key 引擎：校验目标引擎 key 非空（请求新值优先，否则 DB 已存值）。
	if engine.NeedsKey() {
		key := effectiveKey(r, existing, engine)
		if strings.TrimSpace(key) == "" {
			return nil, &APIError{
				Code: http.StatusBadRequest,
				Msg:  "切换 " + engine.DisplayName() + " 需要先填写 API Key（申请地址：" + engine.KeyApplyURL() + "）",
			}
		}
	}

	updates := map[string]interface{}{"engine": engine}
	if r.BochaKey != "" {
		updates["bocha_key"] = r.BochaKey
	}
	if r.TavilyKey != "" {
		updates["tavily_key"] = r.TavilyKey
	}
	if r.SerperKey != "" {
		updates["serper_key"] = r.SerperKey
	}
	if r.TencentWSAKey != "" {
		updates["tencent_wsa_key"] = r.TencentWSAKey
	}
	if r.BaiduQianfanKey != "" {
		updates["baidu_qianfan_key"] = r.BaiduQianfanKey
	}

	if err := searchDAO.Update(ctx, updates); err != nil {
		return nil, err
	}

	updated, _ := searchDAO.Get(ctx)
	if updated != nil {
		return buildSearchConfigItem(updated), nil
	}
	return nil, nil
}

func UpdateSearchConfigBinder() interface{} { return &UpdateSearchConfigRequest{} }

// TestSearch 用指定引擎跑一次真实搜索，验证 key / 网络是否可用。
func TestSearch(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*TestSearchRequest)
	engine := model.SearchEngine(r.Engine)
	if !engine.Valid() {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "engine 必须为 baidu / ddg / bocha / tavily / serper / tencent_wsa / baidu_qianfan"}
	}
	if engine.Hidden() {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: engine.DisplayName() + " 已下线，不支持连通性测试"}
	}
	query := strings.TrimSpace(r.Query)
	if query == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "query 不能为空"}
	}

	spec, err := resolveSearchSpec(ctx, engine, r.BochaKey, r.TavilyKey, r.SerperKey, r.TencentWSAKey, r.BaiduQianfanKey)
	if err != nil {
		return &TestSearchResponse{Success: false, ErrorMsg: err.Error()}, nil
	}

	testCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	results, err := workspacepkg.SearchWebOnce(testCtx, spec, query, workspacepkg.DefaultSearchMaxResults())
	if err != nil {
		return &TestSearchResponse{Success: false, ErrorMsg: err.Error()}, nil
	}
	return &TestSearchResponse{Success: true, Results: results}, nil
}

func TestSearchBinder() interface{} { return &TestSearchRequest{} }

// ============================================================
// 辅助函数
// ============================================================

func buildSearchConfigItem(cfg *model.SearchConfig) *SearchConfigItem {
	if cfg == nil {
		return &SearchConfigItem{Engine: string(model.SearchEngineBaidu)}
	}
	return &SearchConfigItem{
		Engine:          string(cfg.Engine),
		BochaKey:        cfg.BochaKey,
		TavilyKey:       cfg.TavilyKey,
		SerperKey:       cfg.SerperKey,
		TencentWSAKey:   cfg.TencentWSAKey,
		BaiduQianfanKey: cfg.BaiduQianfanKey,
		UpdateTs:        cfg.UpdatedAt.UnixMilli(),
	}
}

// effectiveKey 取某需 key 引擎的「生效 key」：请求里带的新值优先，否则 DB 已存值。
func effectiveKey(r *UpdateSearchConfigRequest, existing *model.SearchConfig, engine model.SearchEngine) string {
	var reqKey, dbKey string
	switch engine {
	case model.SearchEngineBocha:
		reqKey, dbKey = r.BochaKey, existing.BochaKey
	case model.SearchEngineTavily:
		reqKey, dbKey = r.TavilyKey, existing.TavilyKey
	case model.SearchEngineSerper:
		reqKey, dbKey = r.SerperKey, existing.SerperKey
	case model.SearchEngineTencentWSA:
		reqKey, dbKey = r.TencentWSAKey, existing.TencentWSAKey
	case model.SearchEngineBaiduQianfan:
		reqKey, dbKey = r.BaiduQianfanKey, existing.BaiduQianfanKey
	}
	if strings.TrimSpace(reqKey) != "" {
		return reqKey
	}
	return dbKey
}

// resolveSearchSpec 组装一次搜索所用的 SearchSpec：请求里带的 key 优先，否则 DB 已存 key。
func resolveSearchSpec(ctx context.Context, engine model.SearchEngine, bochaKey, tavilyKey, serperKey, tencentWSAKey, baiduQianfanKey string) (workspacepkg.SearchSpec, error) {
	spec := workspacepkg.SearchSpec{Engine: engine}
	key := ""
	switch engine {
	case model.SearchEngineBocha:
		key = bochaKey
	case model.SearchEngineTavily:
		key = tavilyKey
	case model.SearchEngineSerper:
		key = serperKey
	case model.SearchEngineTencentWSA:
		key = tencentWSAKey
	case model.SearchEngineBaiduQianfan:
		key = baiduQianfanKey
	}

	// 需 key 引擎：请求未带 key 时回落到 DB。
	if engine.NeedsKey() && strings.TrimSpace(key) == "" {
		cfg, err := (&dao.SearchConfigDAO{}).GetOrCreate(ctx)
		if err != nil {
			return spec, err
		}
		switch engine {
		case model.SearchEngineBocha:
			key = cfg.BochaKey
		case model.SearchEngineTavily:
			key = cfg.TavilyKey
		case model.SearchEngineSerper:
			key = cfg.SerperKey
		case model.SearchEngineTencentWSA:
			key = cfg.TencentWSAKey
		case model.SearchEngineBaiduQianfan:
			key = cfg.BaiduQianfanKey
		}
	}
	spec.APIKey = key
	return spec, nil
}
