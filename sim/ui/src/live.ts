import { createContext, createElement, useContext, useMemo, type ReactNode } from 'react';
import type { Live } from './hooks';

/** The single SSE-backed live state, provided by <App> and read by every page. */
export const LiveCtx = createContext<Live>({ snapshot: null, events: [], connected: false, lastMessageAt: 0, restarts: 0 });
export function useLiveCtx() { return useContext(LiveCtx); }

/** Full txids the UI has seen in structured data (watched outpoints, event data, alert funds,
 *  a run's variables). Linking hashes found in free text is gated on this set, so block and
 *  alert hashes, which are never in it, stay plain. */
export const TxidsCtx = createContext<Set<string>>(new Set());
export function useKnownTxids() { return useContext(TxidsCtx); }

/** Adds page-specific txids to the known set for its subtree. */
export function KnownTxids({ extra, children }: { extra?: Set<string>; children: ReactNode }) {
  const parent = useContext(TxidsCtx);
  const merged = useMemo(() => {
    if (!extra || extra.size === 0) return parent;
    const s = new Set(parent);
    for (const x of extra) s.add(x);
    return s;
  }, [parent, extra]);
  return createElement(TxidsCtx.Provider, { value: merged }, children);
}

/** Arcade's browser-reachable base URL; '' before the first snapshot or when arcade is not configured. */
export function useArcadeBase(): string {
  const { snapshot } = useLiveCtx();
  const a = snapshot?.arcade;
  return a?.configured && a.hostURL ? a.hostURL.replace(/\/$/, '') : '';
}
