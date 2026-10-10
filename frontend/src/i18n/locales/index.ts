import { zh as coreZh } from './zh';
import { en as coreEn } from './en';
import { chatNS } from './ns-chat';
import { workflowNS } from './ns-workflow';
import { debugNS } from './ns-debug';
import { modelsNS } from './ns-models';
import { mcpNS } from './ns-mcp';
import { skillsNS } from './ns-skills';
import { searchNS } from './ns-search';

// ============================================================
// 词典装配
//
// 核心（common / nav / status …）留在 zh.ts / en.ts；每个大页面一个
// ns-*.ts，里面 zh 与 en **成对**写在一起：
//
//   export const chatNS = { zh: { … }, en: { … } };
//
// 为什么成对而不是分成两个文件：翻译最容易出的错是「一边加了 key 另一边
// 忘了」，key 不对齐时英文缺失会静默回落中文。放同一个文件里，加 key 时
// 上下两行就是对照，diff 也一眼能看出漏翻。
// ============================================================

export const zh = {
  ...coreZh,
  chat: chatNS.zh,
  workflow: workflowNS.zh,
  debug: debugNS.zh,
  models: modelsNS.zh,
  mcp: mcpNS.zh,
  skills: skillsNS.zh,
  searchConfig: searchNS.zh,
};

export const en = {
  ...coreEn,
  chat: chatNS.en,
  workflow: workflowNS.en,
  debug: debugNS.en,
  models: modelsNS.en,
  mcp: mcpNS.en,
  skills: skillsNS.en,
  searchConfig: searchNS.en,
};
