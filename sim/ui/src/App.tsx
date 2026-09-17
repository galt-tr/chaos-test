import { useMemo, useRef, type MouseEvent } from 'react';
import { useLive, useRoute, useTick, ago } from './hooks';
import { LiveCtx, TxidsCtx } from './live';
import { collectTxids } from './txids';
import { FleetPage } from './pages/Fleet';
import { AlertsPage } from './pages/Alerts';
import { ChainPage } from './pages/Chain';
import { ChaosPage } from './pages/Chaos';
import { ScenariosPage } from './pages/Scenarios';
import { LogsPage } from './pages/Logs';
import { DiagnosticsPage } from './pages/Diagnostics';
import { WalletPage } from './pages/Wallet';
import { AutoMineChip } from './automine';

const ROUTES: { id: string; label: string; page: () => JSX.Element }[] = [
  { id: 'fleet', label: 'Fleet', page: FleetPage },
  { id: 'alerts', label: 'Alerts', page: AlertsPage },
  { id: 'wallet', label: 'Wallet', page: WalletPage },
  { id: 'chain', label: 'Chain & UTXOs', page: ChainPage },
  { id: 'chaos', label: 'Chaos', page: ChaosPage },
  { id: 'scenarios', label: 'Scenarios', page: ScenariosPage },
  { id: 'logs', label: 'Logs', page: LogsPage },
  { id: 'diag', label: 'Diagnostics', page: DiagnosticsPage },
];

export function App() {
  const live = useLive();
  const route = useRoute('fleet');
  useTick(1000);
  // Grace period so the disconnect banner does not flash while the first SSE connection opens.
  const mountedAt = useRef(Date.now());
  const showDisconnected = !live.connected && (live.lastMessageAt > 0 || Date.now() - mountedAt.current > 3000);
  const current = ROUTES.find((r) => r.id === route) ?? ROUTES[0];
  const Page = current.page;
  const snap = live.snapshot;
  const updated = snap ? ago(snap.updatedAt) : null;
  // Every full txid seen in structured data; free-text linkification is gated on this set.
  const known = useMemo(() => {
    const s = collectTxids(snap?.watched);
    for (const e of live.events) collectTxids(e.data, '', s, e.kind === 'rejected_tx' ? REJECTED_TX_KEYS : undefined);
    return s;
  }, [snap?.watched, live.events]);
  const arcade = snap?.arcade?.configured ? snap.arcade.hostURL : undefined;
  const openArcade = (e: MouseEvent) => {
    if (!arcade) return;
    e.preventDefault();
    // One named window: repeated clicks refocus it instead of opening more tabs.
    window.open(arcade, 'chaos-arcade', 'popup,width=1280,height=900');
  };
  return (
    <LiveCtx.Provider value={live}>
    <TxidsCtx.Provider value={known}>
      <header>
        <h1>chaos-test</h1>
        <nav>
          {ROUTES.map((r) => <a key={r.id} href={`#/${r.id}`} className={r.id === current.id ? 'active' : ''}>{r.label}</a>)}
        </nav>
        <span className="spacer" />
        <AutoMineChip />
        <span className="muted small">
          {snap ? <>{snap.network} · {snap.nodes.length} nodes · </> : null}
          {updated !== null ? <>updated <b className={live.connected ? '' : 'warn'}>{updated}</b> ago</> : 'waiting for snapshot…'}
        </span>
        {arcade && <a className="btn" href={arcade} onClick={openArcade} title={`open arcade (${arcade}) in its own window`}>arcade ↗</a>}
        <span className={`pill ${live.connected ? 'ok' : showDisconnected ? 'bad' : 'muted'}`} title={live.connected ? 'SSE stream open' : 'SSE stream closed; reconnecting every 2s, polling /api/state every 3s'}>
          {live.connected ? 'live' : showDisconnected ? 'disconnected' : 'connecting…'}
        </span>
      </header>
      {showDisconnected && (
        <div className="banner bad">
          Event stream disconnected — reconnecting to the orchestrator (http://localhost:8600) every 2s.
          {live.lastMessageAt ? <> Last message {ago(live.lastMessageAt)} ago.</> : <> No data received yet.</>}
          {snap ? ' Showing the last known snapshot.' : ''}
        </div>
      )}
      {live.restarts > 0 && live.connected && (
        <div className="banner warn">Orchestrator restarted {live.restarts === 1 ? 'once' : `${live.restarts} times`} since this page loaded; the event log was reloaded from the new process.</div>
      )}
      <main key={current.id}><Page /></main>
    </TxidsCtx.Provider>
    </LiveCtx.Provider>
  );
}

/** For rejected-tx verdicts the `hash` field is a txid. */
const REJECTED_TX_KEYS = new Set(['hash']);
