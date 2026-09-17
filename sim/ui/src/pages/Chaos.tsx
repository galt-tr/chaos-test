import { useState } from 'react';
import { api, type NodeState } from '../api';
import { useLiveCtx } from '../live';
import { useActions } from '../hooks';
import { EventFeed } from './Fleet';
import { Empty, Hash, Pill, Status, Tip } from '../ui';

export function ChaosPage() {
  const { snapshot, events } = useLiveCtx();
  const nodes = snapshot?.nodes ?? [];
  const { status, run } = useActions();
  if (!snapshot) return <div className="panel"><h2>Chaos</h2><Empty>waiting for snapshot…</Empty></div>;
  return (
    <>
      <div className="grid nodes">
        {nodes.map((n) => <NodeChaos key={n.name} n={n} status={status} run={run} />)}
      </div>
      <EventFeed events={events} nodes={nodes} title="chaos & mining events" initialKind="chaos" compact />
    </>
  );
}

type Runner = ReturnType<typeof useActions>;

function NodeChaos({ n, status, run }: { n: NodeState; status: Runner['status']; run: Runner['run'] }) {
  const parts = n.partitions ?? [];
  const alertCut = parts.includes('alert');
  const p2pCut = parts.includes('p2p');
  const [confirmStop, setConfirmStop] = useState(false);
  const [blocks, setBlocks] = useState('1');
  const [address, setAddress] = useState('');
  const k = (s: string) => `${s}:${n.name}`;

  const partition = (plane: 'alert' | 'p2p', on: boolean) =>
    run(k(`part-${plane}`), () => api.partition(n.name, plane, on), () => (on ? `${plane} plane disconnected` : `${plane} plane reconnected`));
  const container = (action: 'pause' | 'unpause' | 'stop' | 'start') =>
    run(k('container'), () => api.chaos(n.name, action), () => `${action} ok`);
  const mine = () => {
    const b = Math.max(1, Number(blocks) || 1);
    return run(k('mine'), () => api.mine(n.name, b, address.trim() || undefined),
      (r) => `mined ${r.hashes?.length ?? b} block(s)${r.hashes?.[r.hashes.length - 1] ? ` → ${r.hashes[r.hashes.length - 1].slice(0, 12)}…` : ''}`);
  };

  return (
    <div className={`panel node ${n.reachable ? '' : 'down'}`}>
      <div className="row between">
        <h2 style={{ margin: 0 }}>{n.name}</h2>
        <span className="row">
          {parts.map((p) => <Pill key={p} ok={false}>{p} plane cut</Pill>)}
          <Pill ok={n.reachable}>{n.reachable ? 'reachable' : 'unreachable'}</Pill>
          <span className={`small ${n.fsm === 'RUNNING' ? 'ok' : 'warn'}`}>{n.fsm || 'fsm ?'}</span>
        </span>
      </div>
      <div className="small muted" style={{ marginTop: 4 }}>height {n.height} · <Tip hash={n.tip} n={8} /> · mempool {n.mempoolCount} · alert #{n.alertSeq < 0 ? 'n/a' : n.alertSeq} · <code>{n.container}</code></div>

      <fieldset>
        <legend>network partitions (podman network disconnect/connect)</legend>
        <div className="row">
          <span style={{ minWidth: 90 }}>alert plane</span>
          {alertCut
            ? <button onClick={() => partition('alert', false)} disabled={status[k('part-alert')]?.busy}>heal alert plane</button>
            : <button className="danger" onClick={() => partition('alert', true)} disabled={status[k('part-alert')]?.busy}>cut alert plane</button>}
          <span className={`small ${alertCut ? 'bad' : 'ok'}`}>{alertCut ? 'disconnected from alertnet' : 'connected'}</span>
          <Status st={status[k('part-alert')]} />
        </div>
        <div className="row" style={{ marginTop: 6 }}>
          <span style={{ minWidth: 90 }}>p2p plane</span>
          {p2pCut
            ? <button onClick={() => partition('p2p', false)} disabled={status[k('part-p2p')]?.busy}>heal p2p plane</button>
            : <button className="danger" onClick={() => partition('p2p', true)} disabled={status[k('part-p2p')]?.busy}>cut p2p plane</button>}
          <span className={`small ${p2pCut ? 'bad' : 'ok'}`}>{p2pCut ? 'disconnected from chaosnet' : 'connected'}</span>
          <Status st={status[k('part-p2p')]} />
        </div>
      </fieldset>

      <fieldset>
        <legend>container</legend>
        <div className="row">
          <button onClick={() => container('pause')} disabled={status[k('container')]?.busy} title="SIGSTOP the container (freezes the process, keeps network)">pause</button>
          <button onClick={() => container('unpause')} disabled={status[k('container')]?.busy}>unpause</button>
          {confirmStop ? (
            <span className="danger-confirm">
              <span className="small">stop {n.name}?</span>
              <button className="danger" onClick={() => { setConfirmStop(false); void container('stop'); }}>yes, stop</button>
              <button onClick={() => setConfirmStop(false)}>cancel</button>
            </span>
          ) : (
            <button className="danger" onClick={() => setConfirmStop(true)} disabled={status[k('container')]?.busy} title="stop the container (asks for confirmation)">stop…</button>
          )}
          <button onClick={() => container('start')} disabled={status[k('container')]?.busy}>start</button>
        </div>
        <Status st={status[k('container')]} />
      </fieldset>

      <fieldset>
        <legend>mine (RPC generate / generatetoaddress)</legend>
        <div className="row">
          <label>blocks<input type="number" min={1} max={1000} value={blocks} onChange={(e) => setBlocks(e.target.value)} style={{ width: 70 }} /></label>
          <label>to address <span className="muted">(optional)</span><input value={address} onChange={(e) => setAddress(e.target.value)} className="mono" style={{ width: 240 }} placeholder="default: node's miner key" /></label>
          <button className="primary" onClick={mine} disabled={status[k('mine')]?.busy || !n.reachable}>mine</button>
        </div>
        <Status st={status[k('mine')]} />
        {status[k('mine')]?.ok && n.tip && <div className="small muted">tip now <Hash h={n.tip} n={8} /> @ {n.height}</div>}
      </fieldset>
    </div>
  );
}
