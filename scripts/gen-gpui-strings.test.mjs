import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import { applyOverrides, entries, rustString, stale } from './gen-gpui-strings.mjs';

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

test('desktop overrides replace the web copy and keep its placeholders', async () => {
  const table = new Map(await entries());
  assert.equal(table.get('boot.error.detail'), 'Error {code}. Reload to try again, or switch to another instance.');
  assert.equal(table.get('search.note'), 'Searches messages held on this device');
  assert.deepEqual(applyOverrides({ a: 'x {n}' }, { a: 'y {n}' }), { a: 'y {n}' });
  assert.throws(() => applyOverrides({ a: 'x' }, { b: 'y' }), /no web key b/);
  assert.throws(() => applyOverrides({ a: 'x {n}' }, { a: 'y {m}' }), /changes its placeholders/);
});

test('no_web_wording_on_the_desktop: no key the desktop reads names the browser, a tab or a page reload', async () => {
  const table = new Map(await entries());
  const desktop = join(resolve(dirname(fileURLToPath(import.meta.url)), '..'), 'clients', 'desktop');
  let src = '';
  const walk = (d) => {
    for (const f of readdirSync(d, { withFileTypes: true })) {
      if (f.name === 'target' || f.name === 'strings.rs') continue;
      const p = join(d, f.name);
      if (f.isDirectory()) walk(p);
      else if (f.name.endsWith('.rs')) src += readFileSync(p, 'utf8');
    }
  };
  walk(desktop);
  // These name the system browser the desktop really opens (passkeys, single sign-on, the web app), or tab characters.
  const allowed = /^(factorSignin\.|factor\.passkey\.body$|factor\.error\.desktopUnsupported$|factorSettings\.stepUp\.passkeyUnsupported$|.*\.(topic|name|reason)Control$|create\.name\.control$)/;
  const web = /\bbrowsers?\b|\btabs?\b|reload the page|\bthis site\b/i;
  const hits = [...table].filter(([k, v]) => !allowed.test(k) && web.test(v.replace(/\{[A-Za-z]+\}/g, "")) && src.includes(`"${k}"`)).map(([k]) => k);
  assert.deepEqual(hits, []);
});
