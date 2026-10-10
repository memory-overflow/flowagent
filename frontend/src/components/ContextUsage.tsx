import { useCallback, useEffect, useRef, useState } from 'react';
import { getContextUsage } from '../services/api';
import type { ContextUsageInfo } from '../services/api';
import { useI18n, useT } from '../i18n';

/**
 * 上下文用量指示器（对话输入框右下角）。
 *
 * 展示「最近一次 LLM 调用真实送出的输入」占模型上下文上限的比例，
 * 点击弹出分块明细卡片：系统提示词 / 工具定义 / 工作流摘要 /
 * 压缩历史摘要 / 历史消息 / 本轮输入与当前任务。
 *
 * 数据来自后端 conversations/context-usage 接口（读最近一次带
 * context_blocks 明细的 turn 快照），不是前端估算 —— 估算口径与
 * 真实请求一定有偏差，读真实数据不会。
 *
 * 刷新时机：挂载 / 切换对话 / 每 20s 轮询（接口是单条索引查询，代价可忽略）。
 * 执行中的任务落库 turn 后最迟 20s 会反映出来；弹窗打开时立即刷新一次。
 */

// 各分块的展示元信息：颜色与调试页保持同一套视觉语言
// （调试页的 turn 卡片复用此表渲染每次 LLM 调用的分块占比）
export interface ContextBlocksView {
  system_prompt: number;
  tools: number;
  workflow_summary: number;
  compacted: number;
  detail: number;
  user_and_current: number;
  total: number;
}

// label / desc 不写死：表里只放 i18n key，由 useBlockMeta() 翻译后再用。
// 调试页与本组件共用同一份定义（含颜色），避免两处对不上。
export interface BlockMeta {
  key: keyof ContextBlocksView;
  labelKey: string;
  color: string;
  descKey: string;
}

export const BLOCK_META: BlockMeta[] = [
  { key: 'system_prompt', labelKey: 'context.block.system', color: '#5b8def', descKey: 'context.block.systemDesc' },
  { key: 'tools', labelKey: 'context.block.tools', color: '#e8a840', descKey: 'context.block.toolsDesc' },
  { key: 'workflow_summary', labelKey: 'context.block.wfSummary', color: '#a78bfa', descKey: 'context.block.wfSummaryDesc' },
  { key: 'compacted', labelKey: 'context.block.compacted', color: '#22b8a6', descKey: 'context.block.compactedDesc' },
  { key: 'detail', labelKey: 'context.block.history', color: '#4caf7d', descKey: 'context.block.historyDesc' },
  { key: 'user_and_current', labelKey: 'context.block.current', color: '#7ba6ff', descKey: 'context.block.currentDesc' },
];

/** 把 BLOCK_META 的 key 翻译成中文/英文文案，返回带 label / desc 的表 */
export function useBlockMeta(): Array<{ key: keyof ContextBlocksView; label: string; color: string; desc: string }> {
  const t = useT();
  return BLOCK_META.map((m) => ({
    key: m.key,
    label: t(m.labelKey),
    color: m.color,
    desc: t(m.descKey),
  }));
}

/** token 数格式化：14731 → 14.3K，983040 → 960.0K */
export function fmtTokens(n: number): string {
  if (n >= 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + 'M';
  if (n >= 1024) return (n / 1024).toFixed(1) + 'K';
  return String(n);
}

export function ContextUsage({ chatId }: { chatId: string }) {
  const { t, lang } = useI18n();
  const blockMeta = useBlockMeta();
  const [usage, setUsage] = useState<ContextUsageInfo | null>(null);
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);

  const fetchUsage = useCallback(async () => {
    try {
      const res = await getContextUsage(chatId);
      setUsage(res.data.data);
    } catch {
      // 指示器是纯展示特性，接口失败就隐藏，不弹错误打扰对话
      setUsage(null);
    }
  }, [chatId]);

  useEffect(() => {
    setUsage(null);
    setOpen(false);
    fetchUsage();
    const timer = setInterval(fetchUsage, 20000);
    return () => clearInterval(timer);
  }, [chatId, fetchUsage]);

  // 点击卡片外关闭
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  if (!usage || !usage.has_data || !usage.blocks) return null;

  const blocks: ContextBlocksView = usage.blocks;
  const total = blocks.total || 0;
  const limit = usage.context_limit || 0;
  const percent = limit > 0 ? (total / limit) * 100 : 0;
  const overTrigger = limit > 0 && total >= usage.trigger;
  const turn = usage.turn;

  // 分段进度条：按占比切成彩色段，空块不渲染
  const segments = blockMeta.map((m) => ({
    ...m,
    value: blocks[m.key] ?? 0,
  })).filter((s) => s.value > 0);

  return (
    <div className="ctx-usage" ref={wrapRef}>
      {/* 指示 pill：环形比例 + 百分比，hover 有摘要 tooltip */}
      <button
        type="button"
        className={`ctx-usage-pill${overTrigger ? ' is-over' : ''}`}
        onClick={() => {
          setOpen((v) => !v);
          if (!open) fetchUsage(); // 打开时刷新一次，保证看到的是最新水位
        }}
        title={
          limit > 0
            ? t('context.pillTip', {
                percent: percent.toFixed(1),
                used: fmtTokens(total),
                limit: fmtTokens(limit),
              })
            : t('context.title')
        }
      >
        <svg className="ctx-usage-ring" viewBox="0 0 20 20" aria-hidden>
          <circle cx="10" cy="10" r="8" fill="none" stroke="var(--border-light)" strokeWidth="3" />
          <circle
            cx="10" cy="10" r="8" fill="none"
            stroke={overTrigger ? 'var(--warning)' : 'var(--accent)'}
            strokeWidth="3"
            strokeDasharray={`${Math.min(percent, 100) / 100 * 50.27} 50.27`}
            strokeLinecap="round"
            transform="rotate(-90 10 10)"
          />
        </svg>
        <span className="ctx-usage-percent">{percent.toFixed(percent < 10 ? 1 : 0)}%</span>
      </button>

      {open && (
        <div className="ctx-usage-pop">
          <div className="ctx-usage-head">
            <span className="ctx-usage-title">{t('context.title')}</span>
            <span className="ctx-usage-spacer" />
            <span className="ctx-usage-model">{usage.model_name || turn?.model || ''}</span>
          </div>
          <div className="ctx-usage-summary">
            <span
              className="ctx-usage-big"
              style={{ color: overTrigger ? 'var(--warning)' : 'var(--text-primary)' }}
            >
              {percent.toFixed(1)}%
            </span>
            <span className="ctx-usage-sub">
              {t('context.used', { used: fmtTokens(total), limit: fmtTokens(limit) })}
            </span>
          </div>

          {/* 分段彩色进度条 + 触发线刻度（80%） */}
          <div className="ctx-usage-bar">
            {segments.map((s) => (
              <div
                key={s.key}
                className="ctx-usage-bar-seg"
                style={{ width: `${total > 0 ? (s.value / (limit || total)) * 100 : 0}%`, background: s.color }}
                title={`${s.label} ${fmtTokens(s.value)}`}
              />
            ))}
            {limit > 0 && (
              <div
                className="ctx-usage-bar-trigger"
                style={{ left: `${(usage.trigger / limit) * 100}%` }}
                title={t('context.triggerLine', { n: fmtTokens(usage.trigger) })}
              />
            )}
          </div>

          <div className="ctx-usage-rows">
            {segments.map((s) => (
              <div key={s.key} className="ctx-usage-row" title={s.desc}>
                <span className="ctx-usage-dot" style={{ background: s.color }} />
                <span className="ctx-usage-label">{s.label}</span>
                <span className="ctx-usage-spacer" />
                <span className="ctx-usage-val">
                  {total > 0 ? `${((s.value / total) * 100).toFixed(1)}%` : '-'}
                </span>
                <span className="ctx-usage-tok">{fmtTokens(s.value)}</span>
              </div>
            ))}
          </div>

          <div className="ctx-usage-foot">
            {turn ? (
              <>
                {t('context.source', { seq: turn.turn_seq, phase: turn.phase_name })}
                {turn.create_ts
                  ? ` · ${new Date(turn.create_ts).toLocaleTimeString(
                      lang === 'zh' ? 'zh-CN' : 'en-US',
                      { hour12: false },
                    )}`
                  : ''}{' '}
                · {t('context.providerTok', { n: fmtTokens(turn.prompt_tokens) })}
              </>
            ) : (
              t('context.noRecord')
            )}
          </div>
        </div>
      )}
    </div>
  );
}
