// Dumps every gateway op that packages/client-core dispatches on, as JSON: [{ name, op }], sorted by op.
//   name  the `Op` key (gateway/frames.ts), e.g. "voiceState"
//   op    its number
// "Dispatches on" is a comparison `.op === Op.<name>` in worker/controller.ts or in a sync/*.ts module (tests and
// sync/testing excluded): the frames the TS client consumes. Read as text, nothing is executed.
// The Rust test `route_table_covers_ts_ops` (dilla-client src/sync/route.rs) runs this and checks that every op is
// either consumed by the sync engine or routed to exactly one area.
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..', '..');
const src = join(root, 'packages/client-core/src');

const frames = readFileSync(join(src, 'gateway/frames.ts'), 'utf8');
const table = /export const Op = \{([\s\S]*?)\} as const;/.exec(frames);
if (!table) throw new Error('gateway/frames.ts: no `export const Op = { ... } as const`');
const ops = new Map();
for (const [, name, value] of table[1].matchAll(/(\w+):\s*(\d+)/g)) ops.set(name, Number(value));
if (ops.size < 20) throw new Error(`gateway/frames.ts: only ${ops.size} ops read`);

const files = [join(src, 'worker/controller.ts')];
for (const f of readdirSync(join(src, 'sync'))) {
  if (f.endsWith('.ts') && !f.endsWith('.test.ts')) files.push(join(src, 'sync', f));
}
const used = new Set();
for (const file of files) {
  for (const [, name] of readFileSync(file, 'utf8').matchAll(/\.op\s*===\s*Op\.(\w+)/g)) {
    if (!ops.has(name)) throw new Error(`${file}: Op.${name} is not in gateway/frames.ts`);
    used.add(name);
  }
}
const out = [...used].map((name) => ({ name, op: ops.get(name) })).sort((a, b) => a.op - b.op);
process.stdout.write(`${JSON.stringify(out)}\n`);
