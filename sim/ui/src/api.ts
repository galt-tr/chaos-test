export type NodeKind = 'teranode' | 'svnode';
export type NodeState = {
  kind?: NodeKind; name: string; index: number; reachable: boolean; height: number; tip: string; fsm: string;
  peers?: number; mempoolCount: number; mempool?: string[]; alertSeq: number; alertReachable: boolean;
  alertSource?: 'node' | 'sidecar'; alertUnprocessed?: number; sidecarURL?: string; sidecarContainer?: string;
  version?: string; container: string; partitions?: string[]; error?: string; updatedAt: string;
  /** The node's own asset dashboard (teranodes), reachable from the browser (static config). */
  hostURL?: string;
};
export const isSV = (n: NodeState) => n.kind === 'svnode';
export type ArcadeDatahub = { url: string; node?: string; source?: string; healthy: boolean };
export type ArcadeState = {
  configured: boolean; reachable: boolean; healthy: boolean; version?: string; status?: string;
  blockHeight: number; height: number; tip: string; datahubs?: ArcadeDatahub[] | null;
  url: string; chaintracksURL?: string; hostURL?: string; hostHealthURL?: string; hostEventsURL?: string; hostChaintracksURL?: string;
  error?: string; tipError?: string; updatedAt: string;
};
export type Snapshot = {
  network: string; nodes: NodeState[];
  hub: { reachable: boolean; sequence: number; activePeers: number; unprocessed: number; error?: string };
  arcade?: ArcadeState;
  services: { name: string; url: string; hostURL?: string; healthy: boolean; detail?: string }[] | null;
  watched: { txid: string; vout: number; label?: string; perNode: Record<string, { status: string; spentBy?: string; satoshis?: number; utxoHash?: string; error?: string }> }[] | null;
  updatedAt: string;
};
export type Event = { id: number; time: string; kind: string; node?: string; message: string; data?: Record<string, unknown> };
export type AlertFund = { txid: string; vout: number; enforceAtHeightStart: number; enforceAtHeightStop: number; policyExpiresWithConsensus: boolean };
export type AlertSummary = { sequence: number; type: string; hash: string; text: string; note?: string; createdAt: string; funds?: AlertFund[] | null };
export type Inventory = {
  network: string; nodes: { name: string; index: number; hostAssetURL: string; hostRPCURL: string; alertAddr: string }[];
  hub?: { hostAPIURL: string }; services?: Record<string, { hostURL?: string; url: string; urls?: Record<string, { url: string; hostURL?: string }> }>;
};
export type AssertResult = { check: string; passed: boolean; should: boolean; detail: string; duration: string };
export type StepResult = { index: number; name: string; action: string; status: string; error?: string; output?: unknown;
  startedAt?: string; finishedAt?: string; resolved?: Record<string, unknown>; assertions?: AssertResult[] };
export type Run = {
  id: string; scenarioId: string; mode: string; status: string; roles: Record<string, string>; params: Record<string, unknown>;
  vars: Record<string, unknown>; findings: string[] | null; startedAt: string; finishedAt?: string; currentStep: number; error?: string;
  steps: StepResult[] | null;
};
export type ScenarioStep = { name: string; action: string; with?: Record<string, unknown>; as?: string; timeout?: string; note?: string;
  assert?: { check: string; with?: Record<string, unknown>; timeout?: string; should?: boolean; message?: string }[] };
export type Scenario = { id: string; name: string; description: string; requires?: string[]; roles: Record<string, string>; params: Record<string, unknown>; steps: ScenarioStep[] | null; source?: string };
export type KeyEntry = { name: string; address: string; note?: string; lockingScript?: string; privateKeyHex?: string };


// ---- logs ---------------------------------------------------------------------------------

/** One parsed container log line. `cont` holds the continuation lines of a wrapped error. */
export type LogLine = {
  seq: number; at: string; logAt?: string; level: string; service?: string; source?: string;
  msg: string; cont?: string[]; continuation?: boolean;
  rootCause?: string; code?: string; codeNum?: number; raw?: string;
};

/** One fetch of a node's log. `levels`/`services` count the window BEFORE filtering, so the
 *  UI can show what the current filter is hiding. */
export type LogPage = {
  node: string; container: string; runtime: string;
  lines: LogLine[]; nextCursor: string;
  reset: boolean; resetReason?: string;
  scanned: number; matched: number; dropped: number; truncated: boolean;
  levels: Record<string, number>; services: Record<string, number>;
  fetchedAt: string; elapsed: string;
};

export type LogQuery = {
  node: string; cursor?: string; since?: string; tail?: number; limit?: number;
  level?: string; service?: string; grep?: string; regex?: boolean; caseSensitive?: boolean;
};

// ---- diagnostics --------------------------------------------------------------------------

export type SourceStatus = { name: string; ok: boolean; error?: string; reason?: string; httpStatus?: number; elapsed: string };
export type FSMInfo = { state: string; stateValue: number; legalEvents?: string[] };
export type PreviousAttempt = {
  peer_id: string; peer_url: string; target_block_hash: string; target_block_height: number;
  error_message: string; error_type: string; attempt_time: number; duration_ms: number;
  blocks_validated: number; peerNode?: string;
};
export type CatchupStatus = {
  is_catching_up: boolean; peer_id: string; peer_url: string;
  target_block_hash: string; target_block_height: number; current_height: number;
  total_blocks: number; blocks_fetched: number; blocks_validated: number;
  fork_depth: number; common_ancestor_hash: string; common_ancestor_height: number;
  start_time: number; duration_ms: number;
  previous_attempt?: PreviousAttempt; peerNode?: string;
};
export type DiagPeer = {
  id: string; client_name: string; transport?: string; height: number; block_hash: string;
  data_hub_url?: string; is_connected: boolean; is_banned: boolean; ban_score: number;
  connected_at?: number; catchup_attempts?: number; catchup_successes?: number; catchup_failures?: number;
  catchup_reputation_score?: number; catchup_avg_response_ms?: number;
  last_catchup_error?: string; last_catchup_error_time?: number; node?: string;
};
export type InvalidBlock = {
  height: number; hash: string; previousblockhash: string; miner: string; timestamp: string;
  transactionCount: number; size: number; coinbaseValue: number;
  minerNode?: string; rejectReason?: string; rejectRootCause?: string; rejectCode?: string;
};
export type HealthDep = { resource: string; status: number; error?: string; message?: string; seenIn?: string[]; ok: boolean };
export type HealthInfo = { overallStatus: number; parsed: boolean; raw?: string; services: number; checks: number; deps: HealthDep[]; unhealthy: string[] };
export type LogHint = { services?: string; level?: string; grep?: string };
export type Verdict = {
  class: string; severity: string; headline: string;
  details?: string[]; evidence?: string[]; missing?: string[]; confidence: string;
  suggest?: string[]; logHint?: LogHint;
};
/** A null section means UNKNOWN, not "nothing wrong" — check `sources` for why. */
export type Diagnostics = {
  node: string; container: string; containerState?: string; fetchedAt: string; elapsed: string;
  sources: SourceStatus[]; degraded: boolean;
  tip?: { height: number; hash: string };
  fsm?: FSMInfo; catchup?: CatchupStatus; peers?: DiagPeer[];
  heights?: { block_assembly_height: number | null; block_persister_height: number | null };
  invalidBlocks?: InvalidBlock[]; health?: HealthInfo;
  verdict: Verdict;
};
export type FleetNodeBrief = { name: string; height: number; tip: string; reachable: boolean; fsm: string; partitions?: string[] };
export type FleetContext = {
  maxHeight: number; majorityTip: string; majorityTipHeight: number; majorityCount: number;
  nodes: Record<string, FleetNodeBrief>; snapshotAt: string;
};
export type DiagReport = { nodes: Diagnostics[]; fleet: FleetContext; fetchedAt: string; cached: boolean; elapsed: string };

/** Error bodies carry a machine-readable `reason`; branch on it, never on the message. */
export type ApiFailure = { error: string; reason?: string; runtime?: string; container?: string };

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const r = await fetch(path, { method, headers: body ? { 'Content-Type': 'application/json' } : undefined, body: body ? JSON.stringify(body) : undefined });
  const text = await r.text();
  let data: unknown = text;
  try { data = JSON.parse(text); } catch { /* keep text */ }
  if (!r.ok) throw new Error((data as { error?: string })?.error ?? `${r.status} ${text}`);
  return data as T;
}
export const api = {
  state: () => req<{ snapshot: Snapshot; alerts: AlertSummary[] | null; runtime: string; alertHost: boolean }>('GET', '/api/state'),
  inventory: () => req<Inventory>('GET', '/api/inventory'),
  recent: (n = 300) => req<Event[]>('GET', `/api/events/recent?n=${n}`),
  mine: (node: string, blocks: number, address?: string) => req<{ node: string; hashes: string[] }>('POST', '/api/mine', { node, blocks, address }),
  alerts: () => req<AlertSummary[] | null>('GET', '/api/alerts'),
  buildAlert: (body: unknown) => req<{ sequence: number; typeName?: string; text: string; hash?: string }>('POST', '/api/alerts/build', body),
  // The Go PushResult has no json tags, so the wire keys are `Delivered`/`PeerLatestBefore`; both spellings are accepted here.
  pushAlert: (node: string, sequence: number) => req<{ delivered?: number[]; Delivered?: number[]; PeerLatestBefore?: number }>('POST', '/api/alerts/push', { node, sequence }),
  rpcFreeze: (body: unknown) => req('POST', '/api/alerts/rpc', body),
  keys: () => req<KeyEntry[] | null>('GET', '/api/keys'),
  newKey: (name: string, note?: string) => req<KeyEntry>('POST', '/api/keys', { name, note }),
  spend: (body: unknown) => req<{ txid: string; hex: string; efHex: string; size?: number }>('POST', '/api/tx/spend', body),
  submit: (target: string, hex: string, efHex?: string) => req<{ target?: string; txid?: string; accepted: boolean; status: number; body?: string }>('POST', '/api/tx/submit', { target, hex, efHex }),
  tx: (txid: string, node?: string) => req<Record<string, unknown>>('GET', `/api/tx/${txid}${node ? `?node=${node}` : ''}`),
  coinbase: (node: string, height: number) => req<{ height: number; hash: string; txid: string; hex: string }>('GET', `/api/coinbase?node=${node}&height=${height}`),
  watch: (txid: string, vout: number, label?: string) => req('POST', '/api/watch', { txid, vout, label }),
  unwatch: (txid: string, vout: number) => req('DELETE', `/api/watch/${txid}/${vout}`),
  partition: (node: string, plane: string, on: boolean) => req('POST', '/api/chaos/partition', { node, plane, on }),
  chaos: (node: string, action: string) => req('POST', `/api/chaos/${action}`, { node }),
  scenarios: () => req<Scenario[] | null>('GET', '/api/scenarios'),
  runScenario: (id: string, mode: string, roles?: Record<string, string>, params?: Record<string, unknown>) => req<Run>('POST', `/api/scenarios/${id}/run`, { mode, roles, params }),
  runs: () => req<Run[] | null>('GET', '/api/runs'),
  run: (id: string) => req<Run>('GET', `/api/runs/${id}`),
  runNext: (id: string) => req('POST', `/api/runs/${id}/next`),
  runAbort: (id: string) => req('POST', `/api/runs/${id}/abort`),
  logs: (q: LogQuery) => {
    const p = new URLSearchParams({ node: q.node });
    if (q.cursor) p.set('cursor', q.cursor);
    else if (q.since) p.set('since', q.since);
    else p.set('tail', String(q.tail ?? 500));
    if (q.limit) p.set('limit', String(q.limit));
    if (q.level) p.set('level', q.level);
    if (q.service) p.set('service', q.service);
    if (q.grep) p.set('grep', q.grep);
    if (q.regex) p.set('regex', '1');
    if (q.caseSensitive) p.set('case', '1');
    return req<LogPage>('GET', `/api/logs?${p}`);
  },
  diagnostics: (node?: string, refresh = false) => {
    const p = new URLSearchParams();
    if (node) p.set('node', node);
    if (refresh) p.set('refresh', '1');
    return req<DiagReport>('GET', `/api/diagnostics?${p}`);
  },
};
