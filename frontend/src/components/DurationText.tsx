import { useCallback } from 'react';
import { formatElapsedMs, useTickingElapsed } from '../hooks/useTickingElapsed';
import type { WorkflowDuration } from '../types/api';
import { useI18n, useT, type Translate } from '../i18n';

/**
 * 执行时长文本。
 *
 * 终点规则在后端：已终态的任务时长算死，运行中的任务以「当前时间」为终点，
 * 每次轮询拿到的都是更大的值。这里再叠一层本地秒跳，避免两次轮询之间
 * （5s）看起来是卡住的 —— 与对话消息耗时的做法保持一致。
 *
 * 单独抽组件是因为 hook 不能写在条件渲染里：节点详情与工作流头部都是
 * 「有数据才渲染」，hook 必须在组件顶层无条件调用。
 */
export default function DurationText({
  duration,
  className,
  title,
}: {
  duration: WorkflowDuration;
  className?: string;
  title?: string;
}) {
  const ms = useTickingElapsed(duration.duration_ms, duration.running);
  return (
    <span className={className} title={title}>
      {formatElapsedMs(ms)}
    </span>
  );
}

/**
 * 时长 tooltip：给出具体起止时刻，方便对账「这段时间到底花在哪」。
 *
 * t 与 locale 由调用方注入（而不是在这里直接用 hook）—— 该函数在列表
 * 循环里被调用，hook 不能进循环；用 useDurationTooltip 包一层即可。
 */
export function durationTooltip(
  t: Translate,
  locale: string,
  label: string,
  d: WorkflowDuration,
): string {
  const start = new Date(d.start_ts).toLocaleTimeString(locale, { hour12: false });
  if (d.running || !d.end_ts) {
    return t('duration.runningTip', { label, elapsed: formatElapsedMs(d.duration_ms), start });
  }
  const end = new Date(d.end_ts).toLocaleTimeString(locale, { hour12: false });
  return t('duration.doneTip', { label, elapsed: formatElapsedMs(d.duration_ms), start, end });
}

/** 在组件里用这个：绑定了当前语言的 durationTooltip */
export function useDurationTooltip() {
  const t = useT();
  const { lang } = useI18n();
  const locale = lang === 'zh' ? 'zh-CN' : 'en-US';
  return useCallback(
    (label: string, d: WorkflowDuration) => durationTooltip(t, locale, label, d),
    [t, locale],
  );
}
