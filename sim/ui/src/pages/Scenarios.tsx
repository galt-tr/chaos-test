import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, type Run, type Scenario, type StepResult } from '../api';
import { KnownTxids, useLiveCtx } from '../live';
import { ago, fmtTime, useActions } from '../hooks';
import { Empty, Json, Linkify, Status, StatusPill } from '../ui';
import { collectTxids } from '../txids';

const ACTIVE = (s: string) => s === 'running' || s === 'paused';

export function ScenariosPage() {
  const { snapshot, events } = useLiveCtx();
  const nodeNames = (snapshot?.nodes ?? []).map((n) => n.name);
  const [scenarios, setScenarios] = useState<Scenario[] | null>(null);
  const [scErr, setScErr] = useState('');
  const [runs, setRuns] = useState<Run[]>([]);
  const [runsErr, setRunsErr] = useState('');
  const [selected, setSelected] = useState<string | null>(null);
  const [run, setRun] = useState<Run | null>(null);
  const [runErr, setRunErr] = useState('');

  const loadScenarios = useCallback(() => api.scenarios().then((s) => { setScenarios(s ?? []); setScErr(''); }).catch((e: Error) => setScErr(e.message)), []);
  const loadRuns = useCallback(() => api.runs().then((r) => { setRuns(r ?? []); setRunsErr(''); }).catch((e: Error) => setRunsErr(e.message)), []);
  useEffect(() => { void loadScenarios(); }, [loadScenarios]);
  useEffect(() => { if (scErr) { const t = setTimeout(loadScenarios, 5000); return () => clearTimeout(t); } }, [scErr, loadScenarios]);

  // Runs list: on mount, on every `scenario` event, and every 5s.
  const lastScenarioEv = useMemo(() => { for (let i = events.length - 1; i >= 0; i--) if (events[i].kind === 'scenario') return events[i].id; return 0; }, [events]);
  useEffect(() => { void loadRuns(); }, [loadRuns, lastScenarioEv]);
  useEffect(() => { const t = setInterval(loadRuns, 5000); return () => clearInterval(t); }, [loadRuns]);

  // Auto-select the most recent active run when nothing is selected.
  useEffect(() => {
    if (selected && runs.some((r) => r.id === selected)) return;
    const active = runs.find((r) => ACTIVE(r.status)) ?? runs[0];
    if (active) setSelected(active.id);
  }, [runs, selected]);

  // Selected run: refresh on scenario events and every 1.5s while active (5s otherwise).
  const active = run ? ACTIVE(run.status) : true;
  const loadRun = useCallback(() => {
    if (!selected) return;
    api.run(selected).then((r) => { setRun(r); setRunErr(''); }).catch((e: Error) => setRunErr(e.message));
  }, [selected]);
  useEffect(() => { setRun(null); loadRun(); }, [loadRun]);
  useEffect(() => { loadRun(); }, [loadRun, lastScenarioEv]);
  useEffect(() => { const t = setInterval(loadRun, active ? 1500 : 5000); return () => clearInterval(t); }, [loadRun, active]);

  const started = (r: Run) => { setRuns((x) => [r, ...x]); setSelected(r.id); setRun(r); };
  const scenarioById = useMemo(() => new Map((scenarios ?? []).map((s) => [s.id, s])), [scenarios]);

  return (
    <>
      <div className="two">
        <div className="panel">
          <h2>scenarios <span className="muted lower">{scenarios ? `${scenarios.length} loaded` : 'loading…'}</span></h2>
          {scErr && <div className="error small">{scErr} — retrying</div>}
          {scenarios && scenarios.length === 0 && <Empty>No scenarios loaded by the orchestrator.</Empty>}
          {(scenarios ?? []).map((s) => <ScenarioCard key={s.id} s={s} nodeNames={nodeNames} onStarted={started} busy={runs.some((r) => ACTIVE(r.status))} />)}
        </div>
        <div>
          <RunView run={run} err={runErr} scenario={run ? scenarioById.get(run.scenarioId) : undefined} onChanged={() => { loadRun(); void loadRuns(); }} />
          <div className="panel">
            <h2>run history <span className="muted lower">{runs.length}</span></h2>
            {runsErr && <div className="error small">{runsErr}</div>}
            {runs.length === 0 && <Empty>no runs yet</Empty>}
            {runs.length > 0 && (
              <table className="compact runs">
                <thead><tr><th>run</th><th>scenario</th><th>mode</th><th>status</th><th>steps</th><th>findings</th><th>started</th><th>took</th></tr></thead>
                <tbody>
                  {runs.map((r) => {
                    const steps = r.steps ?? [];
                    const passed = steps.filter((s) => s.status === 'passed').length;
                    return (
                      <tr key={r.id} className={r.id === selected ? 'sel' : ''} onClick={() => setSelected(r.id)}>
                        <td><code>{r.id}</code></td>
                        <td>{r.scenarioId}</td>
                        <td>{r.mode}</td>
                        <td><StatusPill status={r.status} /></td>
                        <td>{passed}/{steps.length}</td>
                        <td className={r.findings?.length ? 'warn' : 'muted'}>{r.findings?.length ?? 0}</td>
                        <td className="muted">{fmtTime(r.startedAt)}</td>
                        <td className="muted">{duration(r.startedAt, r.finishedAt)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        </div>
      </div>
    </>
  );
}

function duration(start?: string, end?: string) {
  if (!start) return '';
  const a = new Date(start).getTime();
  const b = end ? new Date(end).getTime() : Date.now();
  const s = Math.max(0, Math.round((b - a) / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s`;
}

function ScenarioCard({ s, nodeNames, onStarted, busy }: { s: Scenario; nodeNames: string[]; onStarted: (r: Run) => void; busy: boolean }) {
  const [open, setOpen] = useState(false);
  const [roles, setRoles] = useState<Record<string, string>>(s.roles ?? {});
  const [params, setParams] = useState<Record<string, string>>(() => Object.fromEntries(Object.entries(s.params ?? {}).map(([k, v]) => [k, typeof v === 'string' ? v : JSON.stringify(v)])));
  const { status, run } = useActions();
  const steps = s.steps ?? [];
  const assertCount = steps.reduce((n, st) => n + (st.assert?.length ?? 0), 0);
  const roleNodes = Array.from(new Set([...nodeNames, ...Object.values(s.roles ?? {})]));

  const start = (mode: 'auto' | 'step') => {
    const p: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(params)) {
      const def = s.params?.[k];
      if (typeof def === 'number') p[k] = Number(v);
      else if (typeof def === 'boolean') p[k] = v === 'true';
      else if (typeof def === 'string') p[k] = v;
      else { try { p[k] = JSON.parse(v); } catch { p[k] = v; } }
    }
    void run(`start`, () => api.runScenario(s.id, mode, roles, p), (r) => `started ${r.id} (${mode})`).then((r) => { if (r) onStarted(r); });
  };

  return (
    <div className="step" style={{ borderLeftColor: 'var(--accent)' }}>
      <div className="hdr">
        <button className="link" onClick={() => setOpen(!open)}>{open ? '▾' : '▸'}</button>
        <b>{s.name}</b>
        <code className="muted small">{s.id}</code>
        <span className="muted small">{steps.length} steps · {assertCount} assertions{s.requires?.length ? ` · requires ${s.requires.join(', ')}` : ''}</span>
        <span className="spacer" style={{ flex: 1 }} />
        <button className="primary" onClick={() => start('auto')} disabled={status.start?.busy} title={busy ? 'another run is active; the engine may refuse concurrent runs' : 'run all steps'}>run auto</button>
        <button onClick={() => start('step')} disabled={status.start?.busy} title="pause before each step; advance with Next">run step</button>
      </div>
      <Status st={status.start} />
      {open && (
        <div style={{ marginTop: 6 }}>
          <div className="desc">{s.description}</div>
          <div className="row">
            {Object.keys(roles).length > 0 && (
              <fieldset style={{ margin: 0 }}><legend>roles</legend>
                <div className="row">
                  {Object.entries(roles).map(([role, node]) => (
                    <label key={role}>{role}
                      <select value={node} onChange={(e) => setRoles({ ...roles, [role]: e.target.value })}>
                        {roleNodes.map((n) => <option key={n} value={n}>{n}</option>)}
                      </select>
                    </label>
                  ))}
                </div>
              </fieldset>
            )}
            {Object.keys(params).length > 0 && (
              <fieldset style={{ margin: 0 }}><legend>params</legend>
                <div className="row">
                  {Object.entries(params).map(([k, v]) => (
                    <label key={k}>{k} <span className="muted">({typeof s.params?.[k]})</span>
                      <input value={v} onChange={(e) => setParams({ ...params, [k]: e.target.value })} style={{ width: 110 }} />
                    </label>
                  ))}
                </div>
              </fieldset>
            )}
          </div>
          <table className="compact" style={{ marginTop: 8 }}>
            <thead><tr><th>#</th><th>step</th><th>action</th><th>with</th><th>asserts</th></tr></thead>
            <tbody>
              {steps.map((st, i) => (
                <tr key={i}>
                  <td className="muted">{i + 1}</td>
                  <td>{st.name}{st.as ? <span className="muted small"> → ${'{'}{st.as}{'}'}</span> : null}</td>
                  <td><code>{st.action}</code></td>
                  <td className="small muted wrap">{st.with ? Object.entries(st.with).map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`).join(' ') : ''}</td>
                  <td className="small muted">{st.assert?.map((a, j) => <div key={j}>{a.should ? <span className="warn" title="should: a failure is recorded as a finding, not an abort">should </span> : ''}<code>{a.check}</code>{a.timeout ? ` ≤${a.timeout}` : ''}</div>)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function RunView({ run, err, scenario, onChanged }: { run: Run | null; err: string; scenario?: Scenario; onChanged: () => void }) {
  const { status, run: act } = useActions();
  // Every txid this run produced (spend/coinbase/submit outputs), so truncated hashes in
  // assertion details and findings can be linked.
  const known = useMemo(() => collectTxids({ vars: run?.vars, steps: run?.steps }), [run]);
  if (!run) {
    return <div className="panel"><h2>run</h2>{err ? <div className="error small">{err}</div> : <Empty>Select a run from the history or start a scenario.</Empty>}</div>;
  }
  const steps = run.steps ?? [];
  const findings = run.findings ?? [];
  const isActive = ACTIVE(run.status);
  const next = () => act('next', () => api.runNext(run.id), () => 'advanced').then(onChanged);
  const abort = () => act('abort', () => api.runAbort(run.id), () => 'abort requested').then(onChanged);
  const passed = steps.filter((s) => s.status === 'passed').length;
  return (
    <KnownTxids extra={known}>
    <div className="panel">
      <div className="row between">
        <h2 style={{ margin: 0 }}>run <code className="lower">{run.id}</code> <span className="muted lower">{scenario?.name ?? run.scenarioId}</span></h2>
        <span className="row">
          <StatusPill status={run.status} />
          <span className="muted small">{run.mode} · {passed}/{steps.length} steps · {duration(run.startedAt, run.finishedAt)}{run.finishedAt ? '' : ' (running)'}</span>
          {run.mode === 'step' && isActive && <button className="primary" onClick={next} disabled={run.status !== 'paused' || status.next?.busy} title={run.status === 'paused' ? 'execute the next step' : 'a step is executing'}>next ▸</button>}
          {isActive && <button className="danger" onClick={abort} disabled={status.abort?.busy}>abort</button>}
          <Status st={status.next} /><Status st={status.abort} />
        </span>
      </div>
      {err && <div className="error small">{err}</div>}
      <div className="row small muted" style={{ marginTop: 4 }}>
        <span>started {fmtTime(run.startedAt)} ({ago(run.startedAt)} ago)</span>
        {Object.entries(run.roles ?? {}).map(([r, n]) => <span key={r}><b>{r}</b>={n}</span>)}
        {Object.entries(run.params ?? {}).map(([k, v]) => <span key={k}>{k}={typeof v === 'string' ? v : JSON.stringify(v)}</span>)}
      </div>
      {run.error && <div className="error" style={{ margin: '6px 0' }}><Linkify text={run.error} /></div>}
      {findings.length > 0 && (
        <div style={{ margin: '8px 0' }}>
          <h3 style={{ margin: '4px 0' }}>findings ({findings.length})</h3>
          {findings.map((f, i) => <div key={i} className="finding"><Linkify text={f} /></div>)}
        </div>
      )}
      <div style={{ marginTop: 8 }}>
        {steps.length === 0 && <Empty>no steps recorded</Empty>}
        {steps.map((st) => <StepView key={st.index} st={st} current={run.currentStep === st.index && isActive} paused={run.status === 'paused' && run.currentStep === st.index} />)}
      </div>
      {run.vars && Object.keys(run.vars).length > 0 && <Json v={run.vars} label={`vars (${Object.keys(run.vars).length})`} />}
    </div>
    </KnownTxids>
  );
}

function StepView({ st, current, paused }: { st: StepResult; current: boolean; paused: boolean }) {
  const cls = paused ? 'paused' : st.status;
  const asserts = st.assertions ?? [];
  return (
    <div className={`step ${cls}`}>
      <div className="hdr">
        <span className="idx">{st.index + 1}</span>
        <b>{st.name}</b>
        <code className="muted small">{st.action}</code>
        <StatusPill status={paused ? 'paused' : st.status} />
        {current && !paused && <span className="accent small">◉ current</span>}
        {paused && <span className="warn small">waiting for Next</span>}
        <span className="muted small">{st.startedAt ? duration(st.startedAt, st.finishedAt) : ''}</span>
      </div>
      {st.error && <div className="error small wrap" style={{ marginTop: 4 }}><Linkify text={st.error} /></div>}
      {asserts.length > 0 && (
        <div style={{ marginTop: 4 }}>
          {asserts.map((a, i) => (
            <div key={i} className="assert">
              <span className={a.passed ? 'ok' : a.should ? 'warn' : 'bad'}>{a.passed ? '✓' : '✗'}</span>
              <span><code>{a.check}</code>{a.should && !a.passed ? <span className="warn small"> finding</span> : a.should ? <span className="muted small"> should</span> : null}</span>
              <span className="d"><Linkify text={a.detail} /></span>
              <span className="muted small">{a.duration}</span>
            </div>
          ))}
        </div>
      )}
      {st.resolved && Object.keys(st.resolved).length > 0 && <Json v={st.resolved} label="resolved inputs" />}
      {st.output !== undefined && st.output !== null && <Json v={st.output} label="output" />}
    </div>
  );
}
