import { useMemo, useState, type ReactNode } from 'react';
import type { NodeState } from './api';
import { type ActionState, copy, hashColor, short } from './hooks';
import { useArcadeBase, useKnownTxids } from './live';
import { HEX64, NOT_TX_KEYS, isTxKey, linkifyParts } from './txids';

/** Short hash in mono; hover shows the full value, click copies it. For block hashes, alert
 *  hashes and scripts — use TxLink for transaction ids. */
export function Hash({ h, n = 10, className = '' }: { h?: string; n?: number; className?: string }) {
  if (!h) return <span className="muted">—</span>;
  return <code className={`hash ${className}`} title={`${h} (click to copy)`} onClick={() => copy(h)}>{short(h, n)}</code>;
}

export function Tip({ hash, n = 10 }: { hash?: string; n?: number }) {
  if (!hash) return <span className="muted">—</span>;
  return <span className="tipwrap"><span className="tip" style={{ background: hashColor(hash) }} /><Hash h={hash} n={n} /></span>;
}

/** A transaction id: the text opens the tx in arcade in a new tab, the glyph copies it.
 *  Without an arcade base URL it degrades to a copy-only Hash. */
export function TxLink({ txid, n = 10, label, className = '' }: { txid?: string; n?: number; label?: string; className?: string }) {
  const base = useArcadeBase();
  if (!txid) return <span className="muted">—</span>;
  if (!base) return <Hash h={txid} n={n} className={className} />;
  const text = label ?? (n >= 64 ? txid : short(txid, n));
  return (
    <span className={`txlink ${className}`}>
      <a className="hash" href={`${base}/tx/${txid}`} target="_blank" rel="noreferrer" title={`${txid}\nopen in arcade ↗`} onClick={(e) => e.stopPropagation()}>{text}</a>
      <button className="copy" title="copy txid" onClick={(e) => { e.stopPropagation(); copy(txid); }}>⧉</button>
    </span>
  );
}

/** Inline list of txids, collapsed after `max`; `total` notes a longer list the server truncated. */
export function TxList({ txids, n = 8, max = 5, total }: { txids: string[]; n?: number; max?: number; total?: number }) {
  const [all, setAll] = useState(false);
  const shown = all ? txids : txids.slice(0, max);
  return (
    <span className="txlist">
      {shown.map((t) => <TxLink key={t} txid={t} n={n} />)}
      {txids.length > max && !all && <button className="link" onClick={(e) => { e.stopPropagation(); setAll(true); }}>+{txids.length - max} more</button>}
      {total !== undefined && total > txids.length && <span className="muted small">({total} total, first {txids.length} listed)</span>}
    </span>
  );
}

/** Free text with known txids (full or truncated) turned into TxLinks. */
export function Linkify({ text, all = false }: { text: string; all?: boolean }) {
  const known = useKnownTxids();
  const parts = useMemo(() => linkifyParts(text, known, all), [text, known, all]);
  return <>{parts.map((p, i) => (typeof p === 'string' ? p : <TxLink key={i} txid={p.txid} label={p.label} />))}</>;
}

export function Pill({ ok, children, title, className = '' }: { ok?: boolean | null; children: ReactNode; title?: string; className?: string }) {
  const cls = ok === true ? 'ok' : ok === false ? 'bad' : '';
  return <span className={`pill ${cls} ${className}`} title={title}>{children}</span>;
}

export function StatusPill({ status }: { status?: string }) {
  const s = status || 'UNKNOWN';
  return <span className={`pill status-${s}`}>{s}</span>;
}

/** Inline result of an action next to its button. */
export function Status({ st, okPrefix = '' }: { st?: ActionState; okPrefix?: string }) {
  if (!st) return null;
  if (st.busy) return <span className="status muted">working…</span>;
  if (st.err) return <span className="status bad" title={st.err}>✗ {st.err}</span>;
  if (st.ok) return <span className="status ok" title={st.ok}>✓ {okPrefix}<Linkify text={st.ok} /></span>;
  return null;
}

/** Collapsible JSON view. Strings under txid keys become TxLinks (see txids.ts); other strings
 *  are linkified against the known-txid set; `txKeys` names extra keys to treat as txids. */
export function Json({ v, open = false, label = 'data', txKeys }: { v: unknown; open?: boolean; label?: string; txKeys?: Set<string> }) {
  const [o, setO] = useState(open);
  if (v === undefined || v === null) return null;
  return (
    <div className="json">
      <button className="link" onClick={(e) => { e.stopPropagation(); setO(!o); }}>{o ? '▾' : '▸'} {label}</button>
      {o && <pre>{typeof v === 'string' ? <Linkify text={v} /> : <JsonTree v={v} txKeys={txKeys} />}</pre>}
    </div>
  );
}

/** JSON.stringify(v, null, 2) as React nodes, with txid-valued strings rendered as TxLinks. */
export function JsonTree({ v, txKeys }: { v: unknown; txKeys?: Set<string> }) {
  return <>{renderJson(v, 0, '', txKeys)}</>;
}

function renderJson(v: unknown, depth: number, key: string, txKeys?: Set<string>): ReactNode {
  const pad = '  '.repeat(depth + 1);
  const padEnd = '  '.repeat(depth);
  if (v === null || v === undefined) return 'null';
  if (typeof v === 'string') {
    if (isTxKey(key, txKeys) && HEX64.test(v)) return <>"<TxLink txid={v.toLowerCase()} n={64} />"</>;
    const q = JSON.stringify(v);
    if (NOT_TX_KEYS.has(key)) return q;
    return <>"<Linkify text={q.slice(1, -1)} />"</>;
  }
  if (typeof v !== 'object') return JSON.stringify(v);
  if (Array.isArray(v)) {
    if (v.length === 0) return '[]';
    return <>{'[\n'}{v.map((x, i) => <span key={i}>{pad}{renderJson(x, depth + 1, key, txKeys)}{i < v.length - 1 ? ',' : ''}{'\n'}</span>)}{padEnd}]</>;
  }
  const entries = Object.entries(v as Record<string, unknown>);
  if (entries.length === 0) return '{}';
  return <>{'{\n'}{entries.map(([k, x], i) => <span key={k}>{pad}{JSON.stringify(k)}: {renderJson(x, depth + 1, k, txKeys)}{i < entries.length - 1 ? ',' : ''}{'\n'}</span>)}{padEnd}{'}'}</>;
}

export function NodeSelect({ nodes, value, onChange, extra = [], allowEmpty = false }:
  { nodes: NodeState[]; value: string; onChange: (v: string) => void; extra?: string[]; allowEmpty?: boolean }) {
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)}>
      {allowEmpty && <option value="">(default)</option>}
      {nodes.map((n) => <option key={n.name} value={n.name}>{n.name}</option>)}
      {extra.map((x) => <option key={x} value={x}>{x}</option>)}
    </select>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty muted">{children}</div>;
}

export function KV({ k, children, title }: { k: string; children: ReactNode; title?: string }) {
  return <><dt title={title}>{k}</dt><dd>{children}</dd></>;
}
