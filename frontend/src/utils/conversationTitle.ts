// 对话标题的占位形态判定与展示折算。
//
// 后端单点定义占位标题（按 X-Lang 落「新对话」/「New Chat」），前端新建对话时
// 不传 title，因此这里只需要认得占位形态，展示时折算成界面语言的「新建对话」，
// 不会在英文界面里冒出一条中文占位标题。
//
// 历史形态（早期硬编码「新对话」、i18n 后一度写入的「新建对话」/「New chat」）
// 也要一并认得，否则存量对话的占位标题永远不会被首条输入覆盖。
const PLACEHOLDER_TITLES = [
  '',
  '新对话',
  '新建对话',
  'new chat',
  '未命名对话',
  'untitled',
];

export function isPlaceholderTitle(title?: string | null): boolean {
  return PLACEHOLDER_TITLES.includes((title || '').trim().toLowerCase());
}

/** 列表/头部展示用：占位标题折算为界面语言文案，空标题给「未命名」。 */
export function displayTitle(title: string | undefined | null, placeholder: string, untitled: string): string {
  if (!title || title.trim() === '') return untitled;
  return isPlaceholderTitle(title) ? placeholder : title;
}
