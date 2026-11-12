import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { entries, rustString, stale } from './gen-gpui-strings.mjs';

const DIR = join(resolve(dirname(fileURLToPath(import.meta.url)), '..'), 'packages', 'web', 'src', 'strings');

test('strings_generator_is_fresh: the committed strings.rs is what the generator writes', async () => {
  assert.equal(await stale(), false, 'stale: run node scripts/gen-gpui-strings.mjs');
});

test('every_ts_key_present: each key written in a strings file is in the generated table', async () => {
  const have = new Set((await entries()).map(([k]) => k));
  const written = new Set();
  for (const f of readdirSync(DIR)) {
    if (!f.endsWith('.ts') || f.endsWith('.test.ts') || f === 'index.ts' || f === 'en.ts') continue;
    // A key is written quoted ('a.b' or "a.b") or bare (save); a file the generator prefixes (settingsEditing.ts) is
    // checked under its prefix.
    const prefix = f === 'settingsEditing.ts' ? 'settings.editing.' : '';
    for (const m of readFileSync(join(DIR, f), 'utf8').matchAll(/^\s*(?:'([^']+)'|"([^"]+)"|([A-Za-z_$][\w$]*))\s*:/gm)) written.add(prefix + (m[1] ?? m[2] ?? m[3]));
  }
  assert.ok(written.size > 1000);
  assert.ok(written.has('settings.editing.keepEditing'), 'bare keys are read');
  const missing = [...written].filter((k) => !have.has(k));
  assert.deepEqual(missing, []);
});

test('rust literals escape what Rust reads differently', () => {
  assert.equal(rustString('a"b\\c\n'), '"a\\"b\\\\c\\n"');
  assert.equal(rustString('it’s {n}'), '"it’s {n}"');
  assert.equal(rustString('\u0001'), '"\\u{1}"');
});
