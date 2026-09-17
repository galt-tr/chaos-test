export type NodeState = {
  name: string; index: number; reachable: boolean; height: number; tip: string; fsm: string;
  mempoolCount: number; mempool?: string[]; alertSeq: number; alertReachable: boolean; version?: string;
  container: string; partitions?: string[]; error?: string; updatedAt: string;
};
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
};
