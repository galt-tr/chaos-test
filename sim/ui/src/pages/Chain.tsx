import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, type Event, type KeyEntry } from '../api';
import { useLiveCtx } from '../live';
import { copy, fmtTime, useActions } from '../hooks';
import { Empty, Hash, Json, Linkify, NodeSelect, Status, StatusPill, TxLink } from '../ui';

type UTXO = { txid: string; vout: number; lockingScript: string; satoshis: number; utxoHash: string; status: string; spendingData?: { txId: string; vin: number } | null; lockTime: number };
type TxMeta = { frozen: boolean; freezeRecords?: Record<string, unknown> | null; blockIDs?: number[] | null; blockHeights?: number[] | null; isCoinbase: boolean; conflicting: boolean; locked: boolean; unminedSince: number };
type TxLookup = { node: string; txid: string; txmeta?: TxMeta; txmetaError?: string; utxos?: UTXO[] | null; utxosError?: string; hex?: string };

export function ChainPage() {
  const { snapshot } = useLiveCtx();
  const nodes = snapshot?.nodes ?? [];
  const nodeNames = nodes.map((n) => n.name);
  const [keys, setKeys] = useState<KeyEntry[]>([]);
  const [keysErr, setKeysErr] = useState('');
  const [source, setSource] = useState<{ txid: string; vout: number } | null>(null);
  const [lookupTxid, setLookupTxid] = useState('');

  const loadKeys = useCallback(() => api.keys().then((k) => { setKeys(k ?? []); setKeysErr(''); }).catch((e: Error) => setKeysErr(e.message)), []);
  useEffect(() => { void loadKeys(); }, [loadKeys]);

  return (
    <>
      <WatchedMatrix nodeNames={nodeNames} onLookup={setLookupTxid} onSpend={setSource} />
      <div className="two">
        <TxLookup nodeNames={nodeNames} txid={lookupTxid} setTxid={setLookupTxid} onSpend={setSource} />
        <SpendTool nodeNames={nodeNames} keys={keys} reloadKeys={loadKeys} source={source} onLookup={setLookupTxid} />
      </div>
      <KeysPanel keys={keys} err={keysErr} reload={loadKeys} />
    </>
  );
}

function WatchedMatrix({ nodeNames, onLookup, onSpend }: { nodeNames: string[]; onLookup: (txid: string) => void; onSpend: (s: { txid: string; vout: number }) => void }) {
  const { snapshot } = useLiveCtx();
  const watched = snapshot?.watched ?? [];
  const { status, run } = useActions();
  const [txid, setTxid] = useState('');
  const [vout, setVout] = useState('0');
  const [label, setLabel] = useState('');
  const add = () => run('watch', () => api.watch(txid.trim(), Number(vout) || 0, label.trim() || undefined), () => `watching ${txid.slice(0, 10)}…:${vout}`).then((r) => { if (r) { setTxid(''); setLabel(''); } });
  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>watched outpoints <span className="muted lower">{watched.length} · status polled on every node by the orchestrator</span></h2>
        <div className="row">
          <input value={txid} onChange={(e) => setTxid(e.target.value.trim())} placeholder="txid (64 hex)" className="mono" style={{ width: 300 }} />
          <input type="number" min={0} value={vout} onChange={(e) => setVout(e.target.value)} style={{ width: 56 }} title="vout" />
          <input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="label" style={{ width: 120 }} />
          <button className="primary" onClick={add} disabled={txid.length !== 64 || status.watch?.busy}>watch</button>
          <Status st={status.watch} />
        </div>
      </div>
      {watched.length === 0 && <Empty>No watched outpoints. Add one above, build a freeze alert with "watch" ticked, or apply an RPC freeze — each adds the outpoint here.</Empty>}
      {watched.length > 0 && (
        <table className="matrix compact">
          <thead>
            <tr><th>outpoint</th><th>label</th>{nodeNames.map((n) => <th key={n}>{n}</th>)}<th></th></tr>
          </thead>
          <tbody>
            {watched.map((w) => {
              const key = `${w.txid}:${w.vout}`;
              const statuses = new Set(Object.values(w.perNode ?? {}).map((v) => v.status));
              return (
                <tr key={key}>
                  <td><TxLink txid={w.txid} n={12} />:<b>{w.vout}</b>{statuses.size > 1 && <span className="warn small" title="nodes disagree on this outpoint"> ≠</span>}</td>
                  <td className="lbl">{w.label ?? ''}</td>
                  {nodeNames.map((n) => {
                    const v = w.perNode?.[n];
                    return (
                      <td key={n}>
                        {v ? <>
                          <StatusPill status={v.status} />
                          {v.spentBy && <span className="small muted"> by <TxLink txid={v.spentBy} n={6} /></span>}
                          {v.satoshis ? <span className="small muted"> {v.satoshis} sat</span> : null}
                          {v.error && <span className="small bad" title={v.error}> !</span>}
                        </> : <span className="muted">—</span>}
                      </td>
                    );
                  })}
                  <td>
                    <span className="row" style={{ gap: 4 }}>
                      <button className="link" onClick={() => onLookup(w.txid)}>lookup</button>
                      <button className="link" onClick={() => onSpend({ txid: w.txid, vout: w.vout })}>spend</button>
                      <button className="link" onClick={() => run(`unwatch:${key}`, () => api.unwatch(w.txid, w.vout), () => 'removed')}>unwatch</button>
                      <Status st={status[`unwatch:${key}`]} />
                    </span>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function TxLookup({ nodeNames, txid, setTxid, onSpend }: { nodeNames: string[]; txid: string; setTxid: (t: string) => void; onSpend: (s: { txid: string; vout: number }) => void }) {
  const { snapshot } = useLiveCtx();
  const [node, setNode] = useState('');
  const [res, setRes] = useState<TxLookup | null>(null);
  const [cbHeight, setCbHeight] = useState('1');
  const { status, run } = useActions();
  const nodeOr = node || nodeNames[0] || '';

  const lookup = useCallback((t: string, n: string) => {
    if (t.length !== 64) return;
    void run('lookup', () => api.tx(t, n || undefined) as Promise<TxLookup>, (r) => `${r.node}: ${r.txmeta ? 'txmeta ok' : 'no txmeta'}, ${r.utxos ? `${r.utxos.length} outputs` : 'no utxos'}`).then((r) => { if (r) setRes(r); });
  }, [run]);
  // When another panel hands us a txid, look it up straight away.
  useEffect(() => { if (txid.length === 64) lookup(txid, nodeOr); }, [txid]); // eslint-disable-line react-hooks/exhaustive-deps

  const coinbase = () => run('coinbase', () => api.coinbase(nodeOr, Number(cbHeight) || 0), (r) => `block ${r.height} coinbase ${r.txid.slice(0, 12)}…`).then((r) => { if (r?.txid) setTxid(r.txid); });
  const watchOut = (u: UTXO) => run(`watch:${u.vout}`, () => api.watch(res!.txid, u.vout, `lookup ${res!.txid.slice(0, 8)}`), () => 'watching');
  const m = res?.txmeta;

  return (
    <div className="panel">
      <h2>transaction lookup <span className="muted lower">GET /api/tx/{'{txid}'}?node=</span></h2>
      <div className="row">
        <input value={txid} onChange={(e) => setTxid(e.target.value.trim())} placeholder="txid (64 hex)" className="mono" style={{ width: 380 }} onKeyDown={(e) => { if (e.key === 'Enter') lookup(txid, nodeOr); }} />
        <NodeSelect nodes={snapshot?.nodes ?? []} value={nodeOr} onChange={setNode} />
        <button className="primary" onClick={() => lookup(txid, nodeOr)} disabled={txid.length !== 64 || status.lookup?.busy}>lookup</button>
        <Status st={status.lookup} />
      </div>
      <div className="row small" style={{ marginTop: 6 }}>
        <span className="muted">coinbase of block</span>
        <input type="number" min={0} value={cbHeight} onChange={(e) => setCbHeight(e.target.value)} style={{ width: 70 }} />
        <button onClick={coinbase} disabled={!nodeOr || status.coinbase?.busy}>fetch coinbase txid</button>
        <Status st={status.coinbase} />
      </div>

      {res && (
        <div style={{ marginTop: 10 }}>
          <div className="small muted">on <b>{res.node}</b> · <TxLink txid={res.txid} n={16} /></div>
          {res.txmetaError && <div className="error small">txmeta: {res.txmetaError}</div>}
          {m && (
            <dl className="kv">
              <dt>frozen</dt><dd className={m.frozen ? 'status-FROZEN' : ''}>{String(m.frozen)}</dd>
              <dt>coinbase</dt><dd>{String(m.isCoinbase)}</dd>
              <dt>block heights</dt><dd>{m.blockHeights?.length ? m.blockHeights.join(', ') : <span className="warn">unmined</span>}{m.blockIDs?.length ? <span className="muted small"> (ids {m.blockIDs.join(', ')})</span> : null}</dd>
              <dt>conflicting</dt><dd className={m.conflicting ? 'bad' : ''}>{String(m.conflicting)}</dd>
              <dt>locked</dt><dd>{String(m.locked)}</dd>
              <dt>unmined since</dt><dd>{m.unminedSince || '—'}</dd>
              {m.freezeRecords && Object.keys(m.freezeRecords).length > 0 && <><dt>freeze records</dt><dd><Json v={m.freezeRecords} open label={`${Object.keys(m.freezeRecords).length} record(s)`} /></dd></>}
            </dl>
          )}
          {res.utxosError && <div className="error small">utxos: {res.utxosError}</div>}
          {res.utxos && (
            <table className="compact" style={{ marginTop: 8 }}>
              <thead><tr><th>vout</th><th>sats</th><th>status</th><th>spent by</th><th>script</th><th></th></tr></thead>
              <tbody>
                {res.utxos.map((u) => (
                  <tr key={u.vout}>
                    <td><b>{u.vout}</b></td>
                    <td>{u.satoshis}</td>
                    <td><StatusPill status={u.status} /></td>
                    <td>{u.spendingData?.txId ? <><TxLink txid={u.spendingData.txId} n={8} /> <span className="muted small">vin {u.spendingData.vin}</span></> : <span className="muted">—</span>}</td>
                    <td><code className="small" title={u.lockingScript}>{u.lockingScript.slice(0, 20)}…</code></td>
                    <td>
                      <span className="row" style={{ gap: 4 }}>
                        <button className="link" onClick={() => onSpend({ txid: res.txid, vout: u.vout })}>spend</button>
                        <button className="link" onClick={() => watchOut(u)}>watch</button>
                        <Status st={status[`watch:${u.vout}`]} />
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {res.hex && <Json v={res.hex} label={`raw hex (${res.hex.length / 2} bytes)`} />}
        </div>
      )}
    </div>
  );
}

/** Key names can be long (victim-<txid>); keep option labels readable. */
function keyLabel(k: KeyEntry) {
  const name = k.name.length > 28 ? `${k.name.slice(0, 14)}…${k.name.slice(-10)}` : k.name;
  return `${name} (${k.address.slice(0, 8)}…)`;
}

function SpendTool({ nodeNames, keys, reloadKeys, source, onLookup }:
  { nodeNames: string[]; keys: KeyEntry[]; reloadKeys: () => Promise<void>; source: { txid: string; vout: number } | null; onLookup: (t: string) => void }) {
  const { snapshot, events } = useLiveCtx();
  const { status, run } = useActions();
  const [node, setNode] = useState('');
  const [txid, setTxid] = useState('');
  const [vout, setVout] = useState('0');
  const [key, setKey] = useState('miner');
  const [to, setTo] = useState('');
  const [toCustom, setToCustom] = useState('');
  const [satoshis, setSatoshis] = useState('0');
  const [fee, setFee] = useState('500');
  const [outputs, setOutputs] = useState('1');
  const [newName, setNewName] = useState('');
  const [built, setBuilt] = useState<{ txid: string; hex: string; efHex: string; size?: number } | null>(null);
  const [target, setTarget] = useState('');
  const [submitted, setSubmitted] = useState<{ target: string; accepted: boolean; status: number; body?: string; txid?: string } | null>(null);
  useEffect(() => { if (source) { setTxid(source.txid); setVout(String(source.vout)); setBuilt(null); setSubmitted(null); } }, [source]);
  useEffect(() => { if (!keys.find((k) => k.name === key) && keys[0]) setKey(keys[0].name); }, [keys, key]);
  const nodeOr = node || nodeNames[0] || '';
  const targetOr = target || nodeNames[0] || 'arcade';
  const dest = to === '__custom' ? toCustom.trim() : to;

  const newKey = () => run('newkey', () => api.newKey(newName.trim(), 'created from UI'), (k) => `key ${k.name} → ${k.address}`).then((k) => { if (k) { void reloadKeys(); setTo(k.name); setNewName(''); } });
  const build = () => run('build', () => api.spend({
    node: nodeOr, txid: txid.trim(), vout: Number(vout) || 0, key, to: dest || undefined,
    satoshis: Number(satoshis) || 0, fee: Number(fee) || 0, outputs: Number(outputs) || 1,
  }), (r) => `built ${r.txid.slice(0, 12)}… (${r.size ?? r.hex.length / 2} bytes)`).then((r) => { if (r) { setBuilt(r); setSubmitted(null); } });
  const submit = () => built && run('submit', () => api.submit(targetOr, built.hex, built.efHex),
    (r) => (r.accepted ? `accepted by ${targetOr} (HTTP ${r.status})` : `rejected by ${targetOr} (HTTP ${r.status})`))
    .then((r) => { if (r) setSubmitted({ target: targetOr, accepted: r.accepted, status: r.status, body: r.body, txid: r.txid }); });

  // Events about the built tx (verdicts arrive asynchronously via Kafka → rejected_tx).
  const related = useMemo<Event[]>(() => {
    if (!built) return [];
    const id = built.txid;
    return events.filter((e) => ['rejected_tx', 'tx', 'utxo', 'invalid_block'].includes(e.kind)
      && (e.message.includes(id) || e.message.includes(id.slice(0, 16)) || e.data?.hash === id || e.data?.txid === id)).slice(-8).reverse();
  }, [events, built]);

  return (
    <div className="panel">
      <h2>spend &amp; submit <span className="muted lower">POST /api/tx/spend → /api/tx/submit</span></h2>
      <fieldset>
        <legend>1 · source outpoint &amp; keys</legend>
        <div className="row">
          <label>fetch from<NodeSelect nodes={snapshot?.nodes ?? []} value={nodeOr} onChange={setNode} /></label>
          <label>source txid<input value={txid} onChange={(e) => setTxid(e.target.value.trim())} className="mono" style={{ width: 300 }} placeholder="64 hex" /></label>
          <label>vout<input type="number" min={0} value={vout} onChange={(e) => setVout(e.target.value)} style={{ width: 56 }} /></label>
        </div>
        <div className="row" style={{ marginTop: 6 }}>
          <label>unlock with key
            <select value={key} onChange={(e) => setKey(e.target.value)}>
              {keys.map((k) => <option key={k.name} value={k.name}>{keyLabel(k)}</option>)}
              {keys.length === 0 && <option value="miner">miner</option>}
            </select>
          </label>
          <label>pay to
            <select value={to} onChange={(e) => setTo(e.target.value)}>
              <option value="">(same key)</option>
              {keys.map((k) => <option key={k.name} value={k.name}>{keyLabel(k)}</option>)}
              <option value="__custom">custom name / hex / WIF…</option>
            </select>
          </label>
          {to === '__custom' && <label>custom<input value={toCustom} onChange={(e) => setToCustom(e.target.value)} className="mono" style={{ width: 240 }} /></label>}
          <label>new key
            <span className="row" style={{ gap: 4 }}>
              <input value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="name" style={{ width: 120 }} />
              <button onClick={newKey} disabled={!newName.trim() || status.newkey?.busy}>create</button>
            </span>
          </label>
          <Status st={status.newkey} />
        </div>
        <div className="row" style={{ marginTop: 6 }}>
          <label>satoshis <span className="muted">(0 = all − fee)</span><input type="number" min={0} value={satoshis} onChange={(e) => setSatoshis(e.target.value)} style={{ width: 110 }} /></label>
          <label>fee<input type="number" min={0} value={fee} onChange={(e) => setFee(e.target.value)} style={{ width: 80 }} /></label>
          <label>outputs<input type="number" min={1} value={outputs} onChange={(e) => setOutputs(e.target.value)} style={{ width: 60 }} title="split over N outputs to the same destination" /></label>
          <button className="primary" onClick={build} disabled={txid.length !== 64 || status.build?.busy}>build &amp; sign</button>
          <Status st={status.build} />
        </div>
      </fieldset>

      {built && (
        <fieldset>
          <legend>2 · built transaction</legend>
          <div className="row small">
            <span>txid <TxLink txid={built.txid} n={16} /></span>
            <button className="link" onClick={() => copy(built.hex)}>copy hex</button>
            <button className="link" onClick={() => copy(built.efHex)}>copy EF hex</button>
            <button className="link" onClick={() => onLookup(built.txid)}>lookup</button>
            <button className="link" onClick={() => run(`watchbuilt`, () => api.watch(built.txid, 0, 'spend output 0'), () => 'watching :0')}>watch :0</button>
            <Status st={status.watchbuilt} />
          </div>
          <Json v={built.hex} label={`hex (${built.hex.length / 2} B)`} />
          <Json v={built.efHex} label={`EF hex (${built.efHex.length / 2} B)`} />
          <div className="row" style={{ marginTop: 6 }}>
            <label>submit to
              <select value={targetOr} onChange={(e) => setTarget(e.target.value)}>
                {nodeNames.map((n) => <option key={n} value={n}>{n} (asset POST /tx)</option>)}
                <option value="arcade">arcade (EF hex)</option>
              </select>
            </label>
            <button className="primary" onClick={submit} disabled={status.submit?.busy}>submit</button>
            <Status st={status.submit} />
          </div>
        </fieldset>
      )}

      {submitted && (
        <div className={submitted.accepted ? 'accepted' : 'rejection'}>
          <b>{submitted.accepted ? 'ACCEPTED' : 'REJECTED'}</b> by {submitted.target} — HTTP {submitted.status}
          {submitted.txid && <> · txid <TxLink txid={submitted.txid} n={12} /></>}
          {submitted.body ? <pre style={{ color: 'inherit', marginTop: 6 }}><Linkify text={submitted.body} /></pre> : <div className="small" style={{ opacity: .8 }}>(empty body{!submitted.accepted ? ' — the rejection reason arrives on the rejectedtx topic, see events below' : ''})</div>}
        </div>
      )}
      {built && related.length > 0 && (
        <div style={{ marginTop: 8 }}>
          <h3>events for this tx</h3>
          {related.map((e) => (
            <div key={e.id} className={`ev kind-${e.kind}`} title={e.data ? JSON.stringify(e.data) : ''}>
              <span className="muted">{fmtTime(e.time)}</span><span className="k">{e.kind}</span><span className="n">{e.node ?? ''}</span>
              <span className="m"><Linkify text={e.message} all={e.kind === 'tx'} />{typeof e.data?.reason === 'string' && e.data.reason ? <b className="bad"> — {e.data.reason}</b> : null}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function KeysPanel({ keys, err, reload }: { keys: KeyEntry[]; err: string; reload: () => Promise<void> }) {
  const { status, run } = useActions();
  const [name, setName] = useState('');
  const [note, setNote] = useState('');
  const create = () => run('new', () => api.newKey(name.trim(), note.trim() || undefined), (k) => `created ${k.name} → ${k.address}`).then((k) => { if (k) { setName(''); setNote(''); void reload(); } });
  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>keyring <span className="muted lower">{keys.length} keys · GET /api/keys (private keys redacted)</span></h2>
        <div className="row">
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="new key name" style={{ width: 150 }} />
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="note" style={{ width: 200 }} />
          <button onClick={create} disabled={!name.trim() || status.new?.busy}>new key</button>
          <Status st={status.new} />
          <button onClick={() => void reload()}>refresh</button>
        </div>
      </div>
      {err && <div className="error small">{err}</div>}
      {keys.length === 0 && !err && <Empty>no keys</Empty>}
      {keys.length > 0 && (
        <table className="compact">
          <thead><tr><th>name</th><th>address</th><th>locking script</th><th>note</th></tr></thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.name} className="key-row">
                <td><b>{k.name}</b></td>
                <td><code className="hash" title="click to copy" onClick={() => copy(k.address)}>{k.address}</code></td>
                <td>{k.lockingScript ? <Hash h={k.lockingScript} n={14} /> : <span className="muted">—</span>}</td>
                <td className="muted">{k.note ?? ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
