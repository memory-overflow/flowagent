// 输入框里 @ / # 引用片段的解析与替换。
//
// 为什么抽成纯函数：这段规则靠「光标前是不是一个触发符」判定，很容易被
// 顺手放宽成「文本里出现 @ 就弹菜单」，于是邮箱（user@example.com）、
// 代码片段、正则里的 @ / # 都会误触发。把判定、替换、过滤放在无 React
// 依赖的纯函数里，才能用脚本直接验证，也方便以后加触发符（如 / 命令）。
//
// 触发条件（三条同时满足）：
//  1. 触发符是 @ 或 #
//  2. 触发符位于文本开头，或前一个字符是空白（含换行）
//  3. 触发符与光标之间没有空白
// 即「正在输入一个引用片段」，此时才提示候选。

/** 引用触发符。@ 引用对话文件，# 调用技能与指令 */
export type MentionTrigger = '@' | '#';

/** 一次进行中的引用输入 */
export interface MentionQuery {
  trigger: MentionTrigger;
  /** 触发符在文本中的下标 —— 替换区间从这里开始 */
  start: number;
  /** 光标位置 —— 替换区间到这里结束 */
  end: number;
  /** 触发符之后、光标之前的片段，用作过滤关键字 */
  query: string;
}

const isSpace = (ch: string) => /\s/.test(ch);

/**
 * 检测光标前是否正在输入一个引用片段。
 *
 * caret 允许越界（浏览器在 value 刚被程序改写时可能报旧位置），
 * 内部先夹到 [0, len] 再判定，避免把 undefined 当 0 处理导致误判。
 */
export function detectMention(text: string, caret: number | null | undefined): MentionQuery | null {
  if (caret == null) return null;
  const pos = Math.max(0, Math.min(text.length, caret));
  if (pos === 0) return null;

  // 从光标往前扫到最近的空白或触发符
  let i = pos;
  while (i > 0 && !isSpace(text[i - 1]) && text[i - 1] !== '@' && text[i - 1] !== '#') {
    i--;
  }
  if (i === 0) return null;

  const trigger = text[i - 1];
  if (trigger !== '@' && trigger !== '#') return null;

  const start = i - 1;
  // 触发符必须处在词首：开头、空白之后
  if (start > 0 && !isSpace(text[start - 1])) return null;

  return { trigger, start, end: pos, query: text.slice(i, pos) };
}

/**
 * 用选中项替换掉正在输入的引用片段。
 *
 * 末尾补一个空格，用户可以接着往下写而不会和引用粘在一起；
 * 后面本来就有空白（或已到文末）时不重复补。
 *
 * insert 为空表示「本地指令」：这类候选就地生效、不进正文，
 * 因此连触发符一起吃掉，且不补空格 —— 否则会留下一个孤零零的「# 」。
 */
export function applyMention(
  text: string,
  q: MentionQuery,
  insert: string,
): { text: string; caret: number } {
  const before = text.slice(0, q.start);
  const after = text.slice(q.end);

  if (insert === '') {
    // 片段后面的空白整段去掉：片段前面本来就有分隔（触发符必须处在词首），
    // 保留后面的空白会在「看下 #停止 好吗」里留下两个连续空格
    const tail = after.replace(/^\s+/, '');
    return { text: before + tail, caret: before.length };
  }

  const token = q.trigger + insert;
  const needSpace = after.length === 0 || !isSpace(after[0]);
  const spacing = needSpace ? ' ' : '';
  return {
    text: before + token + spacing + after,
    caret: before.length + token.length + spacing.length,
  };
}

/** needle 是否为 hay 的子序列（字符按序出现即可，不必连续） */
function isSubsequence(needle: string, hay: string): boolean {
  let i = 0;
  for (let j = 0; j < hay.length && i < needle.length; j++) {
    if (hay[j] === needle[i]) i++;
  }
  return i === needle.length;
}

/**
 * 按查询片段过滤候选。
 *
 * 三级排序，结果比单纯 includes 好用得多：前缀命中 > 子串命中 > 子序列命中。
 * 子序列一级是为了照顾缩写输入（如「wsearch」命中「web_search」）；
 * 同级保持原顺序（技能在前、指令在后），避免筛选后组内顺序跳动。
 */
export function filterMentionItems<T>(
  items: T[],
  query: string,
  key: (item: T) => string,
): T[] {
  const q = query.trim().toLowerCase();
  if (!q) return items;

  const scored: Array<{ item: T; score: number; idx: number }> = [];
  items.forEach((item, idx) => {
    const k = key(item).toLowerCase();
    let score: number;
    if (k.startsWith(q)) score = 0;
    else if (k.includes(q)) score = 1;
    else if (isSubsequence(q, k)) score = 2;
    else return;
    scored.push({ item, score, idx });
  });
  scored.sort((a, b) => a.score - b.score || a.idx - b.idx);
  return scored.map((s) => s.item);
}
