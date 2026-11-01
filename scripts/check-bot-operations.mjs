import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Drift guard: the Bot may report any registered operation in its capability report
 * (cmd/dillabot/utility_runtime.go builds `Supported` from every handler it registers), so every
 * `Op<Name> Operation = <n>` constant in internal/bot/protocol/codec.go is reportable. The client
 * (packages/client-core/src/bot/utilities.ts) keeps KNOWN_OPERATIONS; an operation missing there
 * silently drops its feature, so this fails and names the missing operations.
 */
const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const CODEC = 'internal/bot/protocol/codec.go';
const CLIENT = 'packages/client-core/src/bot/utilities.ts';

export function goOperations(source) {
  const ops = new Map();
  for (const m of source.matchAll(/^\s*(Op[A-Za-z0-9]+)\s+Operation\s*=\s*(\d+)\b/gm)) ops.set(Number(m[2]), m[1]);
  return ops;
}

export function clientOperations(source) {
  const m = /const KNOWN_OPERATIONS = new Set\(\[([^\]]*)\]\)/.exec(source);
  if (!m) throw new Error('KNOWN_OPERATIONS set literal not found');
  return new Set(m[1].split(',').map(s => s.trim()).filter(Boolean).map(Number));
}

export function missingOperations(goSource, clientSource) {
  const go = goOperations(goSource);
  if (go.size === 0) throw new Error('no Op constants found in codec.go');
  const known = clientOperations(clientSource);
  return [...go].filter(([n]) => !known.has(n)).sort((a, b) => a[0] - b[0]).map(([n, name]) => `${n} (${name})`);
}

export function check(base = root) {
  const missing = missingOperations(readFileSync(join(base, CODEC), 'utf8'), readFileSync(join(base, CLIENT), 'utf8'));
  return missing.length === 0 ? [] : [`${CLIENT} KNOWN_OPERATIONS is missing Bot operations from ${CODEC}: ${missing.join(', ')}`];
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const problems = check();
  if (problems.length) { console.error(problems.join('\n')); process.exit(1); }
  console.log('bot operations: client KNOWN_OPERATIONS covers every Bot operation');
}
