/** Per-node action controls, shared by the Fleet cards and the Chaos console.
 *
 *  These are layout-light leaves: each renders its own button(s) plus the inline <Status> and
 *  nothing else — no fieldset, no row, no legend — so every page supplies its own wrapper and
 *  no component needs a `variant` prop. They live here rather than in a page because they call
 *  `api` (so they don't belong in the presentational ui.tsx) and because exporting them from
 *  Chaos.tsx would make Fleet and Chaos import each other.
 */
import { useRef, useState } from 'react';
import { api, isSV, type NodeState } from './api';
import type { useActions } from './hooks';
import { Status, Tip } from './ui';

/** Namespaced useActions key so per-node state never collides: `mine:teranode1`. */
export const akey = (action: string, node: string) => `${action}:${node}`;

type Runner = ReturnType<typeof useActions>;
export type ActionProps = { n: NodeState; status: Runner['status']; run: Runner['run'] };

/** Frees the button when a node accepts the connection but never answers — a SIGSTOP'd container
 *  completes the TCP handshake and then goes silent. The orchestrator's handler and its RPC client
 *  both allow 5 minutes and `req()` sends no AbortSignal, so without this a mis-timed click sits on
 *  `mining…` for that long. The request is NOT cancelled, only stopped being waited on. */
function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return Promise.race([p, new Promise<T>((_, reject) => setTimeout(
    () => reject(new Error(`no response after ${ms / 1000}s — the node may be paused; the mine may still be running`)), ms))]);
}

/**
 * On-demand mining for one node: a button, inline status, and the hash of the block just produced.
 *
 * The hash comes from the POST response, never from the snapshot: the fleet poller is ~1s behind
 * and the SSE snapshot up to 2s, so `n.tip` still names the *previous* tip for a moment after a
 * successful mine. Block hashes are deliberately not TxLinks (see NOT_TX_KEYS in txids.ts), so
 * <Tip> is the right primitive — it gives copy-on-click plus the swatch the Consensus row uses,
 * which is how you see whether the other nodes adopted the block.
 */
export function MineControl({ n, status, run, blocks = 1, address, label = 'mine block' }:
  ActionProps & { blocks?: number; address?: string; label?: string }) {
  const [mined, setMined] = useState<string>();
  const inFlight = useRef(false);
  const key = akey('mine', n.name);
  const st = status[key];
  const cut = n.partitions ?? [];

  const mine = async () => {
    if (inFlight.current) return; // a second click landing before `busy` re-renders the button
    inFlight.current = true;
    try {
      // `hashes` is a Go nil slice on the wire when empty, i.e. JSON null — hence `?.` / `??`.
      const r = await run(key, () => withTimeout(api.mine(n.name, blocks, address), 45_000), (res) => {
        const count = res.hashes?.length ?? blocks;
        return `mined ${count} block${count === 1 ? '' : 's'}`;
      });
      if (r?.hashes?.length) setMined(r.hashes[r.hashes.length - 1]);
    } finally {
      inFlight.current = false;
    }
  };

  const title = st?.busy ? `mining on ${n.name}…`
    : !n.reachable ? `${n.name} is unreachable — cannot mine`
    : cut.length ? `mine ${blocks} block(s) on ${n.name} — it is cut from the ${cut.join(' and ')} plane, so this forks the chain`
    : `mine ${blocks} block${blocks === 1 ? '' : 's'} on ${n.name} (RPC ${address ? 'generatetoaddress' : 'generate'}; the coinbase pays ${address ? 'the address given' : `${n.name}'s own miner key`})`;

  const caughtUp = !!mined && n.tip?.toLowerCase() === mined.toLowerCase();
  return (
    <>
      <button className="primary" onClick={mine} disabled={st?.busy || !n.reachable} title={title}>
        {st?.busy ? 'mining…' : label}
      </button>
      <Status st={st} />
      {mined && (
        // order:1 keeps this last in the flex row (after any sibling control), flex-basis breaks it
        // onto its own line rather than widening the card.
        <div className="small muted" style={{ order: 1, flexBasis: '100%', margin: '4px 0 0' }}>
          new block <Tip hash={mined} n={8} />{' '}
          {caughtUp ? <>· tip @ {n.height}</> : <span className="warn">· waiting for the snapshot…</span>}
        </div>
      )}
    </>
  );
}

/** Cut or heal one network plane for a node. Reversible, and the resulting state is already
 *  visible as a `<plane> plane cut` pill in the card header. */
export function PartitionToggle({ n, status, run, plane }: ActionProps & { plane: 'alert' | 'p2p' }) {
  const key = akey(`part-${plane}`, n.name);
  const st = status[key];
  const cut = (n.partitions ?? []).includes(plane);
  const net = plane === 'alert' ? 'alertnet' : 'chaosnet';
  // An SV node is not on the alert network itself; its go-alert-system sidecar is.
  const target = plane === 'alert' && isSV(n) ? `${n.name}'s alert sidecar` : n.name;
  const toggle = () => run(key, () => api.partition(n.name, plane, !cut),
    () => (cut ? `${plane} plane reconnected` : `${plane} plane disconnected`));
  return (
    <>
      <button className={cut ? '' : 'danger'} onClick={toggle} disabled={st?.busy}
        title={cut ? `reconnect ${target} to ${net} with its pinned IP` : `disconnect ${target} from ${net} (podman network disconnect)`}>
        {cut ? `heal ${plane}` : `cut ${plane === 'alert' && isSV(n) ? 'sidecar alert' : plane}`}
      </button>
      <Status st={st} />
    </>
  );
}

/** Container lifecycle. `stop` takes the node out of the cluster until someone starts it again,
 *  so it keeps the two-step confirm wherever it is rendered. */
export function ContainerControls({ n, status, run }: ActionProps) {
  const [confirmStop, setConfirmStop] = useState(false);
  const key = akey('container', n.name);
  const st = status[key];
  const act = (action: 'pause' | 'unpause' | 'stop' | 'start') =>
    run(key, () => api.chaos(n.name, action), () => `${action} ok`);
  return (
    <>
      <button onClick={() => act('pause')} disabled={st?.busy} title="SIGSTOP the container (freezes the process, keeps network)">pause</button>
      <button onClick={() => act('unpause')} disabled={st?.busy}>unpause</button>
      {confirmStop ? (
        <span className="danger-confirm">
          <span className="small">stop {n.name}?</span>
          <button className="danger" onClick={() => { setConfirmStop(false); void act('stop'); }}>yes, stop</button>
          <button onClick={() => setConfirmStop(false)}>cancel</button>
        </span>
      ) : (
        <button className="danger" onClick={() => setConfirmStop(true)} disabled={st?.busy} title="stop the container (asks for confirmation)">stop…</button>
      )}
      <button onClick={() => act('start')} disabled={st?.busy}>start</button>
      <Status st={st} />
    </>
  );
}

/** The Fleet card's action footer: mine is always there; the destructive controls sit behind a
 *  disclosure, because seven always-visible buttons roughly double the card height at the 320px
 *  grid minimum and a bare `stop` on the page you leave open all day is a footgun. */
export function NodeControls({ n, status, run }: ActionProps) {
  const [more, setMore] = useState(false);
  return (
    <div style={{ marginTop: 10, borderTop: '1px solid var(--line)', paddingTop: 8 }}>
      <div className="row">
        {isSV(n)
          ? <span className="small muted" title="SV nodes follow the teranodes over the legacy service; mine on a teranode">follower</span>
          : <MineControl n={n} status={status} run={run} />}
        <button className="link" onClick={() => setMore(!more)} title="network partitions and container lifecycle for this node">
          {more ? '▾ chaos' : '▸ chaos'}
        </button>
      </div>
      {more && (
        <>
          <div className="row" style={{ marginTop: 6 }}>
            <PartitionToggle n={n} status={status} run={run} plane="alert" />
            <PartitionToggle n={n} status={status} run={run} plane="p2p" />
          </div>
          <div className="row" style={{ marginTop: 6 }}>
            <ContainerControls n={n} status={status} run={run} />
          </div>
        </>
      )}
    </div>
  );
}
