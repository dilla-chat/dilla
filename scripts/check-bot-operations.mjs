import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Drift guard for the Bot's operations (Bot v2 Task 15). The source of truth is the catalog builder's command table
 * (internal/bot/catalog/catalog.go): every `protocol.Op<Name>` it offers must be an `Op<Name> Operation = <n>`
 * constant in internal/bot/protocol/codec.go, or the catalog would publish an operation number nobody defines.
 *
 * Until Task 21/23 move the app's surfaces onto the catalog and delete utilities.ts, the v1 capability report can
 * still name any v1 operation (numbers below FIRST_SEALED_ONLY), so the client's KNOWN_OPERATIONS
 * (packages/client-core/src/bot/utilities.ts) must still cover those; an operation missing there silently drops its
 * feature. Operations from FIRST_SEALED_ONLY on (OpProbe and the sealed reads) exist only on sealed commands and are
 * read by the client from the catalog.
 */
const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const CODEC = 'internal/bot/protocol/codec.go';
const CATALOG = 'internal/bot/catalog/catalog.go';
const CLIENT = 'packages/client-core/src/bot/utilities.ts';
export const FIRST_SEALED_ONLY = 90;

export function goOperations(source) {
  const ops = new Map();
  for (const m of source.matchAll(/^\s*(Op[A-Za-z0-9]+)\s+Operation\s*=\s*(\d+)\b/gm)) ops.set(Number(m[2]), m[1]);
  return ops;
}

/** The operation names the catalog builder's command table offers, in order of first use. */
export function catalogOperations(source) {
  const start = source.indexOf('var commands = []Command{');
  if (start < 0) throw new Error('the catalog command table (var commands = []Command{) was not found');
  const end = source.indexOf('\n}\n', start);
  const table = source.slice(start, end < 0 ? undefined : end);
  const names = [];
  for (const m of table.matchAll(/protocol\.(Op[A-Za-z0-9]+)\b/g)) if (!names.includes(m[1])) names.push(m[1]);
  if (names.length === 0) throw new Error('the catalog command table names no operation');
  return names;
}

export function clientOperations(source) {
  const m = /const KNOWN_OPERATIONS = new Set\(\[([^\]]*)\]\)/.exec(source);
  if (!m) throw new Error('KNOWN_OPERATIONS set literal not found');
  return new Set(m[1].split(',').map(s => s.trim()).filter(Boolean).map(Number));
}

/** Catalog operations with no codec constant. */
export function undefinedCatalogOperations(goSource, catalogSource) {
  const defined = new Set(goOperations(goSource).values());
  return catalogOperations(catalogSource).filter(name => !defined.has(name));
}

/** v1 operations (below FIRST_SEALED_ONLY) the client does not know. */
export function missingOperations(goSource, clientSource) {
  const go = goOperations(goSource);
  if (go.size === 0) throw new Error('no Op constants found in codec.go');
  const known = clientOperations(clientSource);
  return [...go].filter(([n]) => n < FIRST_SEALED_ONLY && !known.has(n)).sort((a, b) => a[0] - b[0]).map(([n, name]) => `${n} (${name})`);
}

export function check(base = root) {
  const go = readFileSync(join(base, CODEC), 'utf8');
  const problems = [];
  const undefinedOps = undefinedCatalogOperations(go, readFileSync(join(base, CATALOG), 'utf8'));
  if (undefinedOps.length) problems.push(`${CATALOG} offers operations ${CODEC} does not define: ${undefinedOps.join(', ')}`);
  const missing = missingOperations(go, readFileSync(join(base, CLIENT), 'utf8'));
  if (missing.length) problems.push(`${CLIENT} KNOWN_OPERATIONS is missing v1 Bot operations from ${CODEC}: ${missing.join(', ')}`);
  return problems;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const problems = check();
  if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
  console.log('bot operations: the catalog offers only defined operations; the client covers every v1 operation');
}
