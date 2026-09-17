import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, type AlertSummary } from '../api';
import { KnownTxids, useLiveCtx } from '../live';
import { fmtTime, useActions } from '../hooks';
import { Empty, Hash, NodeSelect, Status, TxLink } from '../ui';

const TYPES = ['freeze', 'unfreeze', 'informational', 'invalidateblock', 'confiscate', 'ban', 'unban'] as const;
type AlertType = (typeof TYPES)[number];

type FundRow = { txid: string; vout: string; start: string; stop: string; policyExpires: boolean };
const emptyFund = (): FundRow => ({ txid: '', vout: '0', start: '0', stop: '0', policyExpires: false });

export function AlertsPage() {
  const { snapshot, events } = useLiveCtx();
  const nodes = snapshot?.nodes ?? [];
  const [alerts, setAlerts] = useState<AlertSummary[] | null>(null);
  const [alertHost, setAlertHost] = useState<boolean | null>(null);
  const [loadErr, setLoadErr] = useState('');
  const { status, run } = useActions();

  const refresh = useCallback(() => {
    api.state()
      .then((s) => { setAlerts(s.alerts ?? []); setAlertHost(s.alertHost); setLoadErr(''); })
      .catch((e: Error) => setLoadErr(e.message));
  }, []);
  // Refresh whenever a new `alert` event arrives, plus a slow safety poll.
  const lastAlertEv = useMemo(() => { for (let i = events.length - 1; i >= 0; i--) if (events[i].kind === 'alert') return events[i].id; return 0; }, [events]);
  useEffect(() => { refresh(); }, [refresh, lastAlertEv]);
  useEffect(() => { const t = setInterval(refresh, 15000); return () => clearInterval(t); }, [refresh]);

  const latest = alerts?.reduce((m, a) => Math.max(m, a.sequence), 0) ?? 0;
  const sorted = useMemo(() => [...(alerts ?? [])].sort((a, b) => b.sequence - a.sequence), [alerts]);
  const fundTxids = useMemo(() => new Set((alerts ?? []).flatMap((a) => a.funds?.map((f) => f.txid.toLowerCase()) ?? [])), [alerts]);

  return (
    <KnownTxids extra={fundTxids}>
      <div className="panel">
        <div className="row between">
          <h2 style={{ margin: 0 }}>alert log <span className="muted lower">{alerts ? `${alerts.length} built · latest #${latest}` : 'loading…'}</span></h2>
          <div className="row small">
            {alertHost === false && <span className="warn" title="The orchestrator is not attached to alertnet, so POST /api/alerts/push will fail. Alerts can still be built and applied via RPC.">alert host disabled — pushes unavailable</span>}
            {alertHost === true && <span className="ok">alert host on alertnet</span>}
            <span className="muted">hub #{snapshot?.hub.sequence ?? '?'}</span>
            <button onClick={refresh}>refresh</button>
          </div>
        </div>
        {loadErr && <div className="error small">{loadErr}</div>}
        {alerts && alerts.length === 0 && <Empty>No alerts built yet. Use the form below to build and sign one; it is appended to the orchestrator's log and can then be pushed to nodes.</Empty>}
        {sorted.length > 0 && (
          <table className="compact">
            <thead><tr><th>#</th><th>type</th><th>funds</th><th>text</th><th>note</th><th>hash</th><th>created</th></tr></thead>
            <tbody>
              {sorted.map((a) => (
                <tr key={a.sequence}>
                  <td><b>#{a.sequence}</b></td>
                  <td><span className="pill">{a.type}</span></td>
                  <td>
                    {a.funds?.length ? a.funds.map((f) => (
                      <div key={`${f.txid}:${f.vout}`} className="small">
                        <TxLink txid={f.txid} n={8} />:<b>{f.vout}</b>{' '}
                        <span className="muted" title="[enforceAtHeightStart, enforceAtHeightStop)">[{f.enforceAtHeightStart}, {f.enforceAtHeightStop}){f.policyExpiresWithConsensus ? ' policy-expires' : ''}</span>
                      </div>
                    )) : <span className="muted">—</span>}
                  </td>
                  <td className="wrap">{a.text}</td>
                  <td className="muted">{a.note ?? ''}</td>
                  <td><Hash h={a.hash} n={8} /></td>
                  <td className="muted">{fmtTime(a.createdAt)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="two">
        <BuildForm onBuilt={refresh} heights={nodes.map((n) => n.height)} />
        <div>
          <div className="panel">
            <h2>push to nodes <span className="muted lower">POST /api/alerts/push — node pulls everything it lacks up to the sequence</span></h2>
            {nodes.length === 0 && <Empty>no nodes in snapshot</Empty>}
            {nodes.length > 0 && (
              <table className="compact">
                <thead><tr><th>node</th><th>node seq</th><th>log latest</th><th>push</th><th></th></tr></thead>
                <tbody>
                  {nodes.map((n) => <PushRow key={n.name} name={n.name} nodeSeq={n.alertSeq} latest={latest} alertReachable={n.alertReachable}
                    st={status[`push:${n.name}`]} onPush={(seq) => run(`push:${n.name}`, () => api.pushAlert(n.name, seq),
                      (r) => { const d = r.delivered ?? r.Delivered ?? []; return d.length ? `delivered #${d.join(', #')}` : 'nothing requested (node already up to date?)'; })} />)}
                </tbody>
              </table>
            )}
          </div>
          <RpcForm nodes={nodes.map((n) => n.name)} />
        </div>
      </div>
    </KnownTxids>
  );
}

function PushRow({ name, nodeSeq, latest, alertReachable, st, onPush }:
  { name: string; nodeSeq: number; latest: number; alertReachable: boolean; st?: ReturnType<typeof useActions>['status'][string]; onPush: (seq: number) => void }) {
  const [seq, setSeq] = useState<string>('');
  useEffect(() => { setSeq(String(latest)); }, [latest]);
  const target = Number(seq) || latest;
  const behind = nodeSeq >= 0 && nodeSeq < target;
  return (
    <tr>
      <td><b>{name}</b> {!alertReachable && <span className="muted small" title="alert p2p endpoint not reachable from orchestrator">(alert p2p down)</span>}</td>
      <td className={nodeSeq < 0 ? 'muted' : behind ? 'warn' : 'ok'}>{nodeSeq < 0 ? 'n/a' : `#${nodeSeq}`}</td>
      <td>#{latest}</td>
      <td>
        <span className="row" style={{ gap: 4 }}>
          <input type="number" min={1} value={seq} onChange={(e) => setSeq(e.target.value)} style={{ width: 64 }} />
          <button disabled={!latest || st?.busy} onClick={() => onPush(target)} title={`push alerts up to #${target} to ${name}`}>push up to #{target}</button>
        </span>
      </td>
      <td><Status st={st} /></td>
    </tr>
  );
}

function BuildForm({ onBuilt, heights }: { onBuilt: () => void; heights: number[] }) {
  const { snapshot } = useLiveCtx();
  const { status, run } = useActions();
  const [type, setType] = useState<AlertType>('freeze');
  const [funds, setFunds] = useState<FundRow[]>([emptyFund()]);
  const [message, setMessage] = useState('');
  const [blockHash, setBlockHash] = useState('');
  const [reason, setReason] = useState('');
  const [enforceAt, setEnforceAt] = useState('0');
  const [txHex, setTxHex] = useState('');
  const [peer, setPeer] = useState('');
  const [note, setNote] = useState('');
  const [watch, setWatch] = useState(true);
  const [sequence, setSequence] = useState('');
  const maxH = heights.length ? Math.max(...heights) : 0;
  const watched = snapshot?.watched ?? [];

  const setFund = (i: number, patch: Partial<FundRow>) => setFunds((f) => f.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const isFunds = type === 'freeze' || type === 'unfreeze';

  const submit = () => {
    const body: Record<string, unknown> = { type, note, watch };
    if (sequence.trim()) body.sequence = Number(sequence);
    if (isFunds) {
      body.funds = funds.filter((f) => f.txid.trim()).map((f) => ({
        txid: f.txid.trim(), vout: Number(f.vout) || 0,
        enforceAtHeightStart: Number(f.start) || 0, enforceAtHeightStop: Number(f.stop) || 0,
        policyExpiresWithConsensus: f.policyExpires,
      }));
    } else if (type === 'informational') body.message = message;
    else if (type === 'invalidateblock') { body.blockHash = blockHash.trim(); body.reason = reason; }
    else if (type === 'confiscate') { body.enforceAt = Number(enforceAt) || 0; body.txHex = txHex.trim(); }
    else { body.peer = peer.trim(); body.reason = reason; }
    void run('build', () => api.buildAlert(body), (r) => `built #${r.sequence}${r.typeName ? ` (${r.typeName})` : ''}: ${r.text}`).then((r) => { if (r) onBuilt(); });
  };

  return (
    <div className="panel">
      <h2>build &amp; sign alert <span className="muted lower">POST /api/alerts/build — signed with the 3 genesis keys, appended to the log</span></h2>
      <div className="row">
        <label>type
          <select value={type} onChange={(e) => setType(e.target.value as AlertType)}>{TYPES.map((t) => <option key={t}>{t}</option>)}</select>
        </label>
        <label>sequence <span className="muted">(blank = latest+1)</span>
          <input type="number" min={1} value={sequence} onChange={(e) => setSequence(e.target.value)} style={{ width: 90 }} placeholder="auto" />
        </label>
        <label>note
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="free text stored with the alert" style={{ width: 260 }} />
        </label>
      </div>

      {isFunds && (
        <fieldset>
          <legend>funds ({funds.filter((f) => f.txid).length}) — fleet height now {maxH || '?'}</legend>
          <table className="compact">
            <thead><tr><th>txid</th><th>vout</th><th>start</th><th>stop</th><th title="policyExpiresWithConsensus">policy exp.</th><th></th></tr></thead>
            <tbody>
              {funds.map((f, i) => (
                <tr key={i}>
                  <td><input value={f.txid} onChange={(e) => setFund(i, { txid: e.target.value.trim() })} placeholder="64 hex" className="mono" style={{ width: 300 }} /></td>
                  <td><input type="number" min={0} value={f.vout} onChange={(e) => setFund(i, { vout: e.target.value })} style={{ width: 56 }} /></td>
                  <td><input type="number" min={0} value={f.start} onChange={(e) => setFund(i, { start: e.target.value })} style={{ width: 72 }} title="enforceAtHeightStart (0 = now)" /></td>
                  <td><input type="number" min={0} value={f.stop} onChange={(e) => setFund(i, { stop: e.target.value })} style={{ width: 72 }} title="enforceAtHeightStop (0 = forever)" /></td>
                  <td><input type="checkbox" checked={f.policyExpires} onChange={(e) => setFund(i, { policyExpires: e.target.checked })} /></td>
                  <td><button onClick={() => setFunds((x) => x.filter((_, j) => j !== i))} disabled={funds.length === 1}>−</button></td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="row" style={{ marginTop: 6 }}>
            <button onClick={() => setFunds((x) => [...x, emptyFund()])}>+ fund</button>
            {watched.length > 0 && (
              <select value="" onChange={(e) => { const w = watched[Number(e.target.value)]; if (w) setFunds((x) => [...x.filter((f) => f.txid), { ...emptyFund(), txid: w.txid, vout: String(w.vout) }]); }}>
                <option value="">+ from watched outpoint…</option>
                {watched.map((w, i) => <option key={`${w.txid}:${w.vout}`} value={i}>{w.label ? `${w.label} — ` : ''}{w.txid.slice(0, 12)}…:{w.vout}</option>)}
              </select>
            )}
            <button onClick={() => setFunds((x) => x.map((f) => ({ ...f, start: String(maxH + 2), stop: String(maxH + 12) })))} disabled={!maxH} title="window = [tip+2, tip+12]">window tip+2…+12</button>
            <label className="row" style={{ flexDirection: 'row', alignItems: 'center' }}><input type="checkbox" checked={watch} onChange={(e) => setWatch(e.target.checked)} /> watch these outpoints</label>
          </div>
        </fieldset>
      )}
      {type === 'informational' && (
        <fieldset><legend>informational</legend>
          <label>message<textarea value={message} onChange={(e) => setMessage(e.target.value)} rows={2} /></label>
        </fieldset>
      )}
      {type === 'invalidateblock' && (
        <fieldset><legend>invalidate block</legend>
          <div className="row">
            <label>block hash<input value={blockHash} onChange={(e) => setBlockHash(e.target.value)} className="mono" style={{ width: 520 }} placeholder="64 hex" /></label>
            <label>reason<input value={reason} onChange={(e) => setReason(e.target.value)} style={{ width: 240 }} /></label>
          </div>
          {snapshot && (
            <div className="row small" style={{ marginTop: 6 }}>
              <span className="muted">tips:</span>
              {snapshot.nodes.filter((n) => n.tip).map((n) => <button key={n.name} className="link" onClick={() => setBlockHash(n.tip)}>{n.name} tip @{n.height}</button>)}
            </div>
          )}
        </fieldset>
      )}
      {type === 'confiscate' && (
        <fieldset><legend>confiscate</legend>
          <label>enforce at height<input type="number" value={enforceAt} onChange={(e) => setEnforceAt(e.target.value)} style={{ width: 100 }} /></label>
          <label>confiscation tx hex<textarea className="hex" value={txHex} onChange={(e) => setTxHex(e.target.value)} /></label>
        </fieldset>
      )}
      {(type === 'ban' || type === 'unban') && (
        <fieldset><legend>{type} peer</legend>
          <div className="row">
            <label>peer<input value={peer} onChange={(e) => setPeer(e.target.value)} className="mono" style={{ width: 420 }} placeholder="peer id / multiaddr" /></label>
            <label>reason<input value={reason} onChange={(e) => setReason(e.target.value)} style={{ width: 240 }} /></label>
          </div>
        </fieldset>
      )}
      <div className="row">
        <button className="primary" onClick={submit} disabled={status.build?.busy}>build &amp; sign</button>
        <Status st={status.build} />
      </div>
    </div>
  );
}

function RpcForm({ nodes }: { nodes: string[] }) {
  const { snapshot } = useLiveCtx();
  const { status, run } = useActions();
  const [node, setNode] = useState(nodes[0] ?? '');
  const [action, setAction] = useState<'freeze' | 'unfreeze'>('freeze');
  const [txid, setTxid] = useState('');
  const [vout, setVout] = useState('0');
  const [start, setStart] = useState('');
  const [stop, setStop] = useState('');
  const [policyExpires, setPolicyExpires] = useState(false);
  useEffect(() => { if (!node && nodes[0]) setNode(nodes[0]); }, [nodes, node]);
  const submit = () => run('rpc', () => api.rpcFreeze({
    node, action, txid: txid.trim(), vout: Number(vout) || 0,
    start: start.trim() === '' ? undefined : Number(start), stop: stop.trim() === '' ? undefined : Number(stop), policyExpires,
  }), () => `${action} ${txid.slice(0, 10)}…:${vout} on ${node} via RPC (outpoint now watched)`);
  return (
    <div className="panel">
      <h2>RPC {action} <span className="muted lower">POST /api/alerts/rpc — node admin RPC, bypasses the alert network</span></h2>
      <div className="row">
        <label>node<NodeSelect nodes={snapshot?.nodes ?? []} value={node} onChange={setNode} /></label>
        <label>action<select value={action} onChange={(e) => setAction(e.target.value as 'freeze' | 'unfreeze')}><option>freeze</option><option>unfreeze</option></select></label>
        <label>txid<input value={txid} onChange={(e) => setTxid(e.target.value)} className="mono" style={{ width: 240 }} placeholder="64 hex" /></label>
        <label>vout<input type="number" min={0} value={vout} onChange={(e) => setVout(e.target.value)} style={{ width: 56 }} /></label>
      </div>
      <div className="row" style={{ marginTop: 6 }}>
        {action === 'freeze' && <>
          <label>start<input type="number" value={start} onChange={(e) => setStart(e.target.value)} style={{ width: 72 }} placeholder="none" /></label>
          <label>stop<input type="number" value={stop} onChange={(e) => setStop(e.target.value)} style={{ width: 72 }} placeholder="none" /></label>
          <label>policy exp.<input type="checkbox" checked={policyExpires} onChange={(e) => setPolicyExpires(e.target.checked)} /></label>
        </>}
        <button className="primary" onClick={submit} disabled={!txid || status.rpc?.busy}>{action} via RPC</button>
      </div>
      <Status st={status.rpc} />
    </div>
  );
}
