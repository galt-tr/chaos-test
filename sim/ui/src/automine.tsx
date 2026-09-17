/**
 * The auto-mine countdown, shown in the header on every page.
 *
 * The cadence is a metronome: a constant interval that nothing resets. Mining by hand, a
 * scenario's own mine step and a wallet send all leave it alone, so the number here is always
 * the real time until the next automatic block.
 *
 * The countdown is computed locally from an absolute `nextMineAt`, corrected by the server's
 * `now`, so it stays smooth between the (infrequent) polls and stays right even when the
 * browser's clock disagrees with the container's.
 */
import { useState } from 'react';
import { api, type AutoMine, type NodeState } from './api';
import { useLiveCtx } from './live';
import { ago, useActions } from './hooks';
import { NodeSelect, Pill, Status } from './ui';

const INTERVALS = [
  { v: 60, label: '1 min' }, { v: 300, label: '5 min' }, { v: 600, label: '10 min' },
  { v: 1800, label: '30 min' }, { v: 3600, label: '1 hour' },
];

/** Seconds until the next automatic block, or null when unknown.
 *
 *  Uses `nextMineAtLocal`, which useLive computed against this browser's clock when the
 *  response arrived. Subtracting a server timestamp from another server timestamp would be
 *  constant between polls — the countdown has to be anchored to local time to advance. */
export function secondsUntil(a: AutoMine | null): number | null {
  const at = a?.nextMineAtLocal;
  if (!at) return null;
  return Math.max(0, Math.round((at - Date.now()) / 1000));
}

export function mmss(sec: number): string {
  const m = Math.floor(sec / 60);
  return `${m}:${String(sec % 60).padStart(2, '0')}`;
}

/** Header chip. App re-renders every second, so this animates without a timer of its own. */
export function AutoMineChip() {
  const { autoMine } = useLiveCtx();
  if (!autoMine) return null;
  const left = secondsUntil(autoMine);
  const every = Math.round(autoMine.intervalSeconds / 60);

  if (!autoMine.enabled) {
    return (
      <a href="#/fleet" className="automine off" title="auto-mine is off — nothing will confirm until you mine by hand">
        auto-mine off
      </a>
    );
  }
  return (
    <a href="#/fleet" className={`automine${left !== null && left <= 10 ? ' due' : ''}`}
      title={`a block is mined on ${autoMine.node} every ${autoMine.intervalSeconds}s on a fixed schedule.`
        + ` Mining by hand does not change this countdown.`
        + (autoMine.lastMinedAt ? `\nlast auto-mined ${ago(autoMine.lastMinedAt)} ago` : '')
        + (autoMine.lastError ? `\nlast attempt failed: ${autoMine.lastError}` : '')}>
      <span className="muted">next block</span>
      <b className="num">{left === null ? '—' : mmss(left)}</b>
      <span className="muted">every {every}m</span>
      {autoMine.lastError && <span className="bad" title={autoMine.lastError}>✗</span>}
    </a>
  );
}

/** The configuration panel. Lives on the Fleet page because mining is a fleet operation, not a
 *  wallet one — the wallet send merely benefits from blocks being produced. */
export function AutoMinePanel({ nodes }: { nodes: NodeState[] }) {
  const { autoMine } = useLiveCtx();
  const { status, run } = useActions();
  const [local, setLocal] = useState<AutoMine | null>(null);
  const a = local ?? autoMine;
  const miners = nodes.filter((n) => n.kind !== 'svnode');

  const apply = (body: { enabled?: boolean; intervalSeconds?: number; node?: string }) =>
    run('automine', () => api.setAutoMine(body),
      (r) => (r.enabled ? `every ${r.intervalSeconds}s on ${r.node}` : 'disabled'))
      .then((r) => { if (r) setLocal(r); });

  if (!a) return null;
  const left = secondsUntil(a);
  return (
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>
          auto-mine <span className="muted lower">a block on a fixed cadence, for the whole harness</span>
        </h2>
        <span className="row">
          <Pill ok={a.enabled ? true : null}>{a.enabled ? 'on' : 'off'}</Pill>
          {a.enabled && <span className="small">next in <b className="num">{left === null ? '—' : mmss(left)}</b></span>}
        </span>
      </div>
      <div className="row">
        <button className={a.enabled ? '' : 'primary'} disabled={status.automine?.busy}
          onClick={() => apply({ enabled: !a.enabled })}>{a.enabled ? 'turn off' : 'turn on'}</button>
        <label>every
          <select value={String(a.intervalSeconds)} disabled={status.automine?.busy}
            onChange={(e) => apply({ intervalSeconds: Number(e.target.value) })}>
            {INTERVALS.map((i) => <option key={i.v} value={i.v}>{i.label}</option>)}
            {!INTERVALS.some((i) => i.v === a.intervalSeconds) &&
              <option value={a.intervalSeconds}>{a.intervalSeconds}s</option>}
          </select>
        </label>
        <label>on<NodeSelect nodes={miners} value={a.node} onChange={(v) => apply({ node: v })} /></label>
        <Status st={status.automine} />
      </div>
      <div className="small muted">
        The schedule is constant and independent: mining by hand, a scenario's mine step or a wallet send
        all leave this countdown untouched. Changing the interval restarts it from now.
        {a.blocksMined > 0 && <> · {a.blocksMined} block(s) auto-mined over {a.runs} run(s)</>}
        {a.lastMinedAt && <> · last {ago(a.lastMinedAt)} ago</>}
      </div>
      {a.lastError && <div className="error small">last attempt failed: {a.lastError}</div>}
      {a.enabled && miners.length === 0 && (
        <div className="finding">No teranode to mine on. SV nodes follow the teranodes and cannot mine.</div>
      )}
    </div>
  );
}
