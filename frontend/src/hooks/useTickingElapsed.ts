import { useEffect, useState } from 'react';

/**
 * 前端自主秒跳的耗时累加器。
 *
 * 用法：useTickingElapsed(baseMs, active)
 *   - baseMs：最近一次轮询/SSE 拉到的耗时（来自后端 time_cost_ms 字段），作为地板。
 *   - active：true 时按秒累加；false 时直接返回 baseMs（终态由后端算死，无需本地跳）。
 *
 * 当 baseMs 因轮询更新时自动重置基线，避免"已经被刷新到 11s、然后又跳回 10.5s"的回跳。
 * 服务端字段本身是滞后的，前端 tick = "基线 + 自上次拿到基线后流逝的本地时间"，保证：
 *   1. 进度单调不减；
 *   2. 多次轮询之间也能逐秒看到跳动，不依赖 5s 一次的轮询节奏。
 */
export function useTickingElapsed(baseMs: number, active: boolean): number {
  const [anchor, setAnchor] = useState<{ baseMs: number; baseAt: number }>(() => ({
    baseMs,
    baseAt: Date.now(),
  }));

  // baseMs 变化（轮询/SSE 拉到了新值）→ 重置基线
  useEffect(() => {
    setAnchor({ baseMs, baseAt: Date.now() });
  }, [baseMs]);

  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) {
      // 暂停时刷一次，避免外部时钟漂移导致下次恢复时跳一截
      setNow(Date.now());
      return;
    }
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, [active]);

  if (!active) return baseMs;
  return anchor.baseMs + (now - anchor.baseAt);
}

/**
 * 把毫秒渲染成"11.3s / 1m 23.4s / 800ms"这类紧凑串。
 * 与后端 task.go 里 time_cost 的格式保持一致（< 60s 用 1 位小数）。
 */
export function formatElapsedMs(ms: number): string {
  if (!ms || ms < 0) return '0s';
  if (ms < 1000) return `${ms}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${(s - m * 60).toFixed(1)}s`;
}