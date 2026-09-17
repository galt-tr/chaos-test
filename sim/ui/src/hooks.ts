import { useCallback, useEffect, useRef, useState } from 'react';
import { api, type Event, type Snapshot } from './api';

export type Live = {
  snapshot: Snapshot | null;
  events: Event[];
  /** true while the SSE stream is open and delivering. */
  connected: boolean;
  /** ms timestamp of the last message (snapshot or event) received over SSE. */
  lastMessageAt: number;
  /** how many times the backend log was observed to have restarted (ids reset). */
  restarts: number;
};

/**
 * Live snapshot + event log over SSE, with a polling fallback.
 *
 * Reconnects after 2s on error. Before each (re)connect the recent log is fetched so events
 * missed while offline are merged; a backend restart (event ids start again from 1) is
 * detected and the local log is replaced rather than silently frozen.
 * While the stream is down the snapshot is refreshed from GET /api/state every 3s.
 */
export function useLive(): Live {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [events, setEvents] = useState<Event[]>([]);
  const [connected, setConnected] = useState(false);
  const [lastMessageAt, setLastMessageAt] = useState(0);
  const [restarts, setRestarts] = useState(0);
  const lastId = useRef(0);
  const known = useRef<Event[]>([]); // mirror of `events` for restart detection without stale closures
  const connectedRef = useRef(false);

  useEffect(() => {
    let es: EventSource | null = null;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const setEv = (evs: Event[]) => { known.current = evs; setEvents(evs); };

    const sync = async () => {
      try {
        const evs = await api.recent(300);
        const newest = evs.length ? evs[evs.length - 1].id : 0;
        const first = evs[0];
        const mine = first ? known.current.find((e) => e.id === first.id) : undefined;
        const restarted = (lastId.current > 0 && newest < lastId.current) || (!!mine && mine.time !== first.time);
        if (restarted) {
          setEv(evs);
          lastId.current = newest;
          setRestarts((n) => n + 1);
          return;
        }
        const fresh = evs.filter((e) => e.id > lastId.current);
        if (fresh.length) {
          setEv([...known.current, ...fresh].slice(-2000));
          lastId.current = newest;
        }
      } catch { /* backend unreachable: the SSE error path retries */ }
    };

    const connect = async () => {
      if (stopped) return;
      await sync();
      if (stopped) return;
      es = new EventSource(`/api/events?after=${lastId.current}`);
      es.onopen = () => { connectedRef.current = true; setConnected(true); };
      es.addEventListener('snapshot', (m) => {
        setSnapshot(JSON.parse((m as MessageEvent).data));
        setLastMessageAt(Date.now());
        if (!connectedRef.current) { connectedRef.current = true; setConnected(true); }
      });
      es.addEventListener('event', (m) => {
        const e: Event = JSON.parse((m as MessageEvent).data);
        if (e.id <= lastId.current) return;
        lastId.current = e.id;
        setLastMessageAt(Date.now());
        setEv([...known.current.slice(-1999), e]);
      });
      es.onerror = () => {
        es?.close();
        es = null;
        connectedRef.current = false;
        setConnected(false);
        timer = setTimeout(connect, 2000);
      };
    };
    void connect();

    // Polling fallback while the stream is down.
    const poll = setInterval(() => {
      if (connectedRef.current) return;
      api.state().then((s) => { setSnapshot(s.snapshot); setLastMessageAt(Date.now()); }).catch(() => {});
    }, 3000);

    return () => { stopped = true; es?.close(); if (timer) clearTimeout(timer); clearInterval(poll); };
  }, []);
  return { snapshot, events, connected, lastMessageAt, restarts };
}

export function useInterval(fn: () => void, ms: number) {
  useEffect(() => { fn(); const t = setInterval(fn, ms); return () => clearInterval(t); }, [fn, ms]);
}

/** Re-renders the caller every `ms` (for "Ns ago" labels). */
export function useTick(ms = 1000) {
  const [, set] = useState(0);
  useEffect(() => { const t = setInterval(() => set((n) => n + 1), ms); return () => clearInterval(t); }, [ms]);
}

/** Hash-based route: `#/fleet` → "fleet". Defaults to "fleet". */
/**
 * Query parameters carried in the hash, e.g. `#/logs?a=teranode1&b=teranode2`.
 *
 * useRoute deliberately keeps only the first path segment, so anything after `?` never
 * reaches it. Pages that want deep links (the Diagnostics page links into Logs with
 * filters preselected) read them here instead.
 */
export function useHashParams(): URLSearchParams {
  const read = () => new URLSearchParams(window.location.hash.replace(/^#\/?[^?]*\??/, ''));
  const [p, setP] = useState(read);
  useEffect(() => {
    const f = () => setP(read());
    window.addEventListener('hashchange', f);
    return () => window.removeEventListener('hashchange', f);
  }, []);
  return p;
}

/** Whether the tab is visible. Polling pauses when it is not, so a forgotten tab does not
 *  keep reading container logs all afternoon. */
export function useVisible(): boolean {
  const [vis, setVis] = useState(() => !document.hidden);
  useEffect(() => {
    const f = () => setVis(!document.hidden);
    document.addEventListener('visibilitychange', f);
    return () => document.removeEventListener('visibilitychange', f);
  }, []);
  return vis;
}

export function useRoute(fallback = 'fleet') {
  const read = () => {
    const h = window.location.hash.replace(/^#\/?/, '');
    return h.split(/[?/]/)[0] || fallback;
  };
  const [route, setRoute] = useState(read);
  useEffect(() => {
    const f = () => setRoute(read());
    window.addEventListener('hashchange', f);
    return () => window.removeEventListener('hashchange', f);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return route;
}

export type ActionState = { busy: boolean; ok?: string; err?: string; at?: number };

/**
 * Keyed async action runner: `run('mine:teranode1', () => api.mine(...), r => 'mined 3')`.
 * Every call records busy / ok / err for inline status text next to the button.
 */
export function useActions() {
  const [status, setStatus] = useState<Record<string, ActionState>>({});
  const run = useCallback(async <T,>(key: string, fn: () => Promise<T>, ok?: (r: T) => string): Promise<T | undefined> => {
    setStatus((s) => ({ ...s, [key]: { busy: true } }));
    try {
      const r = await fn();
      setStatus((s) => ({ ...s, [key]: { busy: false, ok: ok ? ok(r) : 'ok', at: Date.now() } }));
      return r;
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setStatus((s) => ({ ...s, [key]: { busy: false, err: msg, at: Date.now() } }));
      return undefined;
    }
  }, []);
  const clear = useCallback((key: string) => setStatus((s) => { const n = { ...s }; delete n[key]; return n; }), []);
  return { status, run, clear };
}

export function short(h?: string, n = 10) { return h ? `${h.slice(0, n)}…${h.slice(-4)}` : ''; }
export function ago(iso: string | number) {
  const t = typeof iso === 'number' ? iso : new Date(iso).getTime();
  const s = Math.max(0, Math.round((Date.now() - t) / 1000));
  return s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s / 60)}m` : `${Math.round(s / 3600)}h`;
}
export function fmtTime(iso?: string) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return d.toLocaleTimeString([], { hour12: false });
}
/** Deterministic colour from a hash: nodes on the same tip get the same swatch. */
export function hashColor(hash?: string) {
  if (!hash) return 'transparent';
  const h = parseInt(hash.slice(0, 6), 16) || 0;
  const hue = h % 360;
  const light = 45 + ((h >> 9) % 20);
  return `hsl(${hue} 70% ${light}%)`;
}

/** Event kinds published on the orchestrator bus (others may appear; the feed adds them dynamically). */
export const EVENT_KINDS = ['tip', 'alert_seq', 'utxo', 'rejected_tx', 'invalid_block', 'chaos', 'alert', 'mine', 'tx', 'scenario', 'log', 'error'];

export function copy(text: string) {
  try { void navigator.clipboard?.writeText(text); } catch { /* clipboard unavailable (insecure context) */ }
}
