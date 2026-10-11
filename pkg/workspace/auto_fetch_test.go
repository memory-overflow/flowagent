package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/memory-overflow/flowagent/model"
)

// 测试用抓取回调：按 URL 返回可辨识的正文，便于断言「第 i 条正文对应第 i 条结果」。
func echoFetcher() PageFetcherFunc {
	return func(ctx context.Context, url string, maxChars int) (string, error) {
		return fmt.Sprintf("BODY-%s", url), nil
	}
}

func sampleResults(n int) []webSearchResult {
	out := make([]webSearchResult, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, webSearchResult{
			Title:   fmt.Sprintf("标题%d", i+1),
			URL:     fmt.Sprintf("https://example.com/p%d", i+1),
			Snippet: fmt.Sprintf("摘要%d", i+1),
		})
	}
	return out
}

// TestFetchTopPagesDisabled 未开启自动抓取（回调为 nil / topN<=0）时不应抓取。
func TestFetchTopPagesDisabled(t *testing.T) {
	results := sampleResults(3)
	calls := 0
	counting := func(ctx context.Context, url string, maxChars int) (string, error) {
		calls++
		return "x", nil
	}

	if got := fetchTopPages(context.Background(), nil, results, 3, 100); got != "" {
		t.Errorf("PageFetcher 为 nil 时不应抓取，got %q", got)
	}
	if got := fetchTopPages(context.Background(), counting, results, 0, 100); got != "" {
		t.Errorf("topN<=0 时不应抓取，got %q", got)
	}
	if got := fetchTopPages(context.Background(), counting, nil, 3, 100); got != "" {
		t.Errorf("结果为空时不应抓取，got %q", got)
	}
	if calls != 0 {
		t.Errorf("不应发生任何抓取调用，got %d", calls)
	}
}

// TestFetchTopPagesOnlyTopN 只抓前 N 条，并提示剩余条数仍可用 fetch 精读。
func TestFetchTopPagesOnlyTopN(t *testing.T) {
	got := fetchTopPages(context.Background(), echoFetcher(), sampleResults(5), 3, 100)
	for i := 1; i <= 3; i++ {
		if !strings.Contains(got, fmt.Sprintf("BODY-https://example.com/p%d", i)) {
			t.Errorf("应包含第 %d 条正文，got %q", i, got)
		}
	}
	if strings.Contains(got, "BODY-https://example.com/p4") {
		t.Errorf("不应抓取第 4 条（超出 topN），got %q", got)
	}
	// 剩余 2 条要显式告知模型，否则它会以为搜索结果只有 3 条而漏掉来源。
	if !strings.Contains(got, "另有 2 条结果仅返回摘要") {
		t.Errorf("应提示剩余条数，got %q", got)
	}
}

// TestFetchTopPagesTruncation 正文超长时必须截断，避免一次搜索吃满上下文。
func TestFetchTopPagesTruncation(t *testing.T) {
	long := strings.Repeat("中", 500)
	fetcher := func(ctx context.Context, url string, maxChars int) (string, error) {
		return long, nil
	}
	got := fetchTopPages(context.Background(), fetcher, sampleResults(2), 2, 100)
	if strings.Count(got, "中") > 100*2+10 {
		t.Errorf("正文未按 maxChars 截断，中文字符数 = %d", strings.Count(got, "中"))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("截断后应有省略标记，got %q", got)
	}
}

// TestFetchTopPagesSkipsFailures 单条抓取失败（robots 拦截 / 超时）应静默跳过。
func TestFetchTopPagesSkipsFailures(t *testing.T) {
	fetcher := func(ctx context.Context, url string, maxChars int) (string, error) {
		if strings.HasSuffix(url, "p2") {
			return "", fmt.Errorf("403 robots denied")
		}
		return "OK", nil
	}
	got := fetchTopPages(context.Background(), fetcher, sampleResults(3), 3, 100)
	if !strings.Contains(got, "已抓取到的 2 条正文") {
		t.Errorf("应报告实际抓到 2 条，got %q", got)
	}
	if strings.Contains(got, "403") || strings.Contains(got, "robots") {
		t.Errorf("失败条目的错误不应进入结果，got %q", got)
	}
}

// TestFetchTopPagesConcurrentOrder 并发抓取的完成顺序乱序时，正文仍要按结果序号归位。
func TestFetchTopPagesConcurrentOrder(t *testing.T) {
	// 第 i 条故意睡 (n-i) * 20ms，让后面的先返回。
	fetcher := func(ctx context.Context, url string, maxChars int) (string, error) {
		idx := int(url[len(url)-1] - '0')
		time.Sleep(time.Duration(4-idx) * 20 * time.Millisecond)
		return fmt.Sprintf("BODY-%d", idx), nil
	}
	got := fetchTopPages(context.Background(), fetcher, sampleResults(3), 3, 100)
	for i := 1; i <= 3; i++ {
		want := fmt.Sprintf("[正文 %d] 标题%d\nBODY-%d", i, i, i)
		if !strings.Contains(got, want) {
			t.Errorf("第 %d 条正文串位：期望含 %q，got %q", i, want, got)
		}
	}
}

// TestWebSearchToolAutoFetchOptions 自动抓取配置要体现在工具说明与超时上。
func TestWebSearchToolAutoFetchOptions(t *testing.T) {
	def := WebSearchTool(SearchSpec{Engine: model.SearchEngineBaidu}, WebSearchOptions{
		DefaultMaxResults: 3,
		AutoFetchTopN:     3,
	})
	if !strings.Contains(def.Description, "自动附带前 3 条结果的正文") {
		t.Errorf("工具说明应告知模型正文已自动抓取（否则它会重复 fetch），got %q", def.Description)
	}
	// 超时必须覆盖「搜索 + 抓取」两段，否则慢站点会把搜索拖成整工具超时。
	if def.Timeout <= webSearchTimeout {
		t.Errorf("开启自动抓取后超时应大于搜索超时，got %v", def.Timeout)
	}
	// 默认条数要写进入参说明，模型才知道默认返回几条。
	schemaDesc := fmt.Sprint(def.InputSchema["properties"])
	if !strings.Contains(schemaDesc, "默认 3") {
		t.Errorf("入参说明应反映定制的默认条数，got %v", schemaDesc)
	}

	// 未配置自动抓取时，行为与历史一致：说明里不出现正文相关措辞。
	plain := WebSearchTool(SearchSpec{Engine: model.SearchEngineBaidu}, WebSearchOptions{})
	if strings.Contains(plain.Description, "正文") {
		t.Errorf("未开启自动抓取时说明不应提及正文，got %q", plain.Description)
	}
	if plain.Timeout != webSearchTimeout {
		t.Errorf("未开启自动抓取时超时应保持搜索超时，got %v", plain.Timeout)
	}
}
