import { useMemo, useState, type ReactNode } from 'react';
import { isSV, type Event, type NodeState, type Snapshot } from '../api';
import { useLiveCtx } from '../live';
import { EVENT_KINDS, ago, fmtTime, hashColor, useActions } from '../hooks';
import { NodeControls, type ActionProps } from '../nodeactions';
import { AutoMinePanel } from '../automine';
import { Empty, Hash, JsonTree, Linkify, Pill, Tip, TxList } from '../ui';

/** For rejected-tx verdicts the `hash` field is a txid. */
const REJECTED_TX_KEYS = new Set(['hash']);

export function FleetPage() {
  const { snapshot, events, connected } = useLiveCtx();
  // Must precede the !snapshot early return below, or the hook order changes between renders.
  const { status, run } = useActions();
  if (!snapshot) {
    return (
      <div className="panel">
        <h2>Fleet</h2>
        <Empty>{connected ? 'Waiting for the first snapshot…' : 'No snapshot yet — the orchestrator at http://localhost:8600 is not reachable. Retrying.'}</Empty>
      </div>
    );
  }
  return (
    <>
      <Consensus snap={snapshot} />
      <AutoMinePanel nodes={snapshot.nodes} />
      <div className="grid nodes">
        {snapshot.nodes.map((n) => <NodeCard key={n.name} n={n} status={status} run={run} />)}
        <HubCard snap={snapshot} />
        <ArcadeCard snap={snapshot} />
        <ServicesCard snap={snapshot} />
      </div>
      <EventFeed events={events} nodes={snapshot.nodes} />
    </>
  );
}

function Consensus({ snap }: { snap: Snapshot }) {
  const tips = new Map<string, string[]>();
  for (const n of snap.nodes) {
    if (!n.reachable || !n.tip) continue;
    tips.set(n.tip, [...(tips.get(n.tip) ?? []), n.name]);
  }
  const unreachable = snap.nodes.filter((n) => !n.reachable).map((n) => n.name);
  const partitioned = snap.nodes.filter((n) => n.partitions?.length).map((n) => `${n.name} (${n.partitions!.join(', ')})`);
  const heights = new Set(snap.nodes.filter((n) => n.reachable).map((n) => n.height));
  return (
    <div className="panel consensus">
      <div className="row between">
        <div className="row">
          <span className="muted">chain tips</span>
          {tips.size === 0 && <span className="bad">no reachable node</span>}
          {[...tips.entries()].map(([tip, names]) => (
            <span key={tip} className="tipwrap" title={tip}>
              <span className="tip" style={{ background: hashColor(tip) }} /><Hash h={tip} n={8} />
              <span className="muted small">← {names.join(', ')}</span>
            </span>
          ))}
          {tips.size > 1 ? <Pill ok={false}>fork: {tips.size} distinct tips</Pill>
            : tips.size === 1 ? <Pill ok>all reachable nodes agree{heights.size > 1 ? ' (heights differ)' : ''}</Pill> : null}
          <ArcadeChip snap={snap} tips={tips} />
        </div>
        <div className="row small muted">
          {unreachable.length > 0 && <span className="bad">unreachable: {unreachable.join(', ')}</span>}
          {partitioned.length > 0 && <span className="warn">partitioned: {partitioned.join('; ')}</span>}
          <span>hub seq {snap.hub.reachable ? `#${snap.hub.sequence}` : 'n/a'}</span>
        </div>
      </div>
    </div>
  );
}

function NodeCard({ n, status, run }: ActionProps) {
  const seq = n.alertSeq < 0 ? 'n/a' : `#${n.alertSeq}`;
  const sv = isSV(n);
  const unprocessed = n.alertUnprocessed ?? -1;
  return (
    <div className={`panel node ${n.reachable ? '' : 'down'}`}>
      <div className="row between">
        <h2 style={{ margin: 0 }}>
          {n.name} <span className="muted lower">{sv ? `SV Node${n.version ? ` · ${n.version}` : ''}` : n.version ? `v${n.version}` : ''}</span>
        </h2>
        <span className="row">
          {n.partitions?.map((p) => <Pill key={p} ok={false} title={`${p} plane disconnected${sv && p === 'alert' ? ' (sidecar)' : ''}`}>{p} plane cut</Pill>)}
          <Pill ok={n.reachable}>{n.reachable ? 'reachable' : 'unreachable'}</Pill>
          {n.hostURL && (
            <a className="btn" href={n.hostURL} target="_blank" rel="noreferrer"
              title={`open ${n.name}'s asset dashboard in a new tab`}>open ↗</a>
          )}
        </span>
      </div>
      <dl className="kv">
        {sv
          ? <><dt>peers</dt><dd><b>{n.reachable ? n.peers ?? 0 : '—'}</b> <span className="muted small">outbound to the teranodes' legacy service</span></dd></>
          : <><dt>FSM</dt><dd><span className={n.fsm === 'RUNNING' ? 'ok' : n.fsm ? 'warn' : 'muted'}>{n.fsm || '—'}</span></dd></>}
        <dt>height</dt><dd><b>{n.reachable ? n.height : '—'}</b></dd>
        <dt>tip</dt><dd><Tip hash={n.tip} /></dd>
        <dt>mempool</dt><dd>{n.mempoolCount}{n.mempool?.length ? <TxList txids={n.mempool} total={n.mempoolCount} /> : null}</dd>
        <dt>{sv ? 'alert seq (sidecar)' : 'alert seq'}</dt>
        <dd>
          <b>{seq}</b>{' '}
          <span className={`small ${n.alertReachable ? 'ok' : 'muted'}`} title={sv ? 'the go-alert-system sidecar that applies alerts to this SV node over RPC, probed over the alert p2p network' : 'alert-system p2p endpoint reachable from the orchestrator'}>
            {n.alertReachable ? 'alert p2p up' : 'alert p2p unreachable'}
          </span>
          {sv && unprocessed > 0 && <Pill ok={false} className="small" title="alerts the sidecar received but could not apply to the SV node over RPC (retried every 30 s)"> {unprocessed} unprocessed</Pill>}
          {sv && n.sidecarURL && <a className="small" href={`${n.sidecarURL}/health`} target="_blank" rel="noreferrer" style={{ marginLeft: 6 }}>sidecar health ↗</a>}
        </dd>
        <dt>container</dt><dd><code>{n.container}</code>{sv && n.sidecarContainer ? <> <span className="muted small">+</span> <code>{n.sidecarContainer}</code></> : null}</dd>
        <dt>updated</dt><dd className="muted">{ago(n.updatedAt)} ago</dd>
      </dl>
      <NodeControls n={n} status={status} run={run} />
      {n.error && <div className="error small" title={n.error}>{n.error}</div>}
    </div>
  );
}

function HubCard({ snap }: { snap: Snapshot }) {
  const h = snap.hub;
  return (
    <div className={`panel node ${h.reachable ? '' : 'down'}`}>
      <div className="row between">
        <h2 style={{ margin: 0 }}>alert hub <span className="muted lower">go-alert-system</span></h2>
        <Pill ok={h.reachable}>{h.reachable ? 'reachable' : 'unreachable'}</Pill>
      </div>
      <dl className="kv">
        <dt>sequence</dt><dd><b>{h.reachable ? `#${h.sequence}` : '—'}</b></dd>
        <dt>active peers</dt><dd>{h.activePeers}</dd>
        <dt>unprocessed</dt><dd className={h.unprocessed ? 'warn' : ''}>{h.unprocessed}</dd>
      </dl>
      {h.error && <div className="error small" title={h.error}>{h.error}</div>}
      <h2 style={{ marginTop: 10 }}>node sequences</h2>
      <table className="compact">
        <tbody>
          {snap.nodes.map((n) => (
            <tr key={n.name}>
              <td>{n.name}</td>
              <td className={n.alertSeq < 0 ? 'muted' : n.alertSeq < h.sequence ? 'warn' : 'ok'}>{n.alertSeq < 0 ? 'n/a' : `#${n.alertSeq}`}</td>
              <td className="muted small">{n.alertSeq >= 0 && h.reachable && n.alertSeq < h.sequence ? `${h.sequence - n.alertSeq} behind hub` : ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Arcade's chaintracks tip next to the node tips (kept out of the fork test: a lagging arcade is not a fork). */
function ArcadeChip({ snap, tips }: { snap: Snapshot; tips: Map<string, string[]> }) {
  const a = snap.arcade;
  if (!a?.configured) return null;
  if (!a.tip) return <span className="muted small">arcade: {a.reachable ? 'no chaintracks tip yet' : 'unreachable'}</span>;
  const agree = tips.has(a.tip);
  return (
    <span className="tipwrap" title={`arcade chaintracks tip @${a.height}${a.reachable ? '' : ' (arcade unreachable, last known)'}`}>
      <span className="tip" style={{ background: hashColor(a.tip) }} /><Hash h={a.tip} n={8} />
      <span className={`small ${agree ? 'muted' : 'warn'}`}>← arcade{agree ? '' : ` @${a.height}`}</span>
    </span>
  );
}

function ArcadeCard({ snap }: { snap: Snapshot }) {
  const a = snap.arcade;
  if (!a?.configured) {
    return <div className="panel node"><h2>arcade</h2><Empty>arcade is not in the inventory</Empty></div>;
  }
  const live = snap.nodes.filter((n) => n.reachable && n.tip);
  const same = live.filter((n) => n.tip.toLowerCase() === (a.tip ?? '').toLowerCase()).map((n) => n.name);
  const maxH = live.reduce((m, n) => Math.max(m, n.height), 0);
  let agree: ReactNode;
  if (!a.tip) agree = <span className="muted">—</span>;
  else if (live.length === 0) agree = <span className="muted">no reachable node to compare with</span>;
  else if (same.length) agree = <Pill ok>in sync with {same.join(', ')}</Pill>;
  else if (a.height < maxH) agree = <Pill className="warn" title="chaintracks has not seen the nodes' latest block(s) yet">behind by {maxH - a.height}</Pill>;
  else if (a.height > maxH) agree = <Pill className="warn">ahead of the reachable nodes</Pill>;
  else agree = <Pill ok={false}>different hash at the same height</Pill>;
  const ct = a.hostChaintracksURL;
  return (
    <div className={`panel node ${a.reachable ? '' : 'down'}`}>
      <div className="row between">
        <h2 style={{ margin: 0 }}>arcade <span className="muted lower">{a.version ? `v${a.version} · ` : ''}broadcaster · chaintracks</span></h2>
        <span className="row">
          <Pill ok={a.reachable}>{a.reachable ? 'reachable' : 'unreachable'}</Pill>
          {a.reachable && <Pill ok={a.healthy}>{a.healthy ? 'healthy' : 'unhealthy'}</Pill>}
          {a.hostURL && <a className="btn" href={a.hostURL} target="_blank" rel="noreferrer" title="open arcade in a new tab">open ↗</a>}
        </span>
      </div>
      <dl className="kv">
        <dt>status</dt><dd>{a.reachable ? a.status || '—' : <span className="muted">—</span>}</dd>
        <dt>height</dt><dd><b>{a.reachable ? a.blockHeight : '—'}</b> <span className="muted small">arcade's own view (/health)</span></dd>
        <dt>chaintracks tip</dt><dd>{a.tip ? <><Tip hash={a.tip} /> <span className="muted small">@ {a.height}</span></> : <span className="muted">—</span>}</dd>
        <dt>vs nodes</dt><dd>{agree}</dd>
        <dt>updated</dt><dd className="muted">{a.updatedAt ? `${ago(a.updatedAt)} ago` : '—'}</dd>
      </dl>
      {a.datahubs?.length ? (
        <table className="compact" style={{ marginTop: 8 }}>
          <thead><tr><th>datahub</th><th>source</th><th></th></tr></thead>
          <tbody>
            {a.datahubs.map((d) => (
              <tr key={d.url}>
                <td title={d.url}>{d.node ? <b>{d.node}</b> : <code className="small">{d.url}</code>}</td>
                <td className="muted small">{d.source ?? ''}</td>
                <td><Pill ok={d.healthy}>{d.healthy ? 'healthy' : 'unhealthy'}</Pill></td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
      <div className="links">
        {a.hostURL && <a href={`${a.hostURL}/health`} target="_blank" rel="noreferrer">health</a>}
        {ct && <a href={`${ct}/chaintracks/v2/tip`} target="_blank" rel="noreferrer">chaintracks tip</a>}
        {a.hostEventsURL && <a href={`${a.hostEventsURL}/events`} target="_blank" rel="noreferrer">events (SSE)</a>}
        {a.hostURL && <a href={`${a.hostURL}/policy`} target="_blank" rel="noreferrer">policy</a>}
      </div>
      {a.error && <div className="error small" title={a.error}>{a.error}</div>}
      {a.tipError && <div className="error small" title={a.tipError}>chaintracks: {a.tipError}</div>}
    </div>
  );
}

function ServicesCard({ snap }: { snap: Snapshot }) {
  const svcs = snap.services ?? [];
  return (
    <div className="panel node">
      <h2>services</h2>
      {svcs.length === 0 && <Empty>no services configured</Empty>}
      <table className="compact">
        <tbody>
          {svcs.map((s) => (
            <tr key={s.name}>
              <td><b>{s.name}</b></td>
              <td><Pill ok={s.healthy}>{s.healthy ? 'healthy' : 'unhealthy'}</Pill></td>
              <td><a href={s.hostURL ?? s.url} target="_blank" rel="noreferrer" className="small" title={s.hostURL ? `in-network: ${s.url}` : undefined}>{s.hostURL ?? s.url}</a></td>
            </tr>
          ))}
        </tbody>
      </table>
      {svcs.some((s) => s.detail) && (
        <div className="small muted" style={{ marginTop: 6 }}>
          {svcs.filter((s) => s.detail).map((s) => <div key={s.name} className="ellipsis" title={s.detail}><span className="muted">{s.name}:</span> <code>{s.detail}</code></div>)}
        </div>
      )}
    </div>
  );
}

export function EventFeed({ events, nodes, title = 'events', max = 400, initialKind = '', compact = false }:
  { events: Event[]; nodes: NodeState[]; title?: string; max?: number; initialKind?: string; compact?: boolean }) {
  const [kind, setKind] = useState(initialKind);
  const [node, setNode] = useState('');
  const [q, setQ] = useState('');
  const [paused, setPaused] = useState(false);
  const [frozen, setFrozen] = useState<Event[] | null>(null);
  const [open, setOpen] = useState<number | null>(null);

  const source = frozen ?? events;
  const kinds = useMemo(() => {
    const s = new Set(EVENT_KINDS);
    for (const e of events) s.add(e.kind);
    return [...s];
  }, [events]);
  const nodeNames = useMemo(() => {
    const s = new Set<string>(nodes.map((n) => n.name));
    for (const e of events) if (e.node) s.add(e.node);
    return [...s];
  }, [events, nodes]);
  const filtered = useMemo(() => {
    const ql = q.trim().toLowerCase();
    const out: Event[] = [];
    for (let i = source.length - 1; i >= 0 && out.length < max; i--) {
      const e = source[i];
      if (kind && e.kind !== kind) continue;
      if (node && (e.node ?? '') !== node) continue;
      if (ql && !e.message.toLowerCase().includes(ql)) continue;
      out.push(e);
    }
    return out;
  }, [source, kind, node, q, max]);
  const pending = frozen ? events.filter((e) => e.id > (frozen[frozen.length - 1]?.id ?? 0)).length : 0;
  const togglePause = () => {
    if (paused) { setFrozen(null); setPaused(false); } else { setFrozen(events); setPaused(true); }
  };

  return (
    <div className="panel">
      <div className="row between feed-controls">
        <h2 style={{ margin: 0 }}>{title} <span className="muted lower">{filtered.length}{filtered.length === max ? '+' : ''} shown · {events.length} buffered</span></h2>
        <div className="row">
          <select value={kind} onChange={(e) => setKind(e.target.value)} title="filter by kind">
            <option value="">all kinds</option>
            {kinds.map((k) => <option key={k} value={k}>{k}</option>)}
          </select>
          <select value={node} onChange={(e) => setNode(e.target.value)} title="filter by node">
            <option value="">all nodes</option>
            {nodeNames.map((k) => <option key={k} value={k}>{k}</option>)}
          </select>
          <input placeholder="search message" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: 160 }} />
          <button onClick={togglePause} className={paused ? 'active' : ''}>{paused ? `resume${pending ? ` (+${pending} new)` : ''}` : 'pause'}</button>
        </div>
      </div>
      <div className={`events ${compact ? 'compact' : ''}`}>
        {filtered.length === 0 && <Empty>no events{kind || node || q ? ' match the filter' : ' yet'}</Empty>}
        {filtered.map((e) => (
          <div key={e.id} className={`ev kind-${e.kind} ${open === e.id ? 'open' : ''}`} onClick={() => setOpen(open === e.id ? null : e.id)} title={e.time}>
            <span className="muted">{fmtTime(e.time)}</span>
            <span className="k">{e.kind}</span>
            <span className="n">{e.node ?? ''}</span>
            <span className="m"><Linkify text={e.message} all={e.kind === 'tx'} /></span>
            {open === e.id && (
              <pre className="evdata">#{e.id} {e.time}{'\n'}{e.data ? <JsonTree v={e.data} txKeys={e.kind === 'rejected_tx' ? REJECTED_TX_KEYS : undefined} /> : '(no data)'}</pre>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
