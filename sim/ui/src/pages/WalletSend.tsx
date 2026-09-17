import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, type ApiError, type SendStatus, type TxShape, type WalletState } from '../api';
import { useLiveCtx } from '../live';
import { ago, useActions, useSticky, useVisible } from '../hooks';
import { Pill, Status } from '../ui';
import { DualStatus } from '../txstatus';

const SHAPES: { id: TxShape; label: string }[] = [
  { id: 'opreturn', label: 'OP_RETURN data' },
  { id: 'payment', label: 'P2PKH payment' },
  { id: 'fanout', label: 'fan-out' },
  { id: 'custom', label: 'custom script' },
];

/**
 * The sustained send.
 *
 * The lifecycle belongs to the orchestrator, not to this tab: there is no local `running` flag
 * anywhere here. <main key={route}> destroys this component on every navigation, so the
 * mount-time fetch below is the resync, and arriving mid-run is the ordinary case.
 */
export function SendPanel({ wallet, onChanged }: {
  wallet: WalletState | null; onChanged: () => void;
}) {
  const { events } = useLiveCtx();
  const visible = useVisible();
  const { status, run } = useActions();
  const [s, setS] = useState<SendStatus | null>(null);
  const [err, setErr] = useState('');
  const mountedAt = useRef(Date.now());

  const load = useCallback(async () => {
    try { setS(await api.sendStatus()); setErr(''); }
    catch (e) { setErr(e instanceof Error ? e.message : String(e)); }
  }, []);

  useEffect(() => { void load(); }, [load]);
  const live = !!s && (s.running || s.draining);
  useEffect(() => {
    if (!visible) return;
    const t = setInterval(() => { void load(); }, live ? 1500 : 6000);
    return () => clearInterval(t);
  }, [load, visible, live]);

  // A wallet event means the server changed something; refetch rather than wait for the tick.
  const lastEv = useMemo(() => {
    for (let i = events.length - 1; i >= 0; i--) if (events[i].kind === 'wallet') return events[i].id;
    return 0;
  }, [events]);
  useEffect(() => { if (lastEv) void load(); }, [lastEv, load]);

  const [tps, setTps] = useSticky('wallet.send.tps', '1');
  const [mins, setMins] = useSticky('wallet.send.mins', '0');
  const [shape, setShape] = useSticky('wallet.send.shape', 'opreturn');


  const start = () => run('start', () => api.sendStart({
    tps: Number(tps) || 1, shape: shape as TxShape,
    durationSeconds: Number(mins) > 0 ? Number(mins) * 60 : undefined,
  }), () => 'send started').then((r) => { if (r) { setS(r); onChanged(); } });

  const stop = () => run('stop', () => api.sendStop(), () => 'stopping — draining in flight')
    .then((r) => { if (r) setS(r); void load(); });

  // A 409 means someone else started a run between render and click. Resync, don't argue.
  useEffect(() => { if (status.start?.err) void load(); }, [status.start?.err, load]);

  if (live && s) {
    const foreign = !!s.startedAt && new Date(s.startedAt).getTime() < mountedAt.current - 2000;
    return (
      <div className="panel">
        <div className="row between">
          <h2 style={{ margin: 0 }}>
            sustained send <span className="muted lower">
              {s.tps}/s · {s.shape} · → arcade · running in the orchestrator
            </span>
          </h2>
          <div className="row">
            <Pill ok={s.draining ? null : true}>{s.draining ? 'draining' : 'running'}</Pill>
            {s.startedAt && <span className="small muted">{ago(s.startedAt)} elapsed{s.startedBy ? ` · ${s.startedBy}` : ''}</span>}
            <button className="danger" onClick={stop} disabled={s.draining || status.stop?.busy}
              title={s.draining ? 'already stopping; in-flight sends are being awaited' : 'stop scheduling; in-flight sends still finish'}>
              {s.draining ? `stopping… ${s.inFlight} in flight` : 'stop'}
            </button>
            <Status st={status.stop} />
          </div>
        </div>

        {foreign && (
          <div className="finding">
            This send was already running when the page loaded{s.startedBy ? ` — started by ${s.startedBy}` : ''}.
            The settings shown are the <b>server's</b>. Stopping it stops it for everyone.
          </div>
        )}

        <div className="counters">
          <C n={s.attempted} k="attempted" t="handed to the wallet" />
          <C n={s.succeeded} k="wallet took" t="the wallet stored and broadcast it. NOT a network verdict — see the arcade column in the table below." />
          <C n={s.failed} k="failed" t="wallet or transport errors" bad={s.failed > 0} />
          <C n={s.backpressure} k="waiting" t="sends skipped because no coin was spendable — idle, not failed" warn={s.backpressure > 0} />
          <C n={s.canceled} k="canceled" t="in flight when Stop was pressed; not a failure" />
          <C n={s.measuredTps.toFixed(2)} k="tx/s now" t={`target ${s.tps}/s, measured over real elapsed time`}
            warn={s.tps > 0 && s.measuredTps < s.tps * 0.7} />
        </div>

        <CoinGauge s={s} wallet={wallet} />

        {s.lastError && <div className="error small" style={{ marginTop: 6 }}>last error: {s.lastError}</div>}
      </div>
    );
  }

  const noCoins = (wallet?.coins ?? 0) === 0;
  const blocked = !wallet?.connected ? 'the wallet is not connected'
    : noCoins ? 'no spendable coins — top up first'
    : s === null ? 'the send status has not been read yet' : '';
  return (
    <div className="panel">
      <h2>sustained send <span className="muted lower">runs in the orchestrator, not in this tab</span></h2>
      {err && <div className="error small">send status unavailable: {err}</div>}
      {s?.stopReason && s.attempted > 0 && (
        <div className="small muted">
          last run {s.stopReason}: {s.attempted} attempted, {s.succeeded} taken by the wallet,
          {' '}{s.failed} failed, {s.backpressure} waiting
        </div>
      )}
      <div className="row">
        <label>rate
          <span className="row" style={{ gap: 4 }}>
            <input type="number" min={0.05} max={20} step={0.05} value={tps}
              onChange={(e) => setTps(e.target.value)} style={{ width: 80 }} />
            <span className="small muted" style={{ alignSelf: 'center' }}>tx/s</span>
            {['0.2', '0.5', '1', '2'].map((p) => (
              <button key={p} className="link" onClick={() => setTps(p)}>{p}</button>
            ))}
          </span>
        </label>
        <label>duration
          <span className="row" style={{ gap: 4 }}>
            <input type="number" min={0} value={mins} onChange={(e) => setMins(e.target.value)} style={{ width: 70 }} />
            <span className="small muted" style={{ alignSelf: 'center' }}>min · 0 = until stopped</span>
          </span>
        </label>
        <label>shape
          <select value={shape} onChange={(e) => setShape(e.target.value)}>
            {SHAPES.map((x) => <option key={x.id} value={x.id}>{x.label}</option>)}
          </select>
        </label>
        <label>target
          <input value="arcade" readOnly className="mono" style={{ width: 110 }}
            title="the sustained send is arcade-only: aiming at a node parks the change, which would strand the balance within minutes. Use a one-shot transaction for the legacy path." />
        </label>
      </div>

      <div className="small muted">
        Blocks are produced by the orchestrator's own auto-mine cadence, shown as the countdown in the header
        and configured on the Fleet page — it runs whether or not a send is active, and mining by hand never
        disturbs it. Nothing here confirms until a block is mined.
      </div>

      {Number(tps) > 5 && (
        <div className="finding">
          {tps} tx/s is a load test, not a flow. This harness only needs enough traffic that blocks are not empty.
        </div>
      )}
      <div className="row">
        <button className="primary" onClick={start} disabled={!!blocked || status.start?.busy}
          title={blocked || 'start sending'}>start send</button>
        <Status st={status.start} />
        {blocked && <span className="small warn">{blocked}</span>}
      </div>
      <div className="small muted">
        Closing this tab, switching pages or reloading does not stop a run — this panel resyncs from the
        orchestrator every time it loads.
      </div>
    </div>
  );
}

function C({ n, k, t, bad, warn }: { n: number | string; k: string; t: string; bad?: boolean; warn?: boolean }) {
  return (
    <div className="c" title={t}>
      <b className={bad ? 'bad' : warn ? 'warn' : ''}>{n}</b>
      <span>{k}</span>
    </div>
  );
}

/** Spendable coins is the gauge that explains almost every stall: a send is limited by
 *  outputs, not by balance. Running out is the sender waiting, never the sender failing. */
function CoinGauge({ s, wallet }: { s: SendStatus; wallet: WalletState | null }) {
  const total = Math.max(s.coinsAtStart, s.coinsRemaining, wallet?.coins ?? 0, 1);
  const pct = Math.min(100, Math.round((s.coinsRemaining / total) * 100));
  const low = s.coinsRemaining <= 3;
  return (
    <div style={{ marginTop: 8 }}>
      <div className="small muted">spendable coins remaining</div>
      <div className={`bar ${low ? 'warnfill' : 'val'}`}><i style={{ width: `${pct}%` }} /><b>{s.coinsRemaining} / {total}</b></div>
      {s.waitingForFunds && (
        <div className="finding">
          <b>waiting for funds{s.waitingSince ? ` — ${ago(s.waitingSince)}` : ''}.</b> Every coin is in flight, so the
          send is <b>idle, not failed</b>: it resumes as soon as change confirms or you top up. With a long auto-mine
          interval change can take that long to become spendable — fan out or top up to raise the ceiling.
        </div>
      )}
    </div>
  );
}

/** Re-exported so the transaction table and the send panel show status identically. */
export { DualStatus };
export type { ApiError };
