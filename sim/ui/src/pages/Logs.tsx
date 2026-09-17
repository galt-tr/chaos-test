import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { api, type LogLine, type LogPage, type NodeState } from '../api';
import { useLiveCtx } from '../live';
import { useHashParams, useVisible } from '../hooks';
import { Empty, Linkify, NodeSelect, Pill } from '../ui';
import { LEVELS, SERVICE_PRESETS, chain, isNarrowing, levelClass, logTime, viewFilter } from '../logparse';

/** Lines held per pane. Generous: at ~5 lines/s (INFO+) this is roughly 15 minutes. */
const BUF = 4000;
/** Rendered rows per pane. Beyond this the browser, not the data, becomes the limit. */
const RENDER = 1500;

type Filter = { node: string; level: string; service: string };

export function LogsPage() {
  const { snapshot } = useLiveCtx();
  const params = useHashParams();
  const visible = useVisible();
  const nodes = useMemo(() => snapshot?.nodes ?? [], [snapshot]);

  const [a, setA] = useState('');
  const [b, setB] = useState('');
  const [level, setLevel] = useState('INFO');
  const [service, setService] = useState('');
  const [q, setQ] = useState('');
  const [wrap, setWrap] = useState(false);
  const [follow, setFollow] = useState(true);
  const [paused, setPaused] = useState(false);

  const [buf, setBuf] = useState<Record<string, LogLine[]>>({});
  const [frozen, setFrozen] = useState<Record<string, LogLine[]> | null>(null);
  const [meta, setMeta] = useState<Record<string, LogPage>>({});
  const [errs, setErrs] = useState<Record<string, { error: string; reason?: string }>>({});

  const cursors = useRef<Record<string, string>>({});
  // A ref, not state: load() is an async callback and would otherwise close over a stale
  // value, which is exactly how a buffer trim ends up yanking a scrolled-up view.
  const followRef = useRef(follow);
  const prevFilter = useRef<Filter[]>([]);
  const panes = useRef<Record<string, HTMLDivElement | null>>({});

  const logsAvailable = (snapshot as unknown as { logs?: { available: boolean; reason?: string } })?.logs?.available ?? true;

  // Deep link from the Diagnostics page: #/logs?a=teranode2&level=WARN&svc=bval,p2p
  const seeded = useRef(false);
  useEffect(() => {
    if (seeded.current || nodes.length === 0) return;
    seeded.current = true;
    const pa = params.get('a');
    const pb = params.get('b');
    if (params.get('level')) setLevel(params.get('level') as string);
    if (params.get('svc')) setService(params.get('svc') as string);
    if (params.get('grep')) setQ(params.get('grep') as string);
    if (pa) { setA(pa); setB(pb ?? ''); return; }
    // Otherwise open on the most interesting pairing: the node furthest behind, against
    // one at the fleet tip.
    // Pane A: whichever node most needs looking at. Pane B: a DIFFERENT node to compare
    // against — when the fleet is converged the lowest and highest are the same node, and
    // defaulting to one pane would hide the comparison the page is for.
    const reach = nodes.filter((n) => n.reachable);
    const pool = reach.length > 0 ? reach : nodes;
    const byHeight = [...pool].sort((x, y) => x.height - y.height);
    const first = byHeight[0]?.name ?? '';
    setA(first);
    setB(byHeight.reverse().find((n) => n.name !== first)?.name ?? '');
  }, [nodes, params]);

  const active = useMemo(() => [a, b].filter(Boolean), [a, b]);

  const load = useCallback(async () => {
    await Promise.all(active.map(async (node) => {
      try {
        const cursor = cursors.current[node];
        const page = await api.logs({ node, cursor, level, service, limit: 2000, tail: 800 });
        cursors.current[node] = page.nextCursor;
        setMeta((m) => ({ ...m, [node]: page }));
        setErrs((e) => { if (!e[node]) return e; const n = { ...e }; delete n[node]; return n; });
        if (page.lines.length === 0 && !page.reset) return;
        setBuf((prev) => {
          const merged = page.reset ? page.lines : [...(prev[node] ?? []), ...page.lines];
          // Trim ONLY while pinned to the bottom. Appending never disturbs what is above,
          // but dropping from the front shifts every row up — under a reader who has
          // scrolled up to look at something, that is the view jumping out from under them.
          const next = followRef.current && merged.length > BUF ? merged.slice(-BUF) : merged;
          return { ...prev, [node]: next };
        });
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        let reason: string | undefined;
        try { reason = (JSON.parse(msg) as { reason?: string }).reason; } catch { /* plain message */ }
        setErrs((e) => ({ ...e, [node]: { error: msg, reason } }));
      }
    }));
  }, [active, level, service]);

  // Reset when the filter WIDENS: the extra lines were never fetched, so the buffer and
  // cursor must be discarded. Narrowing re-filters the buffer in place (see `shown`).
  // Declared before the polling effect so a reset and its refetch land in one commit.
  useEffect(() => {
    const next: Filter[] = active.map((node) => ({ node, level, service }));
    const prev = prevFilter.current;
    const widened = next.length !== prev.length || next.some((f, i) => !prev[i] || !isNarrowing(prev[i], f));
    prevFilter.current = next;
    if (!widened) return;
    cursors.current = {};
    setBuf({});
    setMeta({});
    setFrozen(null);
    setPaused(false);
    setFollow(true);
    followRef.current = true;
  }, [active, level, service]);

  // Polling continues while paused so the "+N new" count is honest and resuming is instant.
  useEffect(() => {
    if (!visible || active.length === 0 || !logsAvailable) return;
    void load();
    const ms = paused ? 5000 : 1500;
    const t = setInterval(() => { void load(); }, ms);
    return () => clearInterval(t);
  }, [load, paused, visible, active.length, logsAvailable]);

  const setFollowing = (v: boolean) => { followRef.current = v; setFollow(v); };

  const pinAll = () => {
    setFollowing(true);
    for (const el of Object.values(panes.current)) {
      if (el) el.scrollTop = el.scrollHeight;
    }
  };

  const togglePause = () => {
    if (paused) { setFrozen(null); setPaused(false); pinAll(); }
    else { setFrozen(buf); setPaused(true); }
  };

  const view = frozen ?? buf;
  const pending = frozen
    ? active.reduce((n, k) => n + Math.max(0, (buf[k]?.length ?? 0) - (frozen[k]?.length ?? 0)), 0)
    : 0;

  if (!snapshot) return <div className="panel"><h2>Logs</h2><Empty>waiting for snapshot…</Empty></div>;

  if (!logsAvailable) {
    return (
      <div className="panel">
        <h2>Logs</h2>
        <div className="finding">
          <b>Container logs need the podman socket.</b>
          <div className="small" style={{ marginTop: 6 }}>
            The orchestrator reports runtime <code>none</code>, so it cannot read container logs. Run{' '}
            <code>systemctl --user enable --now podman.socket</code> and recreate the orchestrator with{' '}
            <code>make sim-up</code>.
          </div>
        </div>
      </div>
    );
  }

  return (
    <>
      <div className="panel">
        <div className="row between">
          <h2 style={{ margin: 0 }}>
            logs <span className="muted lower">{active.length ? active.join(' vs ') : 'pick a node'}</span>
          </h2>
          <div className="row">
            <label>level
              <select value={level} onChange={(e) => setLevel(e.target.value)}
                title="minimum severity. teranode logs ~75% DEBUG, so INFO+ is the usable default">
                {LEVELS.map((l) => <option key={l} value={l}>{l}+</option>)}
                <option value="ALL">ALL</option>
              </select>
            </label>
            <label>services
              <select value={service} onChange={(e) => setService(e.target.value)}>
                {SERVICE_PRESETS.map((p) => <option key={p.id} value={p.value} title={p.title}>{p.label}</option>)}
              </select>
            </label>
            <label>search
              <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="filter shown lines"
                style={{ width: 170 }} title="filters the lines already loaded; does not refetch" />
            </label>
            <button onClick={togglePause} className={paused ? 'active' : ''}
              title="freeze the view; polling continues so nothing is missed">
              {paused ? `resume${pending ? ` (+${pending})` : ''}` : 'pause'}
            </button>
            <button onClick={pinAll} disabled={follow} className={follow ? '' : 'primary'}
              title={follow ? 'following the newest lines' : 'jump back to the newest lines'}>
              {follow ? 'tailing ↓' : 'jump to newest ↓'}
            </button>
            <button onClick={() => setWrap(!wrap)} className={wrap ? 'active' : ''} title="wrap long lines">wrap</button>
          </div>
        </div>
      </div>

      <div className={b ? 'two' : ''}>
        {[a, b].map((node, i) => (node || i === 0) && (
          <Pane
            key={i} node={node} nodes={nodes} onNode={i === 0 ? setA : setB} allowEmpty={i === 1}
            lines={view[node] ?? []} q={q} wrap={wrap} follow={follow} onFollow={setFollowing}
            meta={meta[node]} err={errs[node]} level={level} service={service}
            bind={(el) => { panes.current[node] = el; }}
          />
        ))}
      </div>
    </>
  );
}

function Pane({ node, nodes, onNode, allowEmpty, lines, q, wrap, follow, onFollow, meta, err, level, service, bind }: {
  node: string; nodes: NodeState[]; onNode: (v: string) => void; allowEmpty: boolean;
  lines: LogLine[]; q: string; wrap: boolean; follow: boolean; onFollow: (v: boolean) => void;
  meta?: LogPage; err?: { error: string; reason?: string }; level: string; service: string;
  bind: (el: HTMLDivElement | null) => void;
}) {
  const el = useRef<HTMLDivElement | null>(null);
  const [open, setOpen] = useState<number | null>(null);
  const shown = useMemo(() => viewFilter(lines, { level, service, q }, RENDER), [lines, level, service, q]);
  const n = nodes.find((x) => x.name === node);

  // A layout effect, not an effect: the scroll lands after the new rows are in the DOM but
  // before paint, so tailing never flashes the previous position.
  useLayoutEffect(() => {
    if (follow && el.current) el.current.scrollTop = el.current.scrollHeight;
  }, [shown, follow, open, wrap]);

  const onScroll = () => {
    const e = el.current;
    if (!e) return;
    const atBottom = e.scrollHeight - e.scrollTop - e.clientHeight < 24;
    if (atBottom !== follow) onFollow(atBottom); // only on a real flip, not per scroll event
  };

  return (
    <div className="panel logpane">
      <div className="row between" style={{ marginBottom: 6 }}>
        <NodeSelect nodes={nodes} value={node} onChange={onNode} allowEmpty={allowEmpty} />
        <span className="row small">
          {n && <span className="muted">h {n.height}</span>}
          {n && <span className={n.fsm === 'RUNNING' ? 'ok' : 'warn'}>{n.fsm || '—'}</span>}
          {n?.partitions?.map((p) => <Pill key={p} ok={false}>{p} cut</Pill>)}
          {meta?.truncated && <Pill ok={false} title="the read hit its size ceiling; older lines in this window were dropped">truncated</Pill>}
          {node && <a className="small" href={`#/diag?node=${node}`} title="why is this node in this state?">diagnose ↗</a>}
        </span>
      </div>

      {err && (
        <div className="error small" style={{ marginBottom: 6 }}>
          {err.reason === 'container_missing' ? `container for ${node} is gone` : err.error}
        </div>
      )}

      {meta && (
        <div className="small muted" style={{ marginBottom: 4 }}>
          {shown.length} shown · {lines.length} held · {meta.scanned} scanned last poll
          {Object.entries(meta.levels).filter(([k]) => k !== level).map(([k, v]) => (
            <span key={k} title={`${v} ${k} line(s) in the last window, hidden by the level filter`}> · {v} {k}</span>
          ))}
        </div>
      )}

      <div className={`logs ${wrap ? 'wrapped' : ''}`} ref={(e) => { el.current = e; bind(e); }} onScroll={onScroll}>
        {shown.length === 0 && <Empty>{node ? 'no lines match — try a lower level or a different service preset' : 'pick a node'}</Empty>}
        {shown.map((l, i) => {
          const prev = shown[i - 1];
          const hidden = prev ? l.seq - prev.seq - 1 : 0;
          return (
            <div key={l.seq}>
              {hidden > 0 && <div className="lggap">· {hidden} line{hidden === 1 ? '' : 's'} hidden by the filter ·</div>}
              <Row line={l} open={open === l.seq} onToggle={() => setOpen(open === l.seq ? null : l.seq)} />
            </div>
          );
        })}
      </div>
    </div>
  );
}

function Row({ line, open, onToggle }: { line: LogLine; open: boolean; onToggle: () => void }) {
  const c = chain(line.msg);
  const headline = line.rootCause || (c ? c.head : line.msg);
  const expandable = !!(line.cont?.length || c);
  return (
    <div className={`lg ${levelClass(line.level)} ${open ? 'open' : ''}`} onClick={onToggle} title={line.source}>
      <span className="t">{logTime(line.at)}</span>
      <span className="l">{line.level || 'RAW'}</span>
      <span className="s">{line.service}</span>
      <span className="m">
        {line.code && <code className="code">{line.code}</code>}{line.code ? ' ' : ''}
        <Linkify text={headline} />
        {!open && expandable && <span className="muted"> ▸</span>}
      </span>
      {open && (
        <pre className="evdata">
          {line.source ? `${line.source}\n` : ''}{line.logAt ? `${line.logAt}\n` : ''}
          {c ? c.links.map((x, i) => `${'  '.repeat(i)}${x}\n`).join('') : ''}
          {c ? `${'  '.repeat(c.links.length)}${c.head}` : line.msg}
          {line.cont?.length ? `\n${line.cont.join('\n')}` : ''}
        </pre>
      )}
    </div>
  );
}
