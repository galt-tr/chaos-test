import { Fragment, useCallback, useEffect, useMemo, useState } from 'react';
import {
  api, isSV, type ApiError, type NodeState, type TopUpResult, type TxRow, type TxShape,
  type WalletDeposit, type WalletState, type WalletTxResult,
} from '../api';
import { useLiveCtx } from '../live';
import { ago, copy, fmtTime, useActions, useSticky, useVisible, withTimeout } from '../hooks';
import { BroadcastSelect, Empty, Hash, Json, KV, Linkify, NodeSelect, Pill, Status, TxLink, TxList } from '../ui';
import {
  ARCADE_GOOD, ARCADE_SETTLED, ArcadeStatus, DualStatus, HonestyNote, WalletBucketPill,
  WalletProblem, tally,
} from '../txstatus';
import { SendPanel } from './WalletSend';
import { EventFeed } from './Fleet';

export function WalletPage() {
  const { snapshot, events } = useLiveCtx();
  const visible = useVisible();
  const nodes = useMemo(() => snapshot?.nodes ?? [], [snapshot]);
  const [st, setSt] = useState<WalletState | null>(null);
  const [err, setErr] = useState<ApiError | null>(null);

  const load = useCallback(async () => {
    try { setSt(await api.walletState()); setErr(null); }
    catch (e) { setErr(e as ApiError); }
  }, []);

  useEffect(() => { void load(); }, [load]);
  const sending = !!st?.send?.running;
  useEffect(() => {
    if (!visible) return;
    const ms = err ? 15000 : sending ? 3000 : 6000;
    const t = setInterval(() => { void load(); }, ms);
    return () => clearInterval(t);
  }, [load, visible, sending, err]);

  const lastEv = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) {
      if (events[i].kind === 'wallet' || events[i].kind === 'wallet_divergence') return events[i].id;
    }
    return 0;
  }, [events]);
  useEffect(() => { if (lastEv) void load(); }, [lastEv, load]);

  const walletEvents = useMemo(
    () => events.filter((e) => e.kind === 'wallet' || e.kind === 'wallet_divergence'), [events]);
  const infra = snapshot?.services?.find((s) => s.name === 'wallet-infra');
  const down = !!err || (!!st && !st.connected);

  if (!snapshot) return <div className="panel"><h2>Wallet</h2><Empty>waiting for snapshot…</Empty></div>;

  return (
    <>
      <div className="panel">
        <div className="row between">
          <h2 style={{ margin: 0 }}>
            wallet <span className="muted lower">BRC-100 via walletd → wallet-infra → arcade</span>
          </h2>
          <span className="row">
            {infra && <Pill ok={infra.healthy} title={infra.hostURL}>wallet-infra {infra.healthy ? 'healthy' : 'unhealthy'}</Pill>}
            <Pill ok={st ? st.connected : null}>{st?.connected ? 'connected' : st ? 'not connected' : 'checking…'}</Pill>
          </span>
        </div>

        {down && <WalletProblem reason={err?.reason ?? st?.reason} error={err?.message ?? st?.error} />}

        {st?.connected && (
          <>
            <div className="row" style={{ gap: 28, alignItems: 'baseline', marginTop: 4 }}>
              <Stat n={st.balance.toLocaleString()} k="satoshis spendable" />
              <Stat n={st.coins} k="spendable coins" t="a send is limited by coins, not by balance" />
              <Stat n={st.health.total} k="wallet actions" />
            </div>
            <div className="row" style={{ marginTop: 8 }}>
              <span className="small muted">wallet buckets</span>
              {Object.entries(st.health.buckets ?? {}).map(([b, n]) => (
                <span key={b} className={`pill wb-${b}`}>{b} <b className="num">{n}</b></span>
              ))}
              {Object.keys(st.health.buckets ?? {}).length === 0 && <span className="muted small">none yet</span>}
            </div>
            <div style={{ marginTop: 4 }}><HonestyNote /></div>
            <dl className="kv" style={{ marginTop: 8 }}>
              <KV k="accept rate">
                {st.health.decided > 0
                  ? <><b className="num">{Math.round(st.health.acceptRate * 100)}%</b>{' '}
                      <span className="muted small">of {st.health.decided} decided outcome(s)</span></>
                  : <span className="muted">no decided outcomes yet — a percentage here would be invented</span>}
              </KV>
              <KV k="identity">{st.identityKey ? <Hash h={st.identityKey} n={16} /> : <span className="muted">—</span>}</KV>
              <KV k="network"><code>{st.network}</code></KV>
              <KV k="checked">{st.checkedAt ? `${ago(st.checkedAt)} ago` : '—'}</KV>
            </dl>
            <CoinList coins={st.coins_list ?? []} />
          </>
        )}
      </div>

      <div className="two">
        <TopUpPanel nodes={nodes} st={st} onDone={load} />
        <DepositPanel />
      </div>

      <SendPanel wallet={st} onChanged={load} />
      <BuilderPanel nodes={nodes} st={st} onDone={load} />
      <TxTable st={st} sending={sending} />
      <EventFeed events={walletEvents} nodes={nodes} title="wallet events" compact />
    </>
  );
}

function Stat({ n, k, t }: { n: number | string; k: string; t?: string }) {
  return <div title={t}><div className="big num">{n}</div><div className="small muted">{k}</div></div>;
}

function CoinList({ coins }: { coins: { outpoint: string; satoshis: number; spendable: boolean }[] }) {
  const [open, setOpen] = useState(false);
  if (coins.length === 0) return null;
  return (
    <>
      <button className="link" onClick={() => setOpen(!open)} style={{ marginTop: 6 }}>
        {open ? '▾' : '▸'} {coins.length} coin(s)
      </button>
      {open && (
        <table className="compact">
          <thead><tr><th>outpoint</th><th>satoshis</th><th>spendable</th></tr></thead>
          <tbody>
            {[...coins].sort((a, b) => b.satoshis - a.satoshis).map((c) => (
              <tr key={c.outpoint}>
                <td><Hash h={c.outpoint.split('.')[0] ?? c.outpoint} n={10} /></td>
                <td className="num">{c.satoshis.toLocaleString()}</td>
                <td>{c.spendable ? <span className="ok">yes</span> : <span className="muted">no</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

/** Presets deliberately make MANY coins rather than one fat one: a single output pins the coin
 *  count at 1 and serialises the whole send on change confirmation. */
const PRESETS = [
  { label: '10 × 200k', count: 10, sats: 2_000_000, primary: true, hint: 'ten spendable coins — the shape a sustained send needs' },
  { label: '30 × 200k', count: 30, sats: 6_000_000, hint: 'enough coins for a long run' },
  { label: '1 × 2M', count: 1, sats: 2_000_000, hint: 'one fat coin: large balance but only ONE spendable output — a send will stall waiting on change' },
];

function TopUpPanel({ nodes, st, onDone }: { nodes: NodeState[]; st: WalletState | null; onDone: () => void }) {
  const { events } = useLiveCtx();
  const { status, run } = useActions();
  const miners = useMemo(() => nodes.filter((n) => !isSV(n) && n.reachable), [nodes]);
  const [node, setNode] = useSticky('wallet.topup.node', '');
  const [startedAt, setStartedAt] = useState(0);
  const [stranded, setStranded] = useState<TopUpResult | null>(null);
  const from = node || miners[0]?.name || '';
  const busy = !!status.topup?.busy;

  const topUp = (count: number, sats: number) => {
    setStartedAt(Date.now());
    setStranded(null);
    return run('topup',
      () => withTimeout(api.walletTopUp({ node: from, satoshis: sats, count, mine: true, waitSeconds: 150 }), 240_000),
      (r) => `credited ${r.satoshis.toLocaleString()} sat — ${r.coins} coin(s), balance ${r.balance.toLocaleString()}`)
      .then((r) => { if (r) onDone(); });
  };

  // The POST is one long call, so progress arrives on the bus instead.
  const stages = events.filter((e) => e.kind === 'wallet' && new Date(e.time).getTime() >= startedAt - 500);
  const proofTimeout = (status.topup?.err ?? '').includes('merkle proof');

  return (
    <div className="panel">
      <h2>top up <span className="muted lower">coinbase → deposit address → internalizeAction</span></h2>
      {miners.length === 0 && (
        <div className="finding">No reachable teranode to mine from. SV nodes follow the teranodes and cannot mine.</div>
      )}
      <div className="row">
        <label>from<NodeSelect nodes={miners} value={from} onChange={setNode} /></label>
        {PRESETS.map((p) => (
          <button key={p.label} className={p.primary ? 'primary' : ''} disabled={busy || !from || !st?.connected}
            title={p.hint} onClick={() => topUp(p.count, p.sats)}>+ {p.label}</button>
        ))}
        <Status st={status.topup} />
      </div>

      {busy && (
        <div className="finding">
          <b>topping up… {Math.round((Date.now() - startedAt) / 1000)}s</b>
          <div className="small">
            It mines to maturity if needed, pays the wallet's deposit address, waits for the transaction to reach a
            mempool, mines until it confirms, then credits it. A funding payment cannot be credited before it is
            mined and proven, so a block is unavoidable. Usually 15–40s.
          </div>
          {stages.slice(-6).map((e) => (
            <div key={e.id} className="small"><span className="muted">{fmtTime(e.time)}</span> <Linkify text={e.message} /></div>
          ))}
        </div>
      )}

      {proofTimeout && (
        <div className="finding">
          <b>The funding transaction has not been mined yet.</b> Nothing is lost — mine a block, then finish the
          credit without funding again.
          <div className="row" style={{ marginTop: 6 }}>
            <button onClick={() => run('retry',
              () => withTimeout(api.walletInternalize({ txid: strandedTxid(status.topup?.err) , waitSeconds: 60 }), 90_000),
              (r) => `credited — balance ${r.balance.toLocaleString()}`).then((r) => { if (r) { setStranded(r); onDone(); } })}
              disabled={status.retry?.busy}>finish the credit</button>
            <Status st={status.retry} />
          </div>
        </div>
      )}
      {stranded && <div className="accepted small">recovered: balance {stranded.balance.toLocaleString()} over {stranded.coins} coin(s)</div>}

      {st?.connected && st.coins === 0 && !busy && (
        <div className="finding">
          <b>No spendable coins.</b> Nothing can be sent until you top up — <b>10 × 200k</b> is the right first click.
        </div>
      )}
      {status.topup?.ok && <StepList steps={lastSteps} />}
    </div>
  );
}

let lastSteps: TopUpResult['steps'] = null;

function StepList({ steps }: { steps: TopUpResult['steps'] }) {
  if (!steps || steps.length === 0) return null;
  return (
    <table className="compact">
      <tbody>
        {steps.map((s) => (
          <tr key={s.name}>
            <td>{s.ok ? <span className="ok">✓</span> : <span className="bad">✗</span>}</td>
            <td>{s.name}</td><td className="muted small">{s.elapsed}</td>
            <td className="small">{s.detail}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function strandedTxid(err?: string): string {
  const m = /for ([0-9a-f]{64})/.exec(err ?? '');
  return m ? m[1] : '';
}

function DepositPanel() {
  const [d, setD] = useState<WalletDeposit | null>(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    api.walletDeposit().then(setD).catch((e: Error) => setErr(e.message));
  }, []);
  return (
    <div className="panel">
      <h2>deposit address <span className="muted lower">BRC-29, derived from the wallet key</span></h2>
      {err && <div className="error small">{err}</div>}
      {d && (
        <>
          <div className="row">
            <code className="hash" title={`${d.address} (click to copy)`} onClick={() => copy(d.address)}>{d.address}</code>
            <span className="muted small">{d.network}</span>
          </div>
          <div className="small muted" style={{ marginTop: 6 }}>
            Paying this address does not credit the wallet on its own — the orchestrator has to call
            <code> internalizeAction</code> with the derivation data below, and that needs the payment mined and
            proven first. The top-up buttons do all of it.
          </div>
          <Json v={{ derivationPrefixB64: d.derivationPrefixB64, derivationSuffixB64: d.derivationSuffixB64, lockingScriptHex: d.lockingScriptHex }} label="derivation data" />
        </>
      )}
    </div>
  );
}

const SHAPES: { id: TxShape; label: string; hint: string }[] = [
  { id: 'payment', label: 'P2PKH payment', hint: 'one ordinary pay-to-address output' },
  { id: 'opreturn', label: 'OP_RETURN data', hint: 'a zero-value data output; the fee comes from the input' },
  { id: 'fanout', label: 'fan-out', hint: 'N outputs back to this wallet — how one coin becomes many spendable coins' },
  { id: 'custom', label: 'custom script', hint: 'raw locking-script hex; may be unspendable and may be non-standard' },
];

function BuilderPanel({ nodes, st, onDone }: { nodes: NodeState[]; st: WalletState | null; onDone: () => void }) {
  const { status, run } = useActions();
  const [shape, setShape] = useSticky('wallet.tx.shape', 'payment');
  const [to, setTo] = useSticky('wallet.tx.to', '');
  const [sats, setSats] = useSticky('wallet.tx.sats', '5000');
  const [data, setData] = useSticky('wallet.tx.data', 'chaos-test');
  const [outs, setOuts] = useSticky('wallet.tx.outs', '8');
  const [script, setScript] = useSticky('wallet.tx.script', '');
  const [target, setTarget] = useSticky('wallet.tx.target', 'arcade');
  const [res, setRes] = useState<WalletTxResult | null>(null);
  const [live, setLive] = useState<TxRow | null>(null);

  const def = SHAPES.find((x) => x.id === shape) ?? SHAPES[0];
  const submit = () => run('tx', () => api.walletTx({
    shape: shape as TxShape, target, label: 'chaos-ui',
    ...(shape === 'payment' ? { to: to.trim() || undefined, satoshis: Number(sats) || 0 } : {}),
    ...(shape === 'opreturn' ? { data } : {}),
    ...(shape === 'fanout' ? { outputs: Number(outs) || 2, satoshis: Number(sats) || 0 } : {}),
    ...(shape === 'custom' ? { script: script.trim(), satoshis: Number(sats) || 0 } : {}),
  }), (r) => (r.txid ? `wallet stored ${r.txid.slice(0, 12)}…` : 'no txid'))
    .then((r) => { if (r) { setRes(r); setLive(null); onDone(); } });

  const noCoins = (st?.coins ?? 0) === 0;
  return (
    <div className="panel">
      <h2>build a transaction <span className="muted lower">spends the wallet's own coins</span></h2>
      <div className="row">
        {SHAPES.map((x) => (
          <button key={x.id} className={shape === x.id ? 'primary' : ''} title={x.hint}
            onClick={() => { setShape(x.id); setRes(null); }}>{x.label}</button>
        ))}
      </div>
      <div className="small muted" style={{ marginTop: 4 }}>{def.hint}</div>

      <fieldset>
        <legend>1 · outputs</legend>
        {shape === 'payment' && (
          <div className="row">
            <label>pay to<input value={to} onChange={(e) => setTo(e.target.value)} className="mono" style={{ width: 340 }}
              placeholder="address — empty pays this wallet's own deposit address" /></label>
            <label>satoshis<input type="number" min={1} value={sats} onChange={(e) => setSats(e.target.value)} style={{ width: 110 }} /></label>
          </div>
        )}
        {shape === 'opreturn' && (
          <>
            <label>payload<textarea className="hex" value={data} onChange={(e) => setData(e.target.value)} /></label>
            <div className="small muted">
              {new TextEncoder().encode(data).length} bytes. The output carries 0 satoshis; the fee comes from the
              input. The backend writes the <code>OP_FALSE OP_RETURN</code> prefix.
            </div>
          </>
        )}
        {shape === 'fanout' && (
          <>
            <div className="row">
              <label>outputs<input type="number" min={2} max={1000} value={outs} onChange={(e) => setOuts(e.target.value)} style={{ width: 90 }} /></label>
              <label>sat each<input type="number" min={1} value={sats} onChange={(e) => setSats(e.target.value)} style={{ width: 110 }} /></label>
              <span className="small muted" style={{ alignSelf: 'flex-end' }}>
                = <b className="num">{((Number(outs) || 0) * (Number(sats) || 0)).toLocaleString()}</b> sat + fee
              </span>
            </div>
            <div className="small muted">
              Each output becomes a new spendable coin. This is the fix for a stalled send: <b>coins</b>, not balance,
              is what limits the rate.
            </div>
          </>
        )}
        {shape === 'custom' && (
          <>
            <label>locking script (hex)<textarea className="hex" value={script} onChange={(e) => setScript(e.target.value)} placeholder="76a914…88ac" /></label>
            <div className="row">
              <label>satoshis<input type="number" min={0} value={sats} onChange={(e) => setSats(e.target.value)} style={{ width: 110 }} /></label>
            </div>
            <div className="finding">
              An arbitrary locking script may be unspendable — these satoshis can be gone for good — and a
              non-standard script is exactly what a node may refuse. That is usually the point; just know which it is.
            </div>
          </>
        )}
      </fieldset>

      <fieldset>
        <legend>2 · broadcast</legend>
        <div className="row">
          <label>target<BroadcastSelect nodes={nodes} value={target} onChange={setTarget} /></label>
          <button className="primary" onClick={submit} disabled={status.tx?.busy || noCoins || !st?.connected}
            title={noCoins ? 'no spendable coins — top up first' : 'create and broadcast'}>create &amp; broadcast</button>
          <Status st={status.tx} />
        </div>
        {target !== 'arcade' && (
          <div className="finding">
            <b>Legacy path.</b> This goes straight at <code>{target}</code> and arcade is never asked to broadcast
            it, so the arcade column stays <i>not checked</i> unless one of arcade's datahubs happens to see it on
            the network. Its change is also <b>parked rather than spendable</b>, which is why the sustained send
            refuses this path.
          </div>
        )}
      </fieldset>

      {res && <SubmitResult res={res} live={live} onChecked={setLive} />}
    </div>
  );
}

function SubmitResult({ res, live, onChecked }: {
  res: WalletTxResult; live: TxRow | null; onChecked: (t: TxRow) => void;
}) {
  const { status, run } = useActions();
  const a = live?.arcadeStatus;
  const box = !res.txid ? 'rejection'
    : a && ARCADE_GOOD.has(a) ? 'accepted'
    : a === 'DOUBLE_SPEND_ATTEMPTED' ? 'rejection'
    : a === 'REJECTED' ? 'finding'
    : 'pending';
  const headline = !res.txid ? 'NOT CREATED'
    : a && ARCADE_GOOD.has(a) ? 'MINED'
    : a === 'DOUBLE_SPEND_ATTEMPTED' ? 'DOUBLE SPEND ATTEMPTED'
    : a === 'REJECTED' ? 'REJECTED (provisional)'
    : 'SUBMITTED — no network verdict yet';

  return (
    <div className={box}>
      <div className="row">
        <b>{headline}</b>
        {res.txid && <TxLink txid={res.txid} n={16} />}
        <span className="small">target <code>{res.target}</code></span>
        {res.noSend && <Pill ok={null} title="built with NoSend for the legacy path; its change is parked, not spendable">nosend</Pill>}
      </div>
      {res.txid && (
        <>
          <div className="small" style={{ marginTop: 4 }}>
            <DualStatus wallet={res.walletStatus ?? live?.walletStatus} arcade={a} at={live?.arcadeCheckedAt} />
          </div>
          {box === 'pending' && (
            <div className="small" style={{ marginTop: 4, opacity: .85 }}>
              The wallet has <b>stored</b> the action. That is not yet evidence the network took it — check arcade,
              or wait for the next block.
            </div>
          )}
          <div className="row" style={{ marginTop: 6 }}>
            <button onClick={() => run('check', () => api.walletTxStatus(res.txid),
              (t) => `arcade: ${t.arcadeStatus || 'unknown'}`).then((t) => { if (t) onChecked(t); })}
              disabled={status.check?.busy}>check arcade now</button>
            <Status st={status.check} />
            {res.rawHex && <button className="link" onClick={() => copy(res.rawHex!)}>copy raw hex</button>}
          </div>
          {live?.competingTxs?.length ? (
            <div className="small" style={{ marginTop: 6 }}>competing: <TxList txids={live.competingTxs} n={8} /></div>
          ) : null}
          {live?.extraInfo && <div className="small mono wrap" style={{ marginTop: 4 }}>{live.extraInfo}</div>}
        </>
      )}
      {res.body && <pre style={{ color: 'inherit', marginTop: 6 }}><Linkify text={res.body} /></pre>}
    </div>
  );
}

function TxTable({ st, sending }: { st: WalletState | null; sending: boolean }) {
  const visible = useVisible();
  const [overlay, setOverlay] = useState<Record<string, TxRow>>({});
  const [auto, setAuto] = useState(true);
  const [open, setOpen] = useState<string | null>(null);
  const actions = st?.recent ?? [];
  const rows = actions.map((a) => ({
    ...a,
    arcadeStatus: a.arcadeStatus || overlay[a.txid]?.arcadeStatus,
    arcadeCheckedAt: a.arcadeCheckedAt || overlay[a.txid]?.arcadeCheckedAt,
    blockHeight: a.blockHeight || overlay[a.txid]?.blockHeight,
  }));
  const t = tally(rows);

  useEffect(() => {
    if (!auto || !visible) return;
    const tick = async () => {
      const due = rows.filter((r) => !r.arcadeStatus || !ARCADE_SETTLED.has(r.arcadeStatus)).slice(0, 6);
      for (const r of due) {
        try {
          const s = await api.walletTxStatus(r.txid);
          setOverlay((o) => ({ ...o, [r.txid]: s }));
        } catch { /* one bad lookup must not blank the table */ }
      }
    };
    void tick();
    const iv = setInterval(() => { void tick(); }, sending ? 6000 : 12000);
    return () => clearInterval(iv);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [auto, visible, sending, actions.length]);

  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>transactions <span className="muted lower">{actions.length} recent</span></h2>
        <div className="row">
          <span className="small">
            <b className="num">{t.decided}</b> decided (<span className="ok">{t.mined} mined</span>
            {t.bad ? <>, <span className="bad">{t.bad} double-spend</span></> : null})
            {t.provisional ? <>{' · '}<span className="warn">{t.provisional} rejected·provisional</span></> : null}
            {' · '}<b className="num">{t.undecided}</b> undecided
          </span>
          <button className={auto ? 'primary' : ''} onClick={() => setAuto(!auto)}
            title="ask arcade about rows it has not decided">{auto ? 'auto-checking arcade' : 'auto-check off'}</button>
        </div>
      </div>
      <div style={{ marginBottom: 6 }}><HonestyNote /></div>

      {t.undecided > 8 && sending && (
        <div className="finding">
          {t.undecided} transactions have no network verdict yet. Between blocks that is expected backlog, not
          failure — mine a block to settle them.
        </div>
      )}

      {actions.length === 0 && <Empty>no transactions yet — build one above, or start a sustained send</Empty>}
      {actions.length > 0 && (
        <table className="compact">
          <thead><tr><th>when</th><th>txid</th><th>target</th><th>sat</th><th>wallet</th><th>arcade</th><th>block</th><th /></tr></thead>
          <tbody>
            {rows.map((r) => (
              <Fragment key={r.txid}>
                <tr onClick={() => setOpen(open === r.txid ? null : r.txid)} style={{ cursor: 'pointer' }}
                  className={r.diverged ? 'sel' : ''}>
                  <td className="muted">{r.createdAt ? fmtTime(r.createdAt) : ''}</td>
                  <td><TxLink txid={r.txid} n={10} /></td>
                  <td className="small">{r.target}{r.shape ? <span className="muted"> · {r.shape}</span> : null}</td>
                  <td className="num">{r.satoshis}</td>
                  <td><WalletBucketPill status={r.walletStatus} /></td>
                  <td><ArcadeStatus status={r.arcadeStatus} at={r.arcadeCheckedAt} /></td>
                  <td className="num">{r.blockHeight || ''}</td>
                  <td>{r.diverged && <span className="bad small" title="the wallet and the network disagree about this transaction">⚠ diverged</span>}</td>
                </tr>
                {open === r.txid && (
                  <tr><td colSpan={8}>
                    <pre className="evdata">
                      {r.extraInfo ? `${r.extraInfo}\n` : ''}
                      {r.competingTxs?.length ? `competing: ${r.competingTxs.join(', ')}\n` : ''}
                      {`wallet=${r.walletStatus || '?'} arcade=${r.arcadeStatus || 'not checked'} terminal=${r.terminal}`}
                    </pre>
                  </td></tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
