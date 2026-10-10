package model

import "time"

// ============================================================
// SearchEngine — 检索引擎枚举
// ============================================================

// SearchEngine 网页检索（web_search）使用的搜索引擎。
//
// 七种取值：
//   - baidu ：百度 HTML 结果页（www.baidu.com/s），零 key、无第三方依赖，默认值。
//   - ddg   ：DuckDuckGo HTML 结果页（html.duckduckgo.com/html），零 key，但中国大陆不可达
//     （已下线，见 Hidden：不在前端展示、不允许切换、不参与降级链）。
//   - bocha ：博查 BochaAI（api.bochaai.com/v1/web-search），需 Bearer key。
//   - tavily：Tavily Search API（api.tavily.com/search），需 key。
//   - serper：Serper Google Search API（google.serper.dev/search），需 X-API-KEY。
//   - tencent_wsa ：腾讯云联网搜索 API（SearchPro），需服务 API KEY。
//   - baidu_qianfan：百度千帆「百度搜索」API（独立搜索接口），需 API Key。
type SearchEngine string

const (
	SearchEngineBaidu        SearchEngine = "baidu"
	SearchEngineDDG          SearchEngine = "ddg"
	SearchEngineBocha        SearchEngine = "bocha"
	SearchEngineTavily       SearchEngine = "tavily"
	SearchEngineSerper       SearchEngine = "serper"
	SearchEngineTencentWSA   SearchEngine = "tencent_wsa"
	SearchEngineBaiduQianfan SearchEngine = "baidu_qianfan"
)

// Valid 判断引擎取值是否合法。
func (e SearchEngine) Valid() bool {
	switch e {
	case SearchEngineBaidu, SearchEngineDDG, SearchEngineBocha, SearchEngineTavily, SearchEngineSerper,
		SearchEngineTencentWSA, SearchEngineBaiduQianfan:
		return true
	}
	return false
}

// Hidden 判断引擎是否已下线：不出现在前端下拉、不允许切换、不参与降级链。
//
// DuckDuckGo 在中国大陆出口 IP 上稳定返回 202（anomaly 反爬），探活与检索都
// 必然失败且无法在 HTTP 层绕过。把它留在降级链里只会拖慢链路、并打出
// 「已降级为 DuckDuckGo」这类误导日志——百度触发安全验证后再降级到一个必败
// 引擎，等于白等一次超时。因此暂时下线。
//
// 注意 Valid 仍接受 ddg：历史配置里可能已经存着 ddg，读配置时判成非法会直接
// 让整行配置失效。Hidden 只在「选择引擎」的入口生效（前端下拉、切换校验、
// 降级链构造），将来网络环境变化或换了可用出口，去掉本方法的判断即可恢复。
func (e SearchEngine) Hidden() bool {
	return e == SearchEngineDDG
}

// NeedsKey 判断引擎是否需要 API key。
// baidu / ddg 走 HTML 抓取，零 key；其余五家为商业搜索 API，需 key 鉴权。
func (e SearchEngine) NeedsKey() bool {
	switch e {
	case SearchEngineBocha, SearchEngineTavily, SearchEngineSerper, SearchEngineTencentWSA, SearchEngineBaiduQianfan:
		return true
	}
	return false
}

// DisplayName 返回引擎的展示名（前端下拉 / 日志）。
func (e SearchEngine) DisplayName() string {
	switch e {
	case SearchEngineBaidu:
		return "百度"
	case SearchEngineDDG:
		return "DuckDuckGo"
	case SearchEngineBocha:
		return "博查 BochaAI"
	case SearchEngineTavily:
		return "Tavily"
	case SearchEngineSerper:
		return "Serper"
	case SearchEngineTencentWSA:
		return "腾讯云联网搜索"
	case SearchEngineBaiduQianfan:
		return "百度千帆搜索"
	}
	return string(e)
}

// KeyApplyURL 返回引擎 API key 的申请链接；零 key 引擎返回空串。
func (e SearchEngine) KeyApplyURL() string {
	switch e {
	case SearchEngineBocha:
		return "https://open.bochaai.com"
	case SearchEngineTavily:
		return "https://tavily.com"
	case SearchEngineSerper:
		return "https://serper.dev"
	}
	return ""
}

// ============================================================
// SearchConfig — 全局检索引擎配置表（单行）
// ============================================================

// SearchConfigGlobalID 单行配置的固定主键。全表只有一行，靠主键 + OnConflict 防并发双写。
const SearchConfigGlobalID = "global"

// SearchConfig 全局检索引擎配置。
//
// 由前端在「搜索」页切换当前引擎 / 填写各引擎 API key，取代原先写死在
// web_search.go 里的单一 DuckDuckGo 后端。scheduler 侧的 web_search 工具
// 运行时读取本表，决定用哪个引擎 + 哪把 key 做检索。
//
// 为什么单独建 key 列而不是一个共用 api_key：
//   - 用户在博查 / Tavily / Serper 之间切换时，各自的 key 需要独立保留，
//     共用一列会在切换时互相覆盖，导致来回切换要反复重填。
//   - baidu / ddg 零 key，无对应列。
type SearchConfig struct {
	// ID 固定为 "global"，单行配置。
	ID string `gorm:"column:id;size:64;primaryKey" json:"id"`
	// Engine 当前检索引擎，缺省 baidu。
	Engine SearchEngine `gorm:"column:engine;size:20;not null;default:baidu" json:"engine"`
	// 各需 key 引擎的 API key。切换时按需校验非空。
	BochaKey        string    `gorm:"column:bocha_key;size:512" json:"bocha_key,omitempty"`
	TavilyKey       string    `gorm:"column:tavily_key;size:512" json:"tavily_key,omitempty"`
	SerperKey       string    `gorm:"column:serper_key;size:512" json:"serper_key,omitempty"`
	TencentWSAKey   string    `gorm:"column:tencent_wsa_key;size:512" json:"tencent_wsa_key,omitempty"`
	BaiduQianfanKey string    `gorm:"column:baidu_qianfan_key;size:512" json:"baidu_qianfan_key,omitempty"`
	CreatedAt       time.Time `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt       time.Time `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
}

func (SearchConfig) TableName() string { return "search_config" }
