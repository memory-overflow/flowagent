package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/memory-overflow/flowagent/model"
)

func TestHasRelevantResults(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		results []webSearchResult
		want    bool
	}{
		{
			name:  "深圳天气命中正常结果",
			query: "深圳市 2026年9月10日 天气预报 气温",
			results: []webSearchResult{
				{Title: "深圳天气预报_天气网", Snippet: "深圳未来一周天气预报"},
				{Title: "深圳市气象局", Snippet: "今日天气实况"},
			},
			want: true,
		},
		{
			name:  "深圳天气返回韩国NAVER垃圾",
			query: "深圳市 2026年9月10日 天气预报 气温",
			results: []webSearchResult{
				{Title: "NAVER", Snippet: "네이버는 다양한 정보를 제공하는 포털 사이트입니다."},
				{Title: "네이버지도", Snippet: "네이버 지도는 다양한 위치 검색을 제공합니다."},
			},
			want: false,
		},
		{
			name:  "深圳天气返回印度扭矩扳手垃圾",
			query: "深圳 9月10日 天气 历史 2026",
			results: []webSearchResult{
				{Title: "Torque Wrench - Adjustable Torque Wrench", Snippet: "Find here Torque Wrench manufacturers in India."},
			},
			want: false,
		},
		{
			name:  "深圳天气返回中文但无关内容",
			query: "深圳天气 2026年9月10日 预报",
			results: []webSearchResult{
				{Title: "我的世界指令教学：音效大全", Snippet: "我的世界音效指令大全"},
				{Title: "中巨芯科技股份有限公司", Snippet: "专注于半导体制造支撑业"},
			},
			want: false,
		},
		{
			name:  "纯英文查询跳过校验",
			query: "golang slice append performance",
			results: []webSearchResult{
				{Title: "Go Slices: usage and internals", Snippet: "The Go Blog"},
			},
			want: true,
		},
		{
			name:  "单字查询无法提取关键词跳过校验",
			query: "雨",
			results: []webSearchResult{
				{Title: "Weather", Snippet: "rain forecast"},
			},
			want: true,
		},
		{
			name:  "关键词分散在不同结果也应通过",
			query: "和平精英 皮肤",
			results: []webSearchResult{
				{Title: "和平精英官网", Snippet: "腾讯光子工作室群"},
				{Title: "皮肤图鉴", Snippet: "全皮肤一览"},
			},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hasRelevantResults(c.query, c.results)
			if got != c.want {
				t.Errorf("hasRelevantResults(%q) = %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestExtractChineseKeywords(t *testing.T) {
	got := extractChineseKeywords("深圳市 2026年9月10日 天气预报 气温")
	// 期望包含实义词「深圳」「天气」「预报」「气温」，不含日期片段与停用词。
	want := map[string]bool{"深圳": true, "天气": true, "预报": true, "气温": true}
	for kw := range want {
		found := false
		for _, k := range got {
			if k == kw {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("关键词 %q 未被提取到，got=%v", kw, got)
		}
	}
	// 日期片段不应作为关键词。
	for _, k := range got {
		for _, r := range k {
			if r == '年' || r == '月' || r == '日' {
				t.Errorf("日期片段 %q 不应成为关键词", k)
			}
		}
	}
}

func TestParseBaiduResults(t *testing.T) {
	// 还原真实百度结果页 DOM 结构：
	//   - recommend_list / board 是推荐位/热搜榜，无 h3 标题，应被过滤；
	//   - result c-container 是自然结果，标题在 h3>a，真实 URL 内联在 mu 属性；
	//   - result-op 是百度自有产品（百科），无 mu，URL 退回 h3>a 的 href。
	htmlDoc := `<!DOCTYPE html><html><body>
<div class="recommend_list"><div class="board"><a>松坪山热门推荐</a></div></div>
<div class="result c-container xpath-log" mu="https://www.example.com/1"><h3 class="t"><a href="https://www.baidu.com/link?url=abc">松坪山三期小区简介</a></h3><div class="c-abstract">松坪山三期位于南山区西丽……</div></div>
<div class="result c-container" mu="https://www.example.com/2"><h3><a href="https://www.baidu.com/link?url=def">松坪山三期户型图</a></h3><div class="c-abstract">户型图与参考价格……</div></div>
<div class="result-op c-container xpath-log"><h3><a href="https://baike.baidu.com/item/松坪山">松坪山（百度百科）</a></h3><div class="c-abstract">松坪山，位于深圳市南山区……</div></div>
</body></html>`

	results := parseBaiduResults([]byte(htmlDoc), 5)

	if len(results) != 3 {
		t.Fatalf("解析结果数 = %d, want 3（推荐位应被过滤）", len(results))
	}

	// 第 1 条：自然结果，标题 + mu 属性真实 URL + 摘要。
	if results[0].Title != "松坪山三期小区简介" {
		t.Errorf("results[0].Title = %q", results[0].Title)
	}
	if results[0].URL != "https://www.example.com/1" {
		t.Errorf("results[0].URL = %q, want mu 属性内联的真实 URL", results[0].URL)
	}
	if results[0].Snippet == "" {
		t.Errorf("results[0].Snippet 不应为空")
	}

	// 第 3 条：result-op（百科），无 mu，URL 保持 href 原始跳转/直链。
	if results[2].Title == "" {
		t.Errorf("result-op 标题应被解析")
	}
	if results[2].URL != "https://baike.baidu.com/item/松坪山" {
		t.Errorf("results[2].URL = %q", results[2].URL)
	}
}

func TestParseBaiduResultsMaxResults(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><body>`)
	for i := 0; i < 5; i++ {
		b.WriteString(`<div class="result c-container" mu="https://example.com/`)
		b.WriteString(fmt.Sprintf("%d", i))
		b.WriteString(`"><h3><a href="https://www.baidu.com/link?url=x">标题`)
		b.WriteString(fmt.Sprintf("%d", i))
		b.WriteString(`</a></h3></div>`)
	}
	b.WriteString(`</body></html>`)

	results := parseBaiduResults([]byte(b.String()), 2)
	if len(results) != 2 {
		t.Fatalf("maxResults=2 但解析出 %d 条", len(results))
	}
	if results[0].URL != "https://example.com/0" || results[1].URL != "https://example.com/1" {
		t.Errorf("截断后结果顺序/URL 不符: %v", results)
	}
}

func TestBuildSearchFallbackChain(t *testing.T) {
	cases := []struct {
		engine model.SearchEngine
		want   []model.SearchEngine
	}{
		// 百度是链条终点：配置引擎就是百度时，链只有它自己，失败即返回错误。
		{model.SearchEngineBaidu, []model.SearchEngine{model.SearchEngineBaidu}},
		// 已下线引擎（ddg）整体回落到百度，不再进链。
		{model.SearchEngineDDG, []model.SearchEngine{model.SearchEngineBaidu}},
		// 其余引擎失败后退到百度即止，不再继续往下退。
		{model.SearchEngineBocha, []model.SearchEngine{model.SearchEngineBocha, model.SearchEngineBaidu}},
		{model.SearchEngineTavily, []model.SearchEngine{model.SearchEngineTavily, model.SearchEngineBaidu}},
		{model.SearchEngineSerper, []model.SearchEngine{model.SearchEngineSerper, model.SearchEngineBaidu}},
		{model.SearchEngineTencentWSA, []model.SearchEngine{model.SearchEngineTencentWSA, model.SearchEngineBaidu}},
		{model.SearchEngineBaiduQianfan, []model.SearchEngine{model.SearchEngineBaiduQianfan, model.SearchEngineBaidu}},
		{model.SearchEngine("invalid"), []model.SearchEngine{model.SearchEngineBaidu}},
	}

	for _, c := range cases {
		got := buildSearchFallbackChain(c.engine)
		if len(got) != len(c.want) {
			t.Errorf("buildSearchFallbackChain(%q) 长度 = %d, want %d", c.engine, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("buildSearchFallbackChain(%q)[%d] = %q, want %q", c.engine, i, got[i], c.want[i])
			}
		}
	}
}

// searchChain 全链失败时，错误信息必须包含链上每个引擎的失败原因（链首在前），
// 而不是只返回最后一个引擎（兜底百度）的错 —— 否则「配置引擎为什么降级」无从排查。
func TestSearchChainAllFailAggregatesErrors(t *testing.T) {
	chain := []model.SearchEngine{model.SearchEngineSerper, model.SearchEngineBaidu}
	spec := SearchSpec{Engine: model.SearchEngineSerper, APIKey: "k"}
	fake := func(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error) {
		if spec.Engine == model.SearchEngineSerper {
			return nil, fmt.Errorf("web_search: 请求 Serper 失败: 状态码 403: invalid key")
		}
		return nil, fmt.Errorf("web_search: 百度触发安全验证（请求过频，请稍后重试）")
	}

	_, err := searchChain(context.Background(), chain, spec, "workflow engine", 5, fake)
	if err == nil {
		t.Fatal("全链失败应返回错误")
	}
	msg := err.Error()
	for _, want := range []string{"Serper", "invalid key", "百度触发安全验证", "所有检索引擎均失败"} {
		if !strings.Contains(msg, want) {
			t.Errorf("聚合错误缺少 %q: %s", want, msg)
		}
	}
	// 链首（配置引擎 Serper）的错误必须排在兜底（百度）之前。
	if strings.Index(msg, "Serper") > strings.Index(msg, "百度触发") {
		t.Errorf("链首错误应在前: %s", msg)
	}
	// 各引擎错误去掉了重复的 "web_search: " 前缀，只保留最外层一个。
	if strings.Count(msg, "web_search:") != 1 {
		t.Errorf("前缀应只出现一次: %s", msg)
	}
}

// 链首成功：直接返回结果，不走兜底。
func TestSearchChainFirstEngineSuccess(t *testing.T) {
	chain := []model.SearchEngine{model.SearchEngineSerper, model.SearchEngineBaidu}
	spec := SearchSpec{Engine: model.SearchEngineSerper, APIKey: "k"}
	fake := func(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error) {
		if spec.Engine != model.SearchEngineSerper {
			t.Errorf("链首成功后不应再调兜底引擎，实际调了 %v", spec.Engine)
		}
		return []webSearchResult{{Title: "t", URL: "https://example.com"}}, nil
	}

	results, err := searchChain(context.Background(), chain, spec, "q", 5, fake)
	if err != nil {
		t.Fatalf("链首成功不应报错: %v", err)
	}
	if len(results) != 1 || results[0].Title != "t" {
		t.Errorf("结果不符: %v", results)
	}
}

// 链首失败、兜底成功：返回兜底结果。
func TestSearchChainFallbackSuccess(t *testing.T) {
	chain := []model.SearchEngine{model.SearchEngineSerper, model.SearchEngineBaidu}
	spec := SearchSpec{Engine: model.SearchEngineSerper, APIKey: "k"}
	fake := func(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error) {
		if spec.Engine == model.SearchEngineSerper {
			return nil, fmt.Errorf("web_search: 请求 Serper 失败: 状态码 401")
		}
		return []webSearchResult{{Title: "baidu", URL: "https://example.com"}}, nil
	}

	results, err := searchChain(context.Background(), chain, spec, "q", 5, fake)
	if err != nil {
		t.Fatalf("兜底成功不应报错: %v", err)
	}
	if len(results) != 1 || results[0].Title != "baidu" {
		t.Errorf("应返回兜底引擎结果: %v", results)
	}
}
