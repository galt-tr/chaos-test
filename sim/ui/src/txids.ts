/** Recognising transaction ids in structured data and free text (64-hex is also a block hash,
 *  an alert hash or a script, so links are gated on the key or on a dictionary of known txids). */
export const HEX64 = /^[0-9a-f]{64}$/i;
/** JSON keys whose string values are txids. Array elements inherit the parent key. */
export const TX_KEYS = new Set(['txid', 'txId', 'spentBy', 'txids', 'mempool']);
/** JSON keys whose 64-hex values are NOT txids. */
export const NOT_TX_KEYS = new Set(['hash', 'hashes', 'block', 'blockHash', 'tip', 'prev', 'last', 'agreedTip', 'utxoHash',
  'merkleRoot', 'previousHash', 'lockingScript', 'hex', 'efHex', 'address', 'peerID', 'peerId', 'forkMark']);

export function isTxKey(k: string, extra?: Set<string>) { return TX_KEYS.has(k) || (extra?.has(k) ?? false); }

/** Collects every full txid found under tx keys, recursively. A `body` string that looks like
 *  JSON (arcade's submit response) is parsed and searched too. */
export function collectTxids(v: unknown, key = '', out = new Set<string>(), extra?: Set<string>): Set<string> {
  if (v === null || v === undefined) return out;
  if (typeof v === 'string') {
    if (isTxKey(key, extra) && HEX64.test(v)) out.add(v.toLowerCase());
    else if (key === 'body' && v.startsWith('{')) { try { collectTxids(JSON.parse(v), '', out, extra); } catch { /* not JSON */ } }
    return out;
  }
  if (Array.isArray(v)) { for (const x of v) collectTxids(x, key, out, extra); return out; }
  if (typeof v === 'object') for (const [k, x] of Object.entries(v as Record<string, unknown>)) collectTxids(x, k, out, extra);
  return out;
}

/** Resolves a truncated hash (prefix, optional suffix) to the one known txid it abbreviates. */
export function matchTruncated(prefix: string, suffix: string | undefined, known: Set<string>): string | undefined {
  const p = prefix.toLowerCase();
  const sfx = suffix?.toLowerCase();
  let found: string | undefined;
  for (const id of known) {
    if (!id.startsWith(p) || (sfx && !id.endsWith(sfx))) continue;
    if (found) return undefined; // ambiguous
    found = id;
  }
  return found;
}

const HASHRE = /([0-9a-f]{64})|([0-9a-f]{8,63})…([0-9a-f]{4})?/gi;
const isHex = (c: string | undefined) => !!c && /[0-9a-f]/i.test(c);
export type Part = string | { txid: string; label: string };

/** Splits text into plain parts and txid links: a full 64-hex run links when it is a known
 *  txid (or `all`), a truncated `xxxxxxxxxxxx…` / `xxxxxxxxxx…xxxx` run links when it
 *  abbreviates exactly one known txid. Hex runs inside longer hex (raw tx, script) never match. */
export function linkifyParts(text: string, known: Set<string>, all: boolean): Part[] {
  const out: Part[] = [];
  let last = 0;
  for (const m of text.matchAll(HASHRE)) {
    const start = m.index ?? 0;
    const end = start + m[0].length;
    if (isHex(text[start - 1]) || isHex(text[end])) continue;
    let txid: string | undefined;
    if (m[1]) { const id = m[1].toLowerCase(); if (all || known.has(id)) txid = id; }
    else txid = matchTruncated(m[2], m[3], known);
    if (!txid) continue;
    if (start > last) out.push(text.slice(last, start));
    out.push({ txid, label: m[0] });
    last = end;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}
