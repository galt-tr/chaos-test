/** Presentation helpers for log lines.
 *
 *  The server already parses the pipe-delimited fields; nothing here re-parses them. This
 *  module turns those fields into presentation — a CSS class, a level set for the next
 *  query, a headline/detail split for a wrapped error. `atLeast` in particular has to be
 *  here because it builds the query the UI is about to send. Pure and React-free (the same
 *  shape as txids.ts) so it can be reasoned about on its own.
 */
import type { LogLine } from './api';

export const LEVELS = ['DEBUG', 'INFO', 'WARN', 'ERROR', 'FATAL'] as const;
export type Level = (typeof LEVELS)[number];

const RANK: Record<string, number> = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3, FATAL: 4 };

/** Rank of a level; an unparsed line ranks below DEBUG so it is never filtered away. */
export function rank(level: string): number {
  return RANK[level] ?? -1;
}

/** The levels at or above `min` — what a level filter actually selects. */
export function atLeast(min: string): Level[] {
  const r = RANK[min] ?? 0;
  return LEVELS.filter((l) => RANK[l] >= r);
}

/** Row class: severity must never be colour-only, so this drives a left bar and a coloured
 *  level cell as well as the text itself. */
export function levelClass(level: string): string {
  switch (level) {
    case 'DEBUG': return 'lv-debug';
    case 'INFO': return 'lv-info';
    case 'WARN': return 'lv-warn';
    case 'ERROR': return 'lv-error';
    case 'FATAL': return 'lv-fatal';
    default: return 'lv-raw'; // panics and runtime stderr — the highest-signal lines
  }
}

/** Service tags worth reading when a node will not sync: block validation, p2p, subtree
 *  validation and the blockchain client (FSM notifications). */
export const SYNC_SERVICES = ['bval', 'p2p', 'stval', 'blkcC'];

/** Request tracing and the alert reconnect loop: ~65% of all lines and almost never useful. */
export const NOISE_SERVICES = ['asset', 'rpc', 'bchn', 'blockassembly', 'alert'];

export const SERVICE_PRESETS: { id: string; label: string; value: string; title: string }[] = [
  { id: 'all', label: 'all services', value: '', title: 'no service filter' },
  { id: 'sync', label: 'sync debugging', value: SYNC_SERVICES.join(','), title: `only ${SYNC_SERVICES.join(', ')} — the services that explain a node not syncing` },
  { id: 'quiet', label: 'hide noise', value: NOISE_SERVICES.map((s) => `!${s}`).join(','), title: `everything except ${NOISE_SERVICES.join(', ')}` },
];

/** A teranode error chain. Teranode wraps outward-in, so the LAST element is the cause. */
export type Chain = { head: string; links: string[] };

/** Splits a " -> " causal chain into its terminal cause and the wrapping above it.
 *  Returns null when there is no chain. A message that merely contains " -> " without
 *  being a chain will be mis-split; the untouched text stays one click away in the
 *  expanded row, which is the cheap mitigation for a heuristic worth having. */
export function chain(msg: string): Chain | null {
  const parts = msg.split(' -> ').map((p) => p.trim()).filter(Boolean);
  if (parts.length < 2) return null;
  return { head: parts[parts.length - 1], links: parts.slice(0, -1) };
}

/** hh:mm:ss.mmm — `fmtTime` drops milliseconds, which is unreadable at ~30 lines/second. */
export function logTime(at: string): string {
  const d = new Date(at);
  if (Number.isNaN(d.getTime())) return '';
  const p = (n: number, w = 2) => String(n).padStart(w, '0');
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

/** Client-side view filter over the buffer: newest-first scan capped at `max`, reversed
 *  once so the result stays oldest-first for a bottom-anchored log. Never refetches.
 *
 *  It re-applies level and service as well as the search box, because narrowing the filter
 *  deliberately keeps the buffer instead of refetching — without re-filtering here, raising
 *  the level would leave the now-excluded lines on screen until the next widening.
 *  Unparsed lines are always kept, matching the server. */
export function viewFilter(
  lines: LogLine[],
  opt: { level: string; service: string; q: string },
  max: number,
): LogLine[] {
  const ql = opt.q.trim().toLowerCase();
  const min = opt.level === 'ALL' ? -1 : rank(opt.level);
  const include = new Set(opt.service.split(',').filter((s) => s && !s.startsWith('!')));
  const exclude = new Set(opt.service.split(',').filter((s) => s.startsWith('!')).map((s) => s.slice(1)));
  const out: LogLine[] = [];
  for (let i = lines.length - 1; i >= 0 && out.length < max; i--) {
    const l = lines[i];
    if (l.level !== '') {
      if (rank(l.level) < min) continue;
      if (include.size > 0 && !include.has(l.service ?? '')) continue;
      if (exclude.has(l.service ?? '')) continue;
    }
    if (ql) {
      const hay = (l.msg + ' ' + (l.cont?.join(' ') ?? '')).toLowerCase();
      if (!hay.includes(ql)) continue;
    }
    out.push(l);
  }
  return out.reverse();
}

/** True when `next` selects a subset of what `prev` already fetched, so the buffer can be
 *  re-filtered in place instead of refetched. Widening must refetch: the extra lines were
 *  never delivered. */
export function isNarrowing(
  prev: { level: string; service: string; node: string },
  next: { level: string; service: string; node: string },
): boolean {
  if (prev.node !== next.node) return false;
  if (rank(next.level) < rank(prev.level)) return false; // level lowered = wider
  if (prev.service === next.service) return true;
  if (prev.service === '') return true; // everything was fetched; any subset is narrower
  const prevSet = new Set(prev.service.split(',').filter(Boolean));
  const nextSet = next.service.split(',').filter(Boolean);
  if (prevSet.size === 0 || nextSet.length === 0) return false;
  return nextSet.every((s) => prevSet.has(s));
}
