/**
 * The wallet's bookkeeping and the network's verdict are two different axes.
 *
 * `createAction` succeeding means the WALLET stored the action. Nothing about the network has
 * happened yet. Presenting that as success would hide exactly the failure this harness exists
 * to catch, so one rule is enforced throughout this module:
 *
 *   green is reserved for arcade MINED / IMMUTABLE. No wallet status produces green.
 */
import { ago } from './hooks';
import { Linkify } from './ui';

/** Arcade's documented statuses. The enum is OPEN: anything unlisted is shown verbatim and
 *  flagged, so a value arcade adds later can never become a silent pass or a silent failure. */
export const ARCADE_STATES = [
  'UNKNOWN', 'RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK',
  'SEEN_MULTIPLE_NODES', 'DOUBLE_SPEND_ATTEMPTED', 'REJECTED', 'PENDING_RETRY',
  'STUMP_PROCESSING', 'MINED', 'IMMUTABLE', 'NOT_FOUND',
];
/** Settled for our purposes. REJECTED is deliberately absent — see PROVISIONAL. */
export const ARCADE_SETTLED = new Set(['MINED', 'IMMUTABLE', 'DOUBLE_SPEND_ATTEMPTED']);
/** Terminal in arcade's model but not final in practice: a later SEEN_* supersedes a REJECTED,
 *  so it is amber, labelled provisional, and never counted as a failure. */
export const ARCADE_PROVISIONAL = new Set(['REJECTED']);
export const ARCADE_GOOD = new Set(['MINED', 'IMMUTABLE']);

const ARCADE_HELP: Record<string, string> = {
  UNKNOWN: 'arcade has no status for this transaction',
  NOT_FOUND: 'arcade has never seen this transaction. Normal right after a submit, and the honest answer for anything broadcast straight to a node',
  RECEIVED: 'arcade accepted the submission; nothing has been handed to a node yet',
  SENT_TO_NETWORK: 'handed to at least one node; no node has confirmed acceptance',
  ACCEPTED_BY_NETWORK: 'a node took it into its mempool',
  SEEN_ON_NETWORK: 'observed coming back from the network',
  SEEN_MULTIPLE_NODES: 'seen on more than one node — the strongest evidence short of a block',
  DOUBLE_SPEND_ATTEMPTED: 'a conflicting spend of the same input was seen',
  REJECTED: 'a node refused it. PROVISIONAL — arcade can supersede this with a later SEEN_*',
  PENDING_RETRY: 'arcade will retry the broadcast',
  STUMP_PROCESSING: 'being processed for its merkle proof',
  MINED: 'in a block',
  IMMUTABLE: 'buried deep enough that arcade calls it immutable',
};

export const WALLET_HELP: Record<string, string> = {
  sending: 'the wallet stored the action and will broadcast it. NOT a network acceptance.',
  unproven: 'broadcast, no merkle proof yet — the wallet is waiting for a block.',
  completed: 'the wallet considers the action done. This still says nothing about what any node did.',
  failed: 'the wallet itself failed the action.',
  aborted: 'aborted; nothing was broadcast.',
  nosend: 'built and signed but not broadcast by the wallet — the legacy path delivered it to a node directly, and its change is parked rather than spendable.',
};

export const arcadeKnown = (s?: string) => !!s && ARCADE_STATES.includes(s);

/** The wallet's bucket. Only a genuine wallet-side failure gets colour: painting `completed`
 *  green is the misreport this module exists to prevent. */
export function WalletBucketPill({ status }: { status?: string }) {
  const s = status || 'unknown';
  return <span className={`pill wb-${s}`} title={WALLET_HELP[s] ?? 'a bucket reported by the wallet'}>{s}</span>;
}

export function ArcadeStatus({ status, at }: { status?: string; at?: string }) {
  if (!status) {
    return <span className="muted small" title="the orchestrator has not asked arcade about this transaction — unknown, not ok">not checked</span>;
  }
  const known = arcadeKnown(status);
  const help = ARCADE_HELP[status] ?? 'not one of the documented values — shown verbatim';
  return (
    <>
      <span className={`pill status-${status}${known ? '' : ' unknown'}`}
        title={`${help}${at ? `\nchecked ${ago(at)} ago` : ''}`}>{status}</span>
      {ARCADE_PROVISIONAL.has(status) &&
        <span className="warn small" title="a later SEEN_* supersedes a REJECTED; this is not a final verdict"> provisional</span>}
      {!known && <span className="muted small" title="unrecognised arcade status"> new?</span>}
    </>
  );
}

/** The pair, always together. Rendering one without the other is the bug. */
export function DualStatus({ wallet, arcade, at }: { wallet?: string; arcade?: string; at?: string }) {
  return (
    <span className="dual">
      <WalletBucketPill status={wallet} />
      <span className="sep" title="left: the wallet's own bookkeeping · right: what arcade saw on the network">→</span>
      <ArcadeStatus status={arcade} at={at} />
    </span>
  );
}

/** Counts outcomes arcade has actually decided. Undecided rows count as neither success nor
 *  failure, and the totals are shown so the ratio can be checked rather than believed. */
export function tally(rows: { arcadeStatus?: string }[]) {
  let decided = 0, mined = 0, provisional = 0, bad = 0;
  for (const r of rows) {
    const s = r.arcadeStatus;
    if (s && ARCADE_PROVISIONAL.has(s)) { provisional++; continue; }
    if (!s || !ARCADE_SETTLED.has(s)) continue;
    decided++;
    if (ARCADE_GOOD.has(s)) mined++; else bad++;
  }
  return { decided, mined, provisional, bad, undecided: rows.length - decided - provisional };
}

/** The standing explanation of why two columns exist. Worth repeating on the page rather than
 *  assuming the reader knows. */
export function HonestyNote() {
  return (
    <div className="small muted">
      <b>wallet</b> is the wallet's own bookkeeping: <code>createAction</code> returns as soon as the action is
      stored, so <i>sending</i> — and with delayed broadcast even <i>completed</i> — can be true for a transaction
      no node ever accepted. <b>arcade</b> is what the network said. Only <span className="ok">MINED</span> is a
      success; a <span className="warn">REJECTED</span> is provisional, because arcade supersedes it with a later
      <code> SEEN_*</code>.
    </div>
  );
}

/** Renders a server-side failure using its reason code rather than its message text. */
export function WalletProblem({ reason, error }: { reason?: string; error?: string }) {
  const hint: Record<string, string> = {
    wallet_disabled: 'The wallet feature is off. Start walletd and pass -walletd to the orchestrator.',
    wallet_unavailable: 'walletd is not reachable. It runs as chaos-walletd under the `wallet` compose profile — `make up` starts it.',
    wallet_not_connected: 'walletd is running but cannot reach wallet-infra (the BRC-100 storage server). Check chaos-wallet-infra and its postgres.',
    arcade_unavailable: 'Arcade is needed to prove a funding transaction before it can be credited.',
    no_spendable_coinbase: 'No unspent mature coinbase was found. Mine more blocks and retry.',
    immature_chain: 'The chain is below coinbase maturity (101 blocks). Mine, or allow the top up to mine for you.',
    proof_timeout: 'The funding transaction has not been mined yet. Mine a block, then finish the top up with the retry button.',
    insufficient_funds: 'No spendable coins. Top up first.',
    legacy_not_supported_in_send: 'The sustained send only goes through arcade.',
  };
  return (
    <div className="finding">
      <b>{hint[reason ?? ''] ?? 'The wallet is unavailable.'}</b>
      {error && <div className="small" style={{ marginTop: 4 }}><Linkify text={error} /></div>}
      {reason && <div className="small muted" style={{ marginTop: 4 }}>reason: <code>{reason}</code></div>}
    </div>
  );
}
