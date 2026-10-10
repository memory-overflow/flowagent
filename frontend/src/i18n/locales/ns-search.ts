// search 页面/组件的文案。zh 与 en 成对维护（见同目录 index.ts 的说明）。
export const searchNS = {
  zh: {
    title: '检索引擎配置',
    intro:
      '全局检索引擎，决定 deep_search 里 web_search 工具用哪个搜索引擎。默认百度（零 key、零依赖，开箱可用）；切博查 / Tavily / Serper / 腾讯云联网搜索 / 百度千帆需先填写对应 API Key。检索失败时按「当前引擎 → 百度」降级，百度是最终兜底。',

    currentEngine: '当前引擎',
    currentBadge: '当前',
    applyKey: '申请 Key ↗',

    apiKeyGroup: 'API Key（按需填写，留空表示不修改）',
    apiKeyPlaceholder: '{engine} 的 API Key',
    keyRequired: '切换 {engine} 需要填写 API Key（申请地址：{url}）',

    connectivityTest: '连通性测试',
    queryPlaceholder: '输入关键词，用当前引擎跑一次真实搜索',
    test: '测试',
    testing: '测试中...',
    errQueryRequired: '请输入测试关键词',
    searchOk: '✅ 搜索成功',
    searchFail: '❌ 搜索失败',
    noResultText: '（无结果文本）',
    testFailed: '测试失败: {msg}',

    saved: '已保存',
    saveFailed: '保存失败: {msg}',

    // ---- 引擎名：品牌名用官方英文写法 ----
    engineBaidu: '百度',
    engineDdg: 'DuckDuckGo',
    engineBocha: '博查 BochaAI',
    engineTavily: 'Tavily',
    engineSerper: 'Serper',
    engineTencentWsa: '腾讯云联网搜索',
    engineBaiduQianfan: '百度千帆搜索',
  },
  en: {
    title: 'Search engine configuration',
    intro:
      'Global search engine — decides which engine the web_search tool in deep_search uses. Baidu is the default (no API key, no dependencies, works out of the box); switching to Bocha / Tavily / Serper / Tencent Cloud Web Search / Baidu Qianfan requires the matching API key first. When a search fails, it degrades from the current engine to Baidu, which is the final fallback.',

    currentEngine: 'Current engine',
    currentBadge: 'Current',
    applyKey: 'Get a key ↗',

    apiKeyGroup: 'API keys (fill in as needed; leave blank to keep unchanged)',
    apiKeyPlaceholder: '{engine} API key',
    keyRequired: 'Switching to {engine} requires an API key (sign up at: {url})',

    connectivityTest: 'Connectivity test',
    queryPlaceholder: 'Enter a query to run one real search with the current engine',
    test: 'Test',
    testing: 'Testing...',
    errQueryRequired: 'Please enter a search query',
    searchOk: '✅ Search succeeded',
    searchFail: '❌ Search failed',
    noResultText: '(no result text)',
    testFailed: 'Test failed: {msg}',

    saved: 'Saved',
    saveFailed: 'Failed to save: {msg}',

    // ---- 引擎名：品牌名用官方英文写法 ----
    engineBaidu: 'Baidu',
    engineDdg: 'DuckDuckGo',
    engineBocha: 'Bocha AI',
    engineTavily: 'Tavily',
    engineSerper: 'Serper',
    engineTencentWsa: 'Tencent Cloud Web Search',
    engineBaiduQianfan: 'Baidu Qianfan Search',
  },
};
