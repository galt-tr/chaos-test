import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, type DiagPeer, type Diagnostics, type DiagReport, type HealthInfo, type InvalidBlock } from '../api';
import { useLiveCtx } from '../live';
import { ago, useHashParams, useVisible } from '../hooks';
import { Empty, Hash, Json, KV, Linkify, NodeSelect, Pill, Tip } from '../ui';

export function DiagnosticsPage() {
  const { snapshot } = useLiveCtx();
  const params = useHashParams();
  const visible = useVisible();
  const nodes = useMemo(() => snapshot?.nodes ?? [], [snapshot]);

  const [node, setNode] = useState('');
  const [rep, setRep] = useState<DiagReport | null>(null);
  const [err, setErr] = useState('');

  // Open on the node most likely to need explaining: unreachable, then not RUNNING, then
  // furthest behind. Landing on a healthy node when another is broken wastes the click.
  const seeded = useMemo(() => params.get('node'), [params]);
  useEffect(() => {
    if (node || nodes.length === 0) return;
    if (seeded) { setNode(seeded); return; }
    const bad = nodes.find((n) => !n.reachable) ?? nodes.find((n) => n.fsm !== 'RUNNING');
    if (bad) { setNode(bad.name); return; }
    setNode([...nodes].sort((a, b) => a.height - b.height)[0]?.name ?? '');
  }, [nodes, node, seeded]);

  const load = useCallback(async (refresh = false) => {
    if (!node) return;
    try {
      setRep(await api.diagnostics(node, refresh));
      setErr('');
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  }, [node]);

  useEffect(() => { setRep(null); void load(); }, [load]);
  useEffect(() => {
    if (!visible) return;
    const t = setInterval(() => { void load(); }, 3000);
    return () => clearInterval(t);
  }, [load, visible]);

  if (!snapshot) return <div className="panel"><h2>Diagnostics</h2><Empty>waiting for snapshot…</Empty></div>;
  const d = rep?.nodes[0];

  return (
    <>
      <div className="panel">
        <div className="row between">
          <h2 style={{ margin: 0 }}>
            diagnostics <span className="muted lower">{rep ? `checked ${ago(rep.fetchedAt)} ago · ${rep.elapsed}` : 'loading…'}</span>
          </h2>
          <div className="row">
            <NodeSelect nodes={nodes} value={node} onChange={setNode} />
            {node && <a className="btn" href={`#/logs?a=${node}${d?.verdict.logHint?.services ? `&svc=${d.verdict.logHint.services}` : ''}${d?.verdict.logHint?.level ? `&level=${d.verdict.logHint.level}` : ''}`}>logs ↗</a>}
            <button onClick={() => void load(true)}>refresh</button>
          </div>
        </div>
        {err && <div className="error small">{err}</div>}
      </div>

      {!d && !err && <div className="panel"><Empty>probing {node}…</Empty></div>}
      {d && (
        <>
          <VerdictPanel d={d} fleet={rep.fleet} node={node} />
          <div className="two">
            <CatchupPanel d={d} />
            <RejectionPanel blocks={d.invalidBlocks ?? []} ok={d.sources.find((s) => s.name === 'invalid')?.ok ?? false} />
          </div>
          <PeersPanel d={d} maxHeight={rep.fleet.maxHeight} />
          <HealthPanel h={d.health} ok={d.sources.find((s) => s.name === 'health')?.ok ?? false} />
          <SourcesPanel d={d} />
        </>
      )}
    </>
  );
}

/** Maps a verdict severity onto the existing callout classes. */
function boxClass(sev: string) {
  if (sev === 'ok') return 'accepted';
  if (sev === 'warn' || sev === 'info') return 'finding';
  if (sev === 'unknown') return 'finding';
  return 'rejection';
}

function VerdictPanel({ d, fleet, node }: { d: Diagnostics; fleet: DiagReport['fleet']; node: string }) {
  const v = d.verdict;
  const brief = fleet.nodes[node];
  const behind = d.tip ? Math.max(0, fleet.maxHeight - d.tip.height) : 0;
  return (
    <div className="panel">
      <h2>why is {node} in this state?</h2>
      <div className={boxClass(v.severity)}>
        <div className="verdict"><b>{v.headline}</b></div>
        {v.details?.map((x, i) => <div key={i} className="small" style={{ marginTop: 4 }}><Linkify text={x} /></div>)}
        {v.confidence === 'low' && (
          <div className="small warn" style={{ marginTop: 6 }}>
            partial data — {v.missing?.join(', ')} did not answer, so this is a best guess
          </div>
        )}
      </div>
      {v.suggest?.map((s, i) => <div key={i} className="small muted" style={{ marginTop: 6 }}>→ {s}</div>)}
      <dl className="kv" style={{ marginTop: 10 }}>
        <KV k="class"><code>{v.class}</code> <span className="muted small">confidence {v.confidence}</span></KV>
        <KV k="fsm">
          {d.fsm ? <span className={d.fsm.state === 'RUNNING' ? 'ok' : 'warn'}>{d.fsm.state}</span> : <Unknown d={d} src="fsm" />}
          {d.fsm?.legalEvents?.length ? <span className="muted small"> · can {d.fsm.legalEvents.join(', ')}</span> : null}
        </KV>
        <KV k="height">
          {d.tip ? <b>{d.tip.height}</b> : <Unknown d={d} src="tip" />}
          {behind > 0 && <span className="warn small"> · {behind} behind the fleet ({fleet.maxHeight})</span>}
          {behind === 0 && d.tip && <span className="ok small"> · at the fleet tip</span>}
        </KV>
        <KV k="tip">{d.tip ? <Tip hash={d.tip.hash} /> : <Unknown d={d} src="tip" />}</KV>
        <KV k="container">
          <code>{d.container}</code>{' '}
          {d.containerState
            ? <span className={d.containerState === 'running' ? 'ok' : 'bad'}>{d.containerState}</span>
            : <Unknown d={d} src="container" />}
        </KV>
        {brief?.partitions?.length ? <KV k="partitions">{brief.partitions.map((p) => <Pill key={p} ok={false}>{p} plane cut</Pill>)}</KV> : null}
      </dl>
    </div>
  );
}

/** A missing section means UNKNOWN, never "fine" — this renders the reason instead of a
 *  reassuring blank. */
function Unknown({ d, src }: { d: Diagnostics; src: string }) {
  const s = d.sources.find((x) => x.name === src);
  return <span className="muted" title={s?.error}>unknown — {s?.reason ?? 'not probed'}</span>;
}

function CatchupPanel({ d }: { d: Diagnostics }) {
  const src = d.sources.find((s) => s.name === 'catchup');
  const c = d.catchup;
  return (
    <div className="panel">
      <h2>catchup</h2>
      {!src?.ok && <div className="small"><Unknown d={d} src="catchup" /></div>}
      {src?.ok && c && !c.is_catching_up && !c.previous_attempt && (
        <Empty>not catching up — nothing to report</Empty>
      )}
      {src?.ok && c?.is_catching_up && (
        <>
          <div className="row" style={{ marginBottom: 6 }}>
            <Pill ok={null}>catching up</Pill>
            {c.peerNode && <span className="small">from <b>{c.peerNode}</b></span>}
          </div>
          {/* Two bars: the gap between fetched and validated IS the diagnosis when a node
              stalls mid-catchup — blocks arriving but not passing validation. */}
          <Bar label="fetched" n={c.blocks_fetched} total={c.total_blocks} />
          <Bar label="validated" n={c.blocks_validated} total={c.total_blocks} green />
          <dl className="kv" style={{ marginTop: 8 }}>
            <KV k="target">{c.target_block_height} <Hash h={c.target_block_hash} n={8} /></KV>
            <KV k="from height">{c.current_height}</KV>
            {c.fork_depth > 0 && (
              <KV k="fork depth">
                <span className={c.fork_depth > 1 ? 'warn' : ''}>{c.fork_depth}</span>
                {c.fork_depth > 1 && <span className="muted small"> · a reorg, not simple lag</span>}
              </KV>
            )}
            {c.common_ancestor_height > 0 && <KV k="diverged at">{c.common_ancestor_height}</KV>}
            <KV k="running for">{Math.round(c.duration_ms / 1000)}s</KV>
          </dl>
        </>
      )}
      {src?.ok && c?.previous_attempt && (
        <div className="rejection" style={{ marginTop: 8 }}>
          <div className="row">
            <b>last attempt failed</b>
            <Pill ok={false}>{c.previous_attempt.error_type || 'unknown'}</Pill>
            <span className="muted small">{ago(c.previous_attempt.attempt_time * 1000)} ago</span>
          </div>
          <div className="small" style={{ marginTop: 4 }}><Linkify text={c.previous_attempt.error_message} /></div>
          {c.previous_attempt.peerNode && <div className="small muted">from {c.previous_attempt.peerNode}</div>}
        </div>
      )}
    </div>
  );
}

function Bar({ label, n, total, green }: { label: string; n: number; total: number; green?: boolean }) {
  const pct = total > 0 ? Math.min(100, Math.round((n / total) * 100)) : 0;
  return (
    <div>
      <div className="small muted">{label}</div>
      <div className={`bar ${green ? 'val' : ''}`}><i style={{ width: `${pct}%` }} /><b>{n} / {total}</b></div>
    </div>
  );
}

/** Rejections are DURABLE HISTORY, not current state: a node can be fully in sync and still
 *  hold rejections from an earlier fork. So this is a plain panel with ages — never a red
 *  banner, and never the page's verdict. Only rejections the verdict itself cites get
 *  escalated, and that is the backend's call. */
function RejectionPanel({ blocks, ok }: { blocks: InvalidBlock[]; ok: boolean }) {
  return (
    <div className="panel">
      <h2>rejection history <span className="muted lower">{ok ? `${blocks.length} recorded` : 'unknown'}</span></h2>
      {!ok && <div className="small muted">the invalid-blocks probe did not answer</div>}
      {ok && blocks.length === 0 && <Empty>no rejected blocks</Empty>}
      {blocks.length > 0 && (
        <>
          <div className="small muted" style={{ marginBottom: 6 }}>
            Blocks this node refused. A non-empty list is normal after a fork and does not by itself mean the node is unhealthy.
          </div>
          <table className="compact">
            <thead><tr><th>height</th><th>block</th><th>miner</th><th>when</th><th>reason</th></tr></thead>
            <tbody>
              {blocks.map((b) => (
                <tr key={b.hash}>
                  <td>{b.height}</td>
                  <td><Hash h={b.hash} n={8} /></td>
                  <td>{b.minerNode || b.miner}</td>
                  <td className="muted">{b.timestamp ? ago(b.timestamp) : '—'}</td>
                  <td>
                    {b.rejectCode && <code className="code">{b.rejectCode}</code>}
                    {b.rejectRootCause ? <span className="small"> {b.rejectRootCause}</span>
                      : <span className="muted small">{b.rejectCode ? '' : 'not in the recent log window'}</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </div>
  );
}

function PeersPanel({ d, maxHeight }: { d: Diagnostics; maxHeight: number }) {
  const src = d.sources.find((s) => s.name === 'peers');
  const peers = d.peers ?? [];
  const connected = peers.filter((p) => p.is_connected);
  const ahead = connected.filter((p) => d.tip && p.height > d.tip.height);

  return (
    <div className="panel">
      <h2>peers <span className="muted lower">{src?.ok ? `${connected.length} connected of ${peers.length}` : 'unknown'}</span></h2>
      {!src?.ok && <div className="small"><Unknown d={d} src="peers" /></div>}
      {src?.ok && connected.length === 0 && (
        <div className="finding">No connected peers — this is a connectivity problem, not a validation one. Check the p2p plane.</div>
      )}
      {src?.ok && connected.length > 0 && ahead.length === connected.length && d.tip && (
        <div className="finding">
          Every connected peer is ahead of this node, so blocks are being offered and not accepted —
          look for <code>bval</code>/<code>stval</code> errors in the log rather than at the network.
        </div>
      )}
      {peers.length > 0 && (
        <table className="compact">
          <thead><tr><th>peer</th><th>height</th><th>behind</th><th>tip</th><th>state</th><th>ban</th><th>rep</th><th>last catchup error</th></tr></thead>
          <tbody>
            {[...peers].sort((a, b) => Number(b.is_connected) - Number(a.is_connected) || b.height - a.height).map((p) => (
              <PeerRow key={p.id} p={p} maxHeight={maxHeight} />
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function PeerRow({ p, maxHeight }: { p: DiagPeer; maxHeight: number }) {
  const behind = Math.max(0, maxHeight - p.height);
  return (
    <tr className={p.is_connected ? '' : 'off'}>
      <td><b>{p.node || p.client_name || <Hash h={p.id} n={8} />}</b></td>
      <td>{p.height}</td>
      <td>{behind > 0 ? <span className="warn">−{behind}</span> : <span className="muted">—</span>}</td>
      <td><Tip hash={p.block_hash} n={8} /></td>
      <td>{p.is_connected ? <span className="ok">connected</span> : <span className="bad">disconnected</span>}</td>
      {/* A non-zero ban score is expected after ordinary rejections, so it is a warning at
          most. Reputation is the field that makes teranode actually decline a peer. */}
      <td title="a non-zero score is normal after rejections and does not by itself mean a bad peer">
        {p.ban_score ? <span className="warn">{p.ban_score}</span> : <span className="muted">0</span>}
      </td>
      <td>{typeof p.catchup_reputation_score === 'number'
        ? <span className={p.catchup_reputation_score < 50 ? 'bad' : ''}>{p.catchup_reputation_score}</span>
        : <span className="muted">—</span>}</td>
      <td className="small">{p.last_catchup_error ? <span className="bad">{p.last_catchup_error}</span> : <span className="muted">—</span>}</td>
    </tr>
  );
}

/** The raw document repeats the same handful of resources under all 11 services (~105 leaf
 *  checks for ~17 distinct ones). The server de-duplicates; here we show the failures, and
 *  keep the rest behind a disclosure so the page is not a wall of green. */
function HealthPanel({ h, ok }: { h?: HealthInfo; ok: boolean }) {
  const [open, setOpen] = useState(false);
  if (!ok || !h) {
    return <div className="panel"><h2>service health</h2><div className="small muted">the health probe did not answer</div></div>;
  }
  const bad = h.deps.filter((d) => !d.ok);
  const good = h.deps.filter((d) => d.ok);
  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>
          service health <span className="muted lower">{h.services} services · {h.checks} distinct checks</span>
        </h2>
        <Pill ok={bad.length === 0}>{bad.length === 0 ? 'all healthy' : `${bad.length} failing`}</Pill>
      </div>
      {!h.parsed && (
        <div className="finding" style={{ marginTop: 6 }}>
          teranode returned HTTP {h.overallStatus} with a malformed health document (it builds this
          JSON by string concatenation, so an unescaped message breaks it). The status is still authoritative.
          {h.raw && <pre className="small" style={{ marginTop: 4 }}>{h.raw}</pre>}
        </div>
      )}
      {bad.map((d) => (
        <div className="rejection" key={d.resource + d.status} style={{ marginTop: 6 }}>
          <div className="row"><b>{d.resource}</b><Pill ok={false}>HTTP {d.status}</Pill></div>
          {d.error && <div className="small bad">{d.error}</div>}
          {d.message && <div className="small muted">{d.message}</div>}
          {d.seenIn && d.seenIn.length > 0 && (
            <div className="small muted">depended on by {d.seenIn.join(', ')}</div>
          )}
        </div>
      ))}
      {good.length > 0 && (
        <>
          <button className="link" onClick={() => setOpen(!open)} style={{ marginTop: 8 }}>
            {open ? '▾' : '▸'} {good.length} healthy checks
          </button>
          {open && (
            <table className="compact">
              <thead><tr><th>resource</th><th>status</th><th>detail</th><th>services</th></tr></thead>
              <tbody>
                {good.map((d) => (
                  <tr key={d.resource + d.status}>
                    <td>{d.resource}</td>
                    <td className="ok">{d.status}</td>
                    <td className="small muted">{d.message}</td>
                    <td className="small muted">{d.seenIn?.length ?? 0}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </>
      )}
    </div>
  );
}

function SourcesPanel({ d }: { d: Diagnostics }) {
  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>sources <span className="muted lower">probed in {d.elapsed}</span></h2>
        <span className="srcs">
          {d.sources.map((s) => (
            <Pill key={s.name} ok={s.ok} title={s.ok ? `${s.name} · ${s.elapsed}` : `${s.name}: ${s.error}`}>
              {s.name}{s.ok ? '' : ` ✗ ${s.reason ?? ''}`}
            </Pill>
          ))}
        </span>
      </div>
      <div className="small muted" style={{ marginTop: 6 }}>
        A failing source degrades only its own panel; a section that could not be read shows as
        <i> unknown</i> rather than as healthy.
      </div>
      <Json v={d} label="raw diagnostics" />
    </div>
  );
}
