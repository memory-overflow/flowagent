// Package workspace — web_search 内置联网搜索工具（多引擎 provider）。
//
// web_search 与 web_fetch（走 MCP mcp-server-fetch）分工不同：
//   - web_fetch 负责「精读」：把某个具体 URL 抓下来转 Markdown，但它内部会遵守
//     robots.txt（urllib.robotparser），搜索引擎结果页（Google/百度）恰恰禁爬，
//     导致「用 fetch 模拟搜索」这条路经常被 robots 拦下；
//   - web_search 负责「发现」：用 Go 原生 HTTP 请求搜索引擎结果页 / 搜索 API，
//     解析出标题 / URL / 摘要片段。Go 的 http.Client 默认不读 robots.txt，因此
//     绕开了 fetch 的 robots 拦截，专门用来「先定位候选链接」。
//
// 引擎可插拔（由全局配置 search_config 表决定，见 model.SearchEngine）：
//   - baidu ：百度 HTML 结果页，零 key、无第三方依赖（默认，保证开箱可用）。
//   - ddg   ：DuckDuckGo HTML 结果页，零 key，但中国大陆不可达（切换前服务端探活）。
//   - bocha / tavily / serper：商业搜索 API，需各自 API key。
//
// 本工具不进 BuiltinTools()：它只在 deep_search 搜索子任务内部注入（与
// analyze_image 的条件注入模式一致），主对话不直接暴露，保持对外工具面更小。
// 搜索子任务的标准检索链路是「web_search 定位 → fetch 精读 → 交叉综合」。
package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/memory-overflow/flowagent/model"
	"golang.org/x/net/html"
)

const (
	// webSearchName 暴露给 LLM 的工具名。
	webSearchName = "web_search"
	// webSearchDefaultMaxResults 默认返回结果条数。
	webSearchDefaultMaxResults = 5
	// webSearchMaxResultsLimit 单次返回条数上限，防止结果太多撑爆上下文。
	webSearchMaxResultsLimit = 10
	// webSearchTimeout 单次搜索的超时（搜索 API / HTML 结果页应秒级返回）。
	webSearchTimeout = 30 * time.Second
	// webSearchAutoFetchMaxChars 自动抓取时单页正文的字符上限。
	//
	// 定这个数的依据：搜索子任务只有 4 轮预算、且结论要回传父任务，正文太长
	// 会挤掉后续交叉验证与总结的空间。1500 字（约 1k token）足够覆盖一篇
	// 普通网页的核心信息，3 条加起来约 3k token，属可接受水位。
	webSearchAutoFetchMaxChars = 1500
	// webSearchAutoFetchTimeout 自动抓取单页的超时。
	//
	// 比搜索本身的超时短：抓取是「顺带做的增强」，某个站点慢不应拖垮整次搜索，
	// 抓不到就退回摘要，模型仍可自己再 fetch。
	webSearchAutoFetchTimeout = 20 * time.Second
	// searchMaxReadBytes 响应体最大读取量（约 1MB），防止异常响应撑爆内存。
	searchMaxReadBytes = 1 << 20
	// searchUserAgent 浏览器 UA：搜索引擎对无 UA / 默认 Go UA 的请求可能拒绝或返回异常页。
	searchUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

	// baiduHomeURL 百度首页，用于预热获取 BAIDUID cookie（无 cookie 的裸请求
	// 连续 2-3 次就会被百度安全验证拦截，必须先访问首页拿 cookie）。
	baiduHomeURL = "https://www.baidu.com/"
	// baiduHTMLBase 百度 HTML 结果页地址（www.baidu.com，零 key、大陆可达）。
	// 中文长尾召回质量远好于 Bing/DDG，是零依赖场景的中文主力引擎。
	baiduHTMLBase = "https://www.baidu.com/s"
	// ddgHTMLBase DuckDuckGo HTML 版结果页地址（静态、无需 JS、无需 API key）。
	ddgHTMLBase = "https://html.duckduckgo.com/html/"

	// 商业搜索 API 端点。
	bochaAPIBase  = "https://api.bochaai.com/v1/web-search"
	tavilyAPIBase = "https://api.tavily.com/search"
	serperAPIBase = "https://google.serper.dev/search"

	// 国内云厂商联网搜索 API（独立搜索接口，直接返回结果，非模型绑定）。
	// 腾讯云 WSA SearchPro：Authorization: Bearer <服务 API KEY>。
	tencentWSAAPIBase = "https://api.wsa.cloud.tencent.com/SearchPro"
	// 百度千帆「百度搜索」：Authorization: Bearer <API Key>（直接鉴权，无需 access_token 交换）。
	baiduQianfanAPIBase = "https://qianfan.baidubce.com/v2/ai_search/web_search"
)

// SearchSpec 一次搜索所用的引擎与凭据。
//
// 由 tool_provider 运行时从 search_config 表解析；APIKey 仅需 key 引擎
// （bocha/tavily/serper）使用，baidu/ddg 忽略。
type SearchSpec struct {
	Engine model.SearchEngine
	APIKey string
}

// DefaultSearchMaxResults 返回单次搜索默认结果条数（供测试端点复用）。
func DefaultSearchMaxResults() int { return webSearchDefaultMaxResults }

// WebSearchOptions web_search 工具的定制项（deep_search 子任务专用）。
//
// 搜索子任务与主 agent 用的是同一个工具实现，但两者的上下文预算完全不同：
// 子任务只有 4 轮、且结论要回传给父任务，因此需要「少返回几条 + 直接附正文」；
// 主 agent 不暴露 fetch，只能看摘要，条数可以多一些。
type WebSearchOptions struct {
	// DefaultMaxResults 模型未显式指定时返回的结果条数。<=0 用
	// webSearchDefaultMaxResults（5）。
	DefaultMaxResults int
	// AutoFetchTopN 搜索后自动抓取前 N 条结果正文的条数。<=0 表示不自动抓取
	// （退化为只返回摘要）。需要 Context.PageFetcher 已注入才真正生效。
	AutoFetchTopN int
	// AutoFetchMaxChars 自动抓取时单页正文的字符上限。<=0 用
	// webSearchAutoFetchMaxChars。
	AutoFetchMaxChars int
}

// WebSearchTool 返回联网搜索工具定义（按 spec 指定的引擎构造）。
//
// 搜索本身不依赖 Context 里的任何回调（VisionAnalyzer / DeepSearchRunner），
// 工具直接用 net/http 抓取解析，因此可在 deep_search 子任务里独立工作。
//
// 唯一可选的回调依赖是 Context.PageFetcher：注入后会在返回结果里附带前
// AutoFetchTopN 条结果的正文（见 fetchTopPages）。这是给搜索子任务做的优化 ——
// 实测模型拿到摘要后几乎总是把全部结果再 fetch 一遍，与其让它多花一轮往返，
// 不如框架直接抓回来。回调为 nil 时行为与之前完全一致。
func WebSearchTool(spec SearchSpec, opts WebSearchOptions) *ToolDef {
	engine := spec.Engine
	if !engine.Valid() {
		engine = model.SearchEngineBaidu
	}
	defaultMaxResults := opts.DefaultMaxResults
	if defaultMaxResults <= 0 {
		defaultMaxResults = webSearchDefaultMaxResults
	}
	autoFetchMaxChars := opts.AutoFetchMaxChars
	if autoFetchMaxChars <= 0 {
		autoFetchMaxChars = webSearchAutoFetchMaxChars
	}
	autoFetchTimeout := time.Duration(0)
	if opts.AutoFetchTopN > 0 {
		autoFetchTimeout = webSearchAutoFetchTimeout
	}
	desc := fmt.Sprintf("联网搜索：用 %s 搜索关键词，返回相关网页的标题、URL 与摘要片段。"+
		"一次返回有限条结果，必要时可换关键词或加 site: 等限定词重搜。",
		engine.DisplayName())
	if opts.AutoFetchTopN > 0 {
		desc += fmt.Sprintf(" 返回值已自动附带前 %d 条结果的正文（已截断），可直接引用，"+
			"不要对这几条重复调用 fetch；确需精读剩余条目时再用 fetch。", opts.AutoFetchTopN)
	}
	return &ToolDef{
		Name:        webSearchName,
		Description: desc,
		InputSchema: objectSchema(map[string]interface{}{
			"query": strProp("搜索关键词或短语（可用 site:、\"精确短语\" 等限定词）"),
			"max_results": map[string]interface{}{
				"type":        "integer",
				"description": fmt.Sprintf("（可选）返回结果条数，默认 %d，上限 %d", defaultMaxResults, webSearchMaxResultsLimit),
			},
		}, []string{"query"}),
		// 超时要给足「搜索 + 自动抓取」两段：搜索本身 30s，抓取再叠加一段。
		// 否则开启自动抓取后，慢站点会把一次本可成功的搜索拖成整工具超时 ——
		// 抓取只是增强，不该反过来拖垮搜索。
		Timeout: webSearchTimeout + autoFetchTimeout,
		// Label 覆盖 turn 记录里的 server_name：web_search 的「来源」是具体检索引擎
		// （百度 / 博查 / Serper 等），而非笼统的 builtin，便于调试页一眼看出用了哪个引擎。
		Label: engine.DisplayName(),
		Handler: func(ctx context.Context, wc *Context, args map[string]interface{}) (string, error) {
			query := strArg(args, "query")
			if strings.TrimSpace(query) == "" {
				return "", fmt.Errorf("web_search 缺少 query 参数")
			}
			maxResults := defaultMaxResults
			if v, ok := args["max_results"]; ok {
				if n, ok2 := intArg(v); ok2 && n > 0 {
					if n > webSearchMaxResultsLimit {
						n = webSearchMaxResultsLimit
					}
					maxResults = n
				}
			}

			// 走 searchWithFallback 而非 SearchWeb：自动抓取需要结构化的
			// []webSearchResult（要拿每条的 URL），SearchWeb 只回格式化文本。
			results, err := searchWithFallback(ctx, spec, query, maxResults)
			if err != nil {
				return "", err
			}
			text := formatWebSearchResults(query, results)
			if opts.AutoFetchTopN <= 0 || wc == nil || wc.PageFetcher == nil {
				return text, nil
			}
			return text + fetchTopPages(ctx, wc.PageFetcher, results, opts.AutoFetchTopN, autoFetchMaxChars), nil
		},
	}
}

// fetchTopPages 并发抓取前 topN 条结果的正文，拼成附加文本。
//
// 设计取舍：
//   - 并发：抓 N 个页面的耗时取决于最慢那个，串行会明显拖长单次 web_search；
//   - 单条独立超时：某个站点挂住不能拖垮整次搜索；
//   - 失败静默跳过：robots 拦截 / 404 / 超时在网页抓取里是常态（fetch 工具本身
//     就经常被拦），把错误体塞进结果只会污染上下文，还让模型误以为「这个
//     来源不可用」；跳过即可，模型仍可自己再试；
//   - 每条截断：正文长度不可控，不截断的话一次搜索就能吃满上下文窗口。
//
// 返回空串表示一条都没抓到（此时不额外追加内容，模型看到的就是纯摘要）。
func fetchTopPages(
	ctx context.Context, fetcher PageFetcherFunc, results []webSearchResult, topN, maxChars int,
) string {
	if fetcher == nil || topN <= 0 || len(results) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = webSearchAutoFetchMaxChars
	}
	n := topN
	if n > len(results) {
		n = len(results)
	}

	type page struct {
		title string
		url   string
		body  string
	}
	pages := make([]page, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if results[i].URL == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, webSearchAutoFetchTimeout)
			defer cancel()
			body, err := fetcher(callCtx, results[i].URL, maxChars)
			if err != nil {
				log.Printf("[web-search] auto fetch page failed (skip): url=%s err=%v", results[i].URL, err)
				return
			}
			body = truncateRunes(strings.TrimSpace(body), maxChars)
			if body == "" {
				return
			}
			pages[i] = page{title: results[i].Title, url: results[i].URL, body: body}
		}(i)
	}
	wg.Wait()

	var b strings.Builder
	written := 0
	for i, p := range pages {
		if p.body == "" {
			continue
		}
		written++
		fmt.Fprintf(&b, "\n[正文 %d] %s\n%s\n", i+1, p.title, p.body)
	}
	if written == 0 {
		return ""
	}
	head := fmt.Sprintf("\n（以下为前 %d 条结果中已抓取到的 %d 条正文，已截断至约 %d 字）\n", n, written, maxChars)
	tail := ""
	if remain := len(results) - n; remain > 0 {
		// 明确告诉模型「还有 N 条没抓」：否则它会以为搜索结果只有这几条，
		// 从而漏掉可能更相关的剩余来源。
		tail = fmt.Sprintf("\n（另有 %d 条结果仅返回摘要，需要精读可用 fetch 工具抓取）\n", remain)
	}
	return head + b.String() + tail
}

// SearchWeb 按 spec 指定的引擎执行一次搜索，返回格式化后的文本结果。
//
// 带降级兜底：配置引擎失败时依次退化为百度 → DuckDuckGo（百度零 key、
// 大陆可达、中文召回好，是最终兜底），保证 deep_search 子任务在任何网络 /
// key 异常下都有结果可用。供 web_search 工具调用。
func SearchWeb(ctx context.Context, spec SearchSpec, query string, maxResults int) (string, error) {
	results, err := searchWithFallback(ctx, spec, query, maxResults)
	if err != nil {
		return "", err
	}
	return formatWebSearchResults(query, results), nil
}

// SearchWebOnce 用指定引擎严格执行一次搜索（不降级）。
//
// 供「搜索配置」页的测试端点使用：测试要如实反映「这个引擎 + key 能不能用」，
// 若走降级会把失败引擎的错漏报成成功，混淆用户判断。
func SearchWebOnce(ctx context.Context, spec SearchSpec, query string, maxResults int) (string, error) {
	results, err := search(ctx, spec, query, maxResults)
	if err != nil {
		return "", err
	}
	if !hasRelevantResults(query, results) {
		return "", fmt.Errorf("返回 %d 条结果均与查询无关（疑似市场误判或召回失败）", len(results))
	}
	return formatWebSearchResults(query, results), nil
}

// CheckEngineReachable 探测某引擎在当前网络是否可达，返回 nil 表示可达。
//
// 用于切换检索引擎前的服务端校验：目前只有 DuckDuckGo 在中国大陆不可达，
// 需要先探活再放行；其余引擎（baidu / 商业 API）可达性由用户后续「测试」自证，
// 不在切换时强校验（避免给无 key 的初次配置增加额外网络往返）。
func CheckEngineReachable(ctx context.Context, engine model.SearchEngine) error {
	if engine != model.SearchEngineDDG {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ddgHTMLBase, nil)
	if err != nil {
		return fmt.Errorf("构造探活请求失败: %w", err)
	}
	req.Header.Set("User-Agent", searchUserAgent)
	client := &http.Client{Timeout: webSearchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("DuckDuckGo 网络不可达（中国大陆一般无法访问）: %w", err)
	}
	defer resp.Body.Close()
	// 202 是 DDG 的 anomaly 反爬挑战：不是网络不可达，而是出口 IP 被判定为机器人。
	// 探活与搜索都会吃 202，且改请求头/方法都无法绕过，单独提示并引导换引擎。
	if resp.StatusCode == http.StatusAccepted {
		return fmt.Errorf("DuckDuckGo 返回 202（anomaly 反爬验证）：当前出口 IP 被判定为机器人，切换后搜索也会失败，建议改用百度或博查/Tavily/Serper 等商业搜索 API")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DuckDuckGo 返回状态码 %d，判定不可达", resp.StatusCode)
	}
	return nil
}

// searchWithFallback 按降级链执行搜索：配置引擎失败 → 百度（终点）。
//
// 降级只在「返回 error」时发生（含请求失败、非 200、解析出 0 结果、key 缺失等），
// 一旦某个引擎成功即返回，不再继续。百度零 key、大陆可达，是唯一兜底：它失败
// 即整链失败，不再往下退（此前会退化到 DuckDuckGo，但 DDG 在大陆出口 IP 上
// 稳定 202，属于必然失败的降级，只会多等一次超时并打出误导日志）。
func searchWithFallback(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error) {
	chain := buildSearchFallbackChain(spec.Engine)
	return searchChain(ctx, chain, spec, query, maxResults, search)
}

// searchChain 依次尝试链上引擎，全部失败时返回聚合错误。
//
// searchFn 抽成参数是为了可测性：search 是按引擎分发的真实实现（无注入点），
// 测试里传 fake 即可模拟「链首失败 → 兜底也失败」的完整降级路径。
func searchChain(
	ctx context.Context,
	chain []model.SearchEngine,
	spec SearchSpec,
	query string,
	maxResults int,
	searchFn func(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error),
) ([]webSearchResult, error) {
	// errs 按链上顺序记录每个引擎的失败原因（已去掉各自的 "web_search: " 前缀，
	// 避免聚合时重复）。整链全败时全部拼进最终错误。
	errs := make([]string, 0, len(chain))
	for i, engine := range chain {
		cur := SearchSpec{Engine: engine}
		if engine == spec.Engine {
			cur.APIKey = spec.APIKey
		}
		results, err := searchFn(ctx, cur, query, maxResults)
		if err == nil && !hasRelevantResults(query, results) {
			err = fmt.Errorf("返回 %d 条结果均与查询无关（疑似市场误判或召回失败）", len(results))
		}
		if err == nil {
			if i > 0 {
				// 降级成功也要把前一引擎为什么失败打进日志，否则「为什么降级了」无从查起。
				log.Printf("[web_search] %s 检索失败（%s），已降级为 %s",
					spec.Engine.DisplayName(), strings.Join(errs, "；"), engine.DisplayName())
			}
			return results, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %s", engine.DisplayName(), strings.TrimPrefix(err.Error(), "web_search: ")))
		log.Printf("[web_search] 引擎 %s 检索失败: %v", engine.DisplayName(), err)
	}
	// 整链全败：不能只返回最后一个引擎的错误。用户配置的引擎（链首）为什么
	// 失败往往才是排查关键 —— 例如 Serper key 失效触发降级、兜底百度又触发
	// 安全验证，若只报百度的错，用户会以为问题出在百度，Serper 的错被吞掉。
	// 参照 LLM tool_choice 降级的处理：把链上每个引擎的错误按顺序带上。
	return nil, fmt.Errorf("web_search: 所有检索引擎均失败 — %s", strings.Join(errs, "；"))
}

// buildSearchFallbackChain 构造检索降级链：配置引擎 → 百度（终点，去重）。
//
// 百度是唯一兜底，因此链条最长为 2：
//   - 配置引擎是百度 / 已下线（Hidden）/ 非法值 → 链只有 [baidu]，失败即返回错误；
//   - 其余引擎 → [配置引擎, baidu]，自身失败后退到百度再试一次。
//
// 已下线引擎（如 ddg）被回落到百度：历史配置可能仍存着它，避免一路必败。
func buildSearchFallbackChain(engine model.SearchEngine) []model.SearchEngine {
	if !engine.Valid() || engine.Hidden() {
		engine = model.SearchEngineBaidu
	}
	chain := make([]model.SearchEngine, 0, 2)
	chain = append(chain, engine)
	if engine != model.SearchEngineBaidu {
		chain = append(chain, model.SearchEngineBaidu)
	}
	return chain
}

// search 按引擎分发到对应后端。
func search(ctx context.Context, spec SearchSpec, query string, maxResults int) ([]webSearchResult, error) {
	switch spec.Engine {
	case model.SearchEngineDDG:
		return duckduckgoSearch(ctx, query, maxResults)
	case model.SearchEngineBocha:
		return bochaSearch(ctx, spec.APIKey, query, maxResults)
	case model.SearchEngineTavily:
		return tavilySearch(ctx, spec.APIKey, query, maxResults)
	case model.SearchEngineSerper:
		return serperSearch(ctx, spec.APIKey, query, maxResults)
	case model.SearchEngineTencentWSA:
		return tencentWSASearch(ctx, spec.APIKey, query, maxResults)
	case model.SearchEngineBaiduQianfan:
		return baiduQianfanSearch(ctx, spec.APIKey, query, maxResults)
	default: // baidu 与非法值都回落 baidu，保证零依赖可用
		return baiduSearch(ctx, query, maxResults)
	}
}

// webSearchResult 一条搜索结果。
type webSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// ============================================================
// 后端：百度 HTML（零 key）
// ============================================================

// baiduClientOnce 保证百度 HTTP client（带 cookie jar）只初始化一次。
// baiduClient 进程内共享，被 deep_search 多轮检索并发复用。
var (
	baiduClientOnce sync.Once
	baiduClient     *http.Client
)

// getBaiduClient 返回进程内共享的百度 HTTP client。
//
// 百度反爬关键点：无 cookie 的裸请求连续 2-3 次就会触发「百度安全验证」
// （302 到 wappass.baidu.com 图形验证码页）。必须先访问首页拿到 BAIDUID
// cookie，后续搜索复用同一 cookie 会话，才能连续稳定返回结果。
//
// http.Client 与 cookiejar.Jar 均并发安全，可被 deep_search 一轮内的多个
// web_search 并发调用。CheckRedirect 不跟随跳转：搜索页正常 200，link
// 跳转 302 需要手动读 Location 头还原真实 URL。
func getBaiduClient() *http.Client {
	baiduClientOnce.Do(func() {
		jar, _ := cookiejar.New(nil)
		baiduClient = &http.Client{
			Timeout: webSearchTimeout,
			Jar:     jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		// 预热：访问首页拿 BAIDUID cookie（失败也不影响后续搜索，届时触发验证码并降级）。
		req, err := http.NewRequest(http.MethodGet, baiduHomeURL, nil)
		if err != nil {
			return
		}
		req.Header.Set("User-Agent", searchUserAgent)
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		if resp, err := baiduClient.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, searchMaxReadBytes))
			resp.Body.Close()
		}
	})
	return baiduClient
}

// baiduSearch 请求百度 HTML 结果页并解析出结构化结果。
func baiduSearch(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	reqURL := baiduHTMLBase + "?wd=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", searchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	client := getBaiduClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求百度失败（可能网络不可达或需走代理）: %w", err)
	}
	defer resp.Body.Close()

	// 触发安全验证时百度 302 到 wappass（CheckRedirect 不跟随，Location 可见）。
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "wappass") {
		return nil, fmt.Errorf("web_search: 百度触发安全验证（请求过频，请稍后重试）")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web_search: 百度返回状态码 %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, searchMaxReadBytes))
	if err != nil {
		return nil, fmt.Errorf("web_search: 读取响应失败: %w", err)
	}

	results := parseBaiduResults(data, maxResults)
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: 未解析到搜索结果（可能被限流、触发验证码，或网络异常）")
	}
	// 无 mu 属性的结果（如百度百科）URL 仍是 baidu.com/link 跳转链接，跟随 302 还原真实 URL。
	for i := range results {
		if !strings.Contains(results[i].URL, "baidu.com/link") {
			continue
		}
		if real, err := resolveBaiduLink(ctx, client, results[i].URL); err == nil {
			results[i].URL = real
		}
	}
	return results, nil
}

// parseBaiduResults 从百度 HTML 里解析搜索结果。
//
// 结构约定（www.baidu.com/s 静态 HTML）：
//   - 自然结果在 <div class="result c-container ...">，百度自有产品（百科等）在
//     <div class="result-op ...">。两者都含独立 class，统一收集。
//   - 标题在 <h3><a href=..>标题</a></h3>（文本含 <em> 高亮标签，nodeText 会剥掉）。
//   - 真实 URL 优先取容器的 mu 属性（百度把目标 URL 直接内联在 mu 里，零额外请求）；
//     mu 缺失时退回 h3 内 a 的 href（baidu.com/link 跳转链接，后续 302 解码）。
//   - 摘要在 <div class="c-abstract">（旧版），按需提取。
//   - 推荐位 / 热搜榜（recommend_list、board 等）无 h3 标题，靠「标题非空」过滤掉。
func parseBaiduResults(data []byte, maxResults int) []webSearchResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}

	var nodes []*html.Node
	collectByClass(doc, "result", &nodes)
	collectByClass(doc, "result-op", &nodes)

	results := make([]webSearchResult, 0, maxResults)
	for _, n := range nodes {
		if len(results) >= maxResults {
			break
		}
		r := webSearchResult{}
		// 标题：h3 内 a 的文本。
		if h3 := firstByTag(n, "h3"); h3 != nil {
			if a := firstByTag(h3, "a"); a != nil {
				r.Title = strings.TrimSpace(nodeText(a))
				r.URL = nodeAttr(a, "href")
			}
		}
		// 真实 URL：mu 属性优先（直接是目标 URL）。
		if mu := nodeAttr(n, "mu"); mu != "" {
			r.URL = mu
		}
		// 摘要：c-abstract（兼容旧版）。
		if abs := firstByClass(n, "c-abstract"); abs != nil {
			r.Snippet = strings.TrimSpace(nodeText(abs))
		}
		// 推荐位 / 热搜榜无标题，靠标题为空过滤。
		if r.Title == "" {
			continue
		}
		results = append(results, r)
	}
	return results
}

// resolveBaiduLink 跟随 baidu.com/link 跳转（302）还原真实目标 URL。
func resolveBaiduLink(ctx context.Context, client *http.Client, link string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", searchUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return "", fmt.Errorf("百度 link 跳转未返回 Location")
}

// ============================================================
// 后端：DuckDuckGo HTML（零 key）
// ============================================================

// duckduckgoSearch 请求 DuckDuckGo HTML 结果页并解析出结构化结果。
func duckduckgoSearch(ctx context.Context, query string, maxResults int) ([]webSearchResult, error) {
	reqURL := ddgHTMLBase + "?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", searchUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	client := &http.Client{Timeout: webSearchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求 DuckDuckGo 失败（可能网络不可达或需走代理）: %w", err)
	}
	defer resp.Body.Close()

	// 202 是 DDG 的 anomaly 反爬挑战页（含 "anomaly" / "challenge" 标记），
	// 通常因出口 IP（机房/数据中心 IP）被判定为机器人触发，与 GET/POST、请求头无关，
	// 无法在纯 HTTP 层绕过，单独提示并引导换引擎，避免误当成普通网络错误。
	if resp.StatusCode == http.StatusAccepted {
		return nil, fmt.Errorf("web_search: DuckDuckGo 返回 202（anomaly 反爬验证），当前出口 IP 被判定为机器人，建议改用百度或博查/Tavily/Serper 等商业搜索 API")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web_search: DuckDuckGo 返回状态码 %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, searchMaxReadBytes))
	if err != nil {
		return nil, fmt.Errorf("web_search: 读取响应失败: %w", err)
	}

	results := parseDDGResults(data, maxResults)
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: 未解析到搜索结果（可能被限流、触发验证码，或网络异常）")
	}
	return results, nil
}

// parseDDGResults 从 DuckDuckGo HTML 里解析搜索结果。
//
// 结构约定（html.duckduckgo.com/html 静态版）：
//   - 每条结果一个容器 div.result，内含 <a class="result__a">（标题 + href）
//     与 <a class="result__snippet">（摘要）。
//   - result__a 的 href 是 DDG 跳转链接（//duckduckgo.com/l/?uddg=<编码>），
//     需解码出真实目标 URL。
func parseDDGResults(data []byte, maxResults int) []webSearchResult {
	doc, err := html.Parse(bytes.NewReader(data))
	if err != nil {
		return nil
	}

	var titleNodes, snippetNodes []*html.Node
	collectByClass(doc, "result__a", &titleNodes)
	collectByClass(doc, "result__snippet", &snippetNodes)

	n := len(titleNodes)
	if n > maxResults {
		n = maxResults
	}
	results := make([]webSearchResult, 0, n)
	for i := 0; i < n; i++ {
		r := webSearchResult{
			Title: strings.TrimSpace(nodeText(titleNodes[i])),
			URL:   decodeDDGLink(nodeAttr(titleNodes[i], "href")),
		}
		if i < len(snippetNodes) {
			r.Snippet = strings.TrimSpace(nodeText(snippetNodes[i]))
		}
		if r.Title == "" && r.URL == "" {
			continue
		}
		results = append(results, r)
	}
	return results
}

// ============================================================
// 后端：博查 BochaAI（Bearer key）
// ============================================================

// bochaResp 博查 web-search 接口返回结构（只取用到的字段）。
type bochaResp struct {
	Code int `json:"code"`
	Data struct {
		WebPages struct {
			Value []struct {
				Name    string `json:"name"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			} `json:"value"`
		} `json:"webPages"`
	} `json:"data"`
}

// bochaSearch 调用博查 BochaAI web-search API。
func bochaSearch(ctx context.Context, apiKey, query string, maxResults int) ([]webSearchResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: 博查引擎缺少 API Key")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"query": query,
		"count": maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bochaAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造博查请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))

	data, err := doJSONRequest(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求博查失败: %w", err)
	}

	var resp bochaResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("web_search: 解析博查响应失败: %w", err)
	}
	if resp.Code != 0 && resp.Code != 200 {
		return nil, fmt.Errorf("web_search: 博查返回错误码 %d，原始响应: %s", resp.Code, summarizeBody(data))
	}

	results := make([]webSearchResult, 0, maxResults)
	for _, v := range resp.Data.WebPages.Value {
		if v.Name == "" && v.URL == "" {
			continue
		}
		results = append(results, webSearchResult{Title: v.Name, URL: v.URL, Snippet: v.Snippet})
		if len(results) >= maxResults {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: 博查未返回搜索结果（key 无效或额度不足）")
	}
	return results, nil
}

// ============================================================
// 后端：Tavily Search API（key）
// ============================================================

// tavilyResp Tavily search 返回结构（只取用到的字段）。
type tavilyResp struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

// tavilySearch 调用 Tavily Search API。
func tavilySearch(ctx context.Context, apiKey, query string, maxResults int) ([]webSearchResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: Tavily 引擎缺少 API Key")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"api_key":     strings.TrimSpace(apiKey),
		"query":       query,
		"max_results": maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造 Tavily 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	data, err := doJSONRequest(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求 Tavily 失败: %w", err)
	}

	var resp tavilyResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("web_search: 解析 Tavily 响应失败: %w", err)
	}

	results := make([]webSearchResult, 0, maxResults)
	for _, v := range resp.Results {
		if v.Title == "" && v.URL == "" {
			continue
		}
		results = append(results, webSearchResult{Title: v.Title, URL: v.URL, Snippet: v.Content})
		if len(results) >= maxResults {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: Tavily 未返回搜索结果（key 无效或额度不足），原始响应: %s", summarizeBody(data))
	}
	return results, nil
}

// ============================================================
// 后端：Serper Google Search API（X-API-KEY）
// ============================================================

// serperResp Serper search 返回结构（只取用到的字段）。
type serperResp struct {
	Organic []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"organic"`
}

// serperSearch 调用 Serper Google Search API。
func serperSearch(ctx context.Context, apiKey, query string, maxResults int) ([]webSearchResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: Serper 引擎缺少 API Key")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"q":   query,
		"num": maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serperAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造 Serper 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", strings.TrimSpace(apiKey))

	data, err := doJSONRequest(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求 Serper 失败: %w", err)
	}

	var resp serperResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("web_search: 解析 Serper 响应失败: %w", err)
	}

	results := make([]webSearchResult, 0, maxResults)
	for _, v := range resp.Organic {
		if v.Title == "" && v.Link == "" {
			continue
		}
		results = append(results, webSearchResult{Title: v.Title, URL: v.Link, Snippet: v.Snippet})
		if len(results) >= maxResults {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: Serper 未返回搜索结果（key 无效或额度不足）")
	}
	return results, nil
}

// ============================================================
// 后端：腾讯云联网搜索 API（SearchPro，服务 API KEY）
// ============================================================

// tencentWSAPage 腾讯云 SearchPro 返回的单条结果。
// Pages 数组里每个元素是 JSON 字符串，需二次 json.Unmarshal 解析。
type tencentWSAPage struct {
	Title   string   `json:"title"`
	Date    string   `json:"date"`
	URL     string   `json:"url"`
	Passage string   `json:"passage"`
	Content string   `json:"content"`
	Site    string   `json:"site"`
	Score   float64  `json:"score"`
	Images  []string `json:"images"`
	Favicon string   `json:"favicon"`
}

// tencentWSAResp SearchPro 接口返回结构。
// 业务错误可能出现在顶层 Error 或 Response.Error 两种位置，均做检查。
type tencentWSAResp struct {
	Response struct {
		Query     string   `json:"Query"`
		Pages     []string `json:"Pages"`
		Version   string   `json:"Version"`
		Msg       string   `json:"Msg"`
		RequestId string   `json:"RequestId"`
		Error     *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"Response"`
	Error *struct {
		Code    string `json:"Code"`
		Message string `json:"Message"`
	} `json:"Error"`
}

// tencentWSAError 构造腾讯云 WSA 业务错误信息，对常见鉴权/开通类错误码附加排查提示。
func tencentWSAError(code, message string) error {
	msg := fmt.Sprintf("web_search: 腾讯云搜索返回错误 %s: %s", code, message)
	switch code {
	case "UnauthorizedOperation":
		msg += "（服务密钥不正确：请确认填写的是 WSA 控制台的「服务密钥 API KEY」——以 sk- 开头，而非腾讯云标准 API 密钥 SecretId/SecretKey）"
	case "ResourceNotFound":
		msg += "（联网搜索服务尚未开通，请到「联网搜索 API 控制台」开通服务）"
	case "ResourceUnavailable":
		msg += "（账号可能欠费或资源不可用，请检查账户余额）"
	case "RequestLimitExceeded":
		msg += "（请求超过频率限制，请稍后重试）"
	}
	return fmt.Errorf("%s", msg)
}

// tencentWSASearch 调用腾讯云联网搜索 SearchPro 接口。
func tencentWSASearch(ctx context.Context, apiKey, query string, maxResults int) ([]webSearchResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: 腾讯云联网搜索引擎缺少 API Key")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"Query": query,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tencentWSAAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造腾讯云搜索请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))

	data, err := doJSONRequest(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求腾讯云搜索失败: %w", err)
	}

	var resp tencentWSAResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("web_search: 解析腾讯云搜索响应失败: %w", err)
	}
	if resp.Response.Error != nil && resp.Response.Error.Code != "" {
		return nil, tencentWSAError(resp.Response.Error.Code, resp.Response.Error.Message)
	}
	if resp.Error != nil && resp.Error.Code != "" {
		return nil, tencentWSAError(resp.Error.Code, resp.Error.Message)
	}
	if resp.Response.Msg == "hit black query" {
		return nil, fmt.Errorf("web_search: 腾讯云搜索命中敏感词（hit black query）")
	}

	results := make([]webSearchResult, 0, maxResults)
	for _, raw := range resp.Response.Pages {
		if len(results) >= maxResults {
			break
		}
		var p tencentWSAPage
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue // 单条解析失败跳过，不影响其余结果
		}
		if p.Title == "" && p.URL == "" {
			continue
		}
		snippet := p.Passage
		if snippet == "" {
			snippet = p.Content
		}
		results = append(results, webSearchResult{Title: p.Title, URL: p.URL, Snippet: snippet})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: 腾讯云搜索未返回结果（key 无效、资源未开通或欠费），原始响应: %s", summarizeBody(data))
	}
	return results, nil
}

// ============================================================
// 后端：百度千帆「百度搜索」API（独立搜索接口，Bearer key）
// ============================================================

// baiduQianfanRef 百度千帆 web_search 返回的单条引用。
type baiduQianfanRef struct {
	ID      int     `json:"id"`
	Title   string  `json:"title"`
	URL     string  `json:"url"`
	Snippet string  `json:"snippet"`
	Content string  `json:"content"`
	Icon    string  `json:"icon"`
	Website string  `json:"website"`
	Date    string  `json:"date"`
	Type    string  `json:"type"`
	Score   float64 `json:"rerank_score"`
}

// baiduQianfanResp 百度千帆 web_search 返回结构。
type baiduQianfanResp struct {
	RequestID  string            `json:"request_id"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	References []baiduQianfanRef `json:"references"`
}

// baiduQianfanSearch 调用百度千帆「百度搜索」接口。
func baiduQianfanSearch(ctx context.Context, apiKey, query string, maxResults int) ([]webSearchResult, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("web_search: 百度千帆搜索引擎缺少 API Key")
	}
	// 百度搜索 query 限制 72 字符（汉字按 2 字符计，约 36 汉字）。按 rune 截断到 36，
	// 覆盖中文最长场景，避免超长 query 触发参数错误浪费一次请求。
	if runes := []rune(query); len(runes) > 36 {
		query = string(runes[:36])
	}
	body, _ := json.Marshal(map[string]interface{}{
		"messages": []map[string]string{
			{"role": "user", "content": query},
		},
		"search_source": "baidu_search_v2",
		"resource_type_filter": []map[string]interface{}{
			{"type": "web", "top_k": maxResults},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baiduQianfanAPIBase, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: 构造百度千帆搜索请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))

	data, err := doJSONRequest(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: 请求百度千帆搜索失败: %w", err)
	}

	var resp baiduQianfanResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("web_search: 解析百度千帆搜索响应失败: %w", err)
	}
	if resp.Code != "" {
		return nil, fmt.Errorf("web_search: 百度千帆搜索返回错误 %s: %s", resp.Code, resp.Message)
	}

	results := make([]webSearchResult, 0, maxResults)
	for _, ref := range resp.References {
		if len(results) >= maxResults {
			break
		}
		if ref.Title == "" && ref.URL == "" {
			continue
		}
		snippet := ref.Snippet
		if snippet == "" {
			snippet = ref.Content
		}
		results = append(results, webSearchResult{Title: ref.Title, URL: ref.URL, Snippet: snippet})
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("web_search: 百度千帆搜索未返回结果（key 无效、额度不足或资源未开通），原始响应: %s", summarizeBody(data))
	}
	return results, nil
}

// ============================================================
// 通用辅助
// ============================================================

// doJSONRequest 发送请求并读回 JSON 响应体；非 2xx 或读取失败返回 error。
func doJSONRequest(req *http.Request) ([]byte, error) {
	client := &http.Client{Timeout: webSearchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, searchMaxReadBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 截断响应体，避免把整段错误页塞给调用方。
		return nil, fmt.Errorf("状态码 %d: %s", resp.StatusCode, summarizeBody(data))
	}
	return data, nil
}

// summarizeBody 截断响应体用于错误信息展示。
//
// 非 2xx 的响应页、以及「2xx 但空结果」时引擎返回的原始响应，都可能长达
// 几十 KB，整段塞进错误信息会让日志与 turn 记录失控（如 Serper key 无效时
// 返回 200 + 一段错误 JSON，而非 4xx）。统一截断到 300 字符，空体给占位说明，
// 保证日志里能定位到引擎返回的具体错误内容，又不至于刷屏。
func summarizeBody(data []byte) string {
	msg := strings.TrimSpace(string(data))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg == "" {
		msg = "(空响应体)"
	}
	return msg
}

// formatWebSearchResults 把搜索结果格式化成给 LLM 的文本。
func formatWebSearchResults(query string, results []webSearchResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "「%s」的搜索结果（共 %d 条）：\n", query, len(results))
	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, r.Title)
		if r.URL != "" {
			fmt.Fprintf(&b, "   链接: %s\n", r.URL)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   摘要: %s\n", r.Snippet)
		}
	}
	return b.String()
}

// decodeDDGLink 把 DDG 的跳转链接还原为真实目标 URL。
func decodeDDGLink(href string) string {
	const key = "uddg="
	i := strings.Index(href, key)
	if i < 0 {
		return href
	}
	rest := href[i+len(key):]
	if j := strings.Index(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	if decoded, err := url.QueryUnescape(rest); err == nil {
		return decoded
	}
	return rest
}

// ============================================================
// 结果相关性守卫
// ============================================================

// hasRelevantResults 相关性守卫：判断搜索结果是否与查询相关。
//
// 部分引擎对低频长尾查询（尤其中文具体名词）召回不稳定，可能返回与查询
// 毫无关系的「推荐 / 热门」内容（曾实测 Bing 搜「深圳天气」返回韩国 NAVER、
// 印度扭矩扳手）。这些垃圾一旦被当成成功结果喂给 LLM，会白白浪费检索轮次
// （LLM 误以为拿到了有效线索，转而 fetch 一堆无关链接）。
//
// 这里做轻量校验：结果标题 / 摘要须命中查询里的至少一个中文关键词，否则
// 视为「未返回有效结果」返回 error，触发降级链。只对能提取出中文关键词的
// 查询生效；纯英文 / 数字查询不校验，避免误伤。
func hasRelevantResults(query string, results []webSearchResult) bool {
	kws := extractChineseKeywords(query)
	if len(kws) == 0 {
		return true // 无法提取中文关键词（如英文查询），跳过校验
	}
	for _, r := range results {
		text := r.Title + " " + r.Snippet
		for _, kw := range kws {
			if strings.Contains(text, kw) {
				return true
			}
		}
	}
	return false
}

// extractChineseKeywords 提取查询里用于相关性校验的中文关键词。
//
// 简单启发式：把所有连续中文片段按 2 字滑窗切成候选词，去掉停用词与
// 日期片段（含年/月/日/号/时/分/秒），得到实义词集合。例如：
//
//	"深圳市 2026年9月10日 天气预报 气温" → 深圳 / 圳市 / 天气 / 气预 / 预报 / 气温
//
// 滑窗会产生「圳市」「气预」这类无意义词，但无妨——它们只是增多了候选，
// 校验逻辑是「任一命中即通过」，候选越多越宽松，不会因此误伤。
func extractChineseKeywords(query string) []string {
	seen := map[string]bool{}
	var kws []string
	for _, seg := range chineseSegments(query) {
		runes := []rune(seg)
		if len(runes) < 2 {
			continue
		}
		for i := 0; i < len(runes)-1; i++ {
			kw := string(runes[i : i+2])
			if isChineseStopKeyword(kw) {
				continue
			}
			if !seen[kw] {
				seen[kw] = true
				kws = append(kws, kw)
			}
		}
	}
	return kws
}

// chineseSegments 提取字符串里所有连续的 CJK 汉字片段（其余字符视为分隔符）。
func chineseSegments(s string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return segs
}

// isChineseStopKeyword 判断 2 字子串是否为停用词 / 日期片段，命中则从关键词剔除。
func isChineseStopKeyword(kw string) bool {
	// 含日期/时间单位字的片段（如「年9」「9月」「月1」「10日」）。
	for _, r := range kw {
		if strings.ContainsRune("年月日号时分秒点", r) {
			return true
		}
	}
	// 常见停用词。
	switch kw {
	case "今天", "明天", "昨天", "后天", "昨日", "今日", "历史", "未来",
		"最新", "查询", "搜索", "什么", "怎么", "如何", "关于", "一个",
		"哪个", "哪些", "哪里", "一下", "现在", "目前", "请问", "帮我":
		return true
	}
	return false
}

// firstByTag 深度遍历，返回第一个标签名精确等于 tag 的元素节点（无则 nil）。
func firstByTag(n *html.Node, tag string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := firstByTag(c, tag); found != nil {
			return found
		}
	}
	return nil
}

// firstByClass 深度遍历，返回第一个 class 精确包含 className 的元素节点（无则 nil）。
func firstByClass(n *html.Node, className string) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode {
		for _, attr := range n.Attr {
			if attr.Key == "class" && hasClass(attr.Val, className) {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := firstByClass(c, className); found != nil {
			return found
		}
	}
	return nil
}

// collectByClass 深度遍历 DOM，收集 class 精确包含 className 的元素节点。
func collectByClass(n *html.Node, className string, out *[]*html.Node) {
	if n.Type == html.ElementNode {
		for _, attr := range n.Attr {
			if attr.Key == "class" && hasClass(attr.Val, className) {
				*out = append(*out, n)
				break
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectByClass(c, className, out)
	}
}

// hasClass 判断 class 属性（空格分隔的多个类名）是否精确包含 name。
func hasClass(class, name string) bool {
	for _, c := range strings.Fields(class) {
		if c == name {
			return true
		}
	}
	return false
}

// nodeText 递归收集节点内所有文本（含嵌套元素）。
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// nodeAttr 取节点的某个属性值，不存在返回空串。
func nodeAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}
