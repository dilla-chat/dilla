// CI test behaviour_gate_table_complete (Task 28 step 2 of the GPUI desktop screens wave 1): clients/desktop/BEHAVIOUR-GATES.md
// has a row for every e2e test title, and says for each one where the behaviour is held now.
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { ROOT, TABLE, cell, e2eTitles, entries, rustRefs, tableRows, titlesOf, unescapeTitle } from './behaviour-gates.mjs';

const rows = tableRows(readFileSync(join(ROOT, TABLE), 'utf8'));
const key = (file, title) => `${file}\u0000${cell(title)}`;

test('behaviour_gate_table_complete: every e2e test title has a row', () => {
  const have = new Set(rows.map((r) => key(r.file, r.title)));
  const missing = e2eTitles().filter(([file, title]) => !have.has(key(file, title)));
  assert.deepEqual(missing.map(([file, title]) => `${file}: ${title}`), [], `unmapped e2e titles: add a row to ${TABLE}`);
});

test('behaviour_gate_table_complete: no row for a title the e2e sources no longer have, and no row twice', () => {
  const have = new Set(e2eTitles().map(([file, title]) => key(file, title)));
  const seen = new Set();
  for (const r of rows) {
    const k = key(r.file, r.title);
    assert.ok(have.has(k), `${r.file}: no e2e test titled "${r.title}" any more; drop or rename its row`);
    assert.ok(!seen.has(k), `${r.file}: "${r.title}" has two rows`);
    seen.add(k);
  }
});

test('behaviour_gate_table_complete: every row says where the behaviour is held', () => {
  for (const r of rows) {
    const gate = entries(r.gate);
    assert.ok(gate.length > 0, `${r.file}: "${r.title}" has an empty gate`);
    for (const e of gate) {
      const ok = /^n\/a \(browser\/phone only\): \S/.test(e) || /^next plan \(native\): \S/.test(e) || /^TODO: dilla-[a-z-]+::[a-z0-9_/]+::[a-z0-9_]+$/.test(e) || /^dilla-[a-z-]+::[a-z0-9_/]+::[a-z0-9_]+$/.test(e);
      assert.ok(ok, `${r.file}: "${r.title}": "${e}" is not a Rust test (crate::path::name), "n/a (browser/phone only): reason", "next plan (native): reason" or "TODO: crate::path::name"`);
    }
  }
});

test('behaviour_gate_table_complete: every Rust reference is a real test function', () => {
  const missing = [];
  for (const r of rows) {
    for (const e of entries(r.gate).filter((x) => !x.startsWith('TODO'))) {
      for (const ref of rustRefs(e)) {
        const path = join(ROOT, 'clients/desktop', ref.crate, `${ref.path}.rs`);
        const source = existsSync(path) ? readFileSync(path, 'utf8') : '';
        // A test is a `fn name` right under a `#[test]`/`#[tokio::test]`/`#[gpui::test]` attribute (other attributes may sit between).
        const found = new RegExp(`#\\[(?:tokio::|gpui::)?test[^\\]]*\\](?:\\s*#\\[[^\\]]*\\])*\\s*(?:pub\\s+)?(?:async\\s+)?fn ${ref.name}\\b`).test(source);
        if (!found) missing.push(`${r.file}: "${r.title}": ${ref.text}`);
      }
    }
  }
  assert.deepEqual(missing, [], 'Rust references that name no test function');
});

test('the e2e title parser reads literals, escapes and modifiers, and refuses a title it cannot read', () => {
  assert.deepEqual(titlesOf("test('a', async () => {});\ntest.skip(\n  \"b \\\"q\\\"\", () => {});\ntest.describe('group', () => { it(`c ${x}`, f); });\nawait expect(test).toBe(1);\nmytest('no', f);"), ['a', 'b "q"', 'c ${x}']);
  assert.equal(unescapeTitle('it\\u2019s a\\\'b'), 'it’s a\'b');
  assert.throws(() => titlesOf("test('a' + 'b', f);"), /not one string literal/);
});
