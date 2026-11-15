#!/usr/bin/env node
/**
 * The behaviour-gate table of the GPUI desktop client (clients/desktop/BEHAVIOUR-GATES.md): every test title of the e2e/media
 * suite (read live) and of the retired web, voice and Electron suites (frozen at desktop-v0.1.0-beta.5 in
 * clients/desktop/behaviour-gates-titles.tsv, `file<TAB>title` rows; those directories are deleted) maps to the Rust test that now holds that behaviour, to "n/a (browser/phone only)"
 * with its reason, to "next plan (native)", or to a "TODO: <proposed test name>" the Task 28 test lane writes.
 * scripts/behaviour-gates.test.mjs (CI: desktop-gpui.yml, `npm run ci:local`) fails on an e2e title missing from the table, on a
 * table row whose title no e2e source has any more, and on a Rust reference whose test function does not exist.
 *
 * `node scripts/behaviour-gates.mjs` prints the e2e titles as `file<TAB>title`, one per line.
 */
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
/** The e2e directories read live; e2e/web, e2e/voice and packages/desktop/test/electron are read from FROZEN_TITLES. */
export const E2E_DIRS = ['e2e/media'];
/** The titles of the retired suites, frozen at desktop-v0.1.0-beta.5: `file<TAB>title` per line. */
export const FROZEN_TITLES = 'clients/desktop/behaviour-gates-titles.tsv';
export const TABLE = 'clients/desktop/BEHAVIOUR-GATES.md';

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name)) : [join(dir, e.name)]));
}

/** The text a JS string literal body stands for (the escapes the e2e titles use). */
export function unescapeTitle(body) {
  return body.replace(/\\(u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2}|[\s\S])/g, (_, c) => {
    if (c[0] === 'u' && c.length === 5) return String.fromCharCode(parseInt(c.slice(1), 16));
    if (c[0] === 'x' && c.length === 3) return String.fromCharCode(parseInt(c.slice(1), 16));
    return { n: '\n', t: '\t' }[c] ?? c;
  });
}

const TEST_CALL = /(?:^|[^\w.$])(?:test|it)(?:\.(?:only|skip|fixme|fail|slow))?\(\s*(['"`])((?:\\[\s\S]|(?!\1)[^\\])*)\1/g;

/**
 * The leaf test titles of one Playwright source: every `test(` / `it(` call (with `.only`, `.skip`, `.fixme`, `.fail`, `.slow`)
 * and its title literal; `test.describe` groups carry no behaviour of their own and are not listed. A title that is not one plain
 * literal (a concatenation, a variable) is an error: it could not be matched against the table.
 */
export function titlesOf(source) {
  const titles = [];
  for (const m of source.matchAll(TEST_CALL)) {
    const after = source.slice(m.index + m[0].length).match(/^\s*(\S)/)?.[1];
    if (after !== ',' && after !== ')') throw new Error(`a test title that is not one string literal: ${m[0].trim()} ...`);
    titles.push(unescapeTitle(m[2]));
  }
  return titles;
}

/** The frozen `[file, title]` rows of the retired suites. */
export function frozenTitles(root = ROOT) {
  return readFileSync(join(root, FROZEN_TITLES), 'utf8').split(/\r?\n/).filter(Boolean).map((line) => {
    const tab = line.indexOf('\t');
    if (tab < 0) throw new Error(`a frozen title row needs file<TAB>title: ${line.slice(0, 120)}`);
    return [line.slice(0, tab), line.slice(tab + 1)];
  });
}

/** Every `[file, title]`: the frozen rows first, then the live e2e suites in file then source order. `file` is repo-relative with `/`. */
export function e2eTitles(root = ROOT) {
  const out = frozenTitles(root);
  for (const dir of E2E_DIRS) {
    for (const path of walk(join(root, dir)).filter((p) => p.endsWith('.spec.ts')).sort()) {
      const file = relative(root, path).split(sep).join('/');
      for (const title of titlesOf(readFileSync(path, 'utf8'))) out.push([file, title]);
    }
  }
  return out;
}

/** A table cell's text for a title: `|` is escaped so it cannot end the cell. */
export const cell = (text) => text.replaceAll('|', '\\|').replace(/\s+/g, ' ');

/**
 * The rows of the table: `| file | title | gate |`, the gate being `;`-separated entries (a Rust reference
 * `crate::path::test_name`, `n/a (browser/phone only): <reason>`, `next plan (native): <reason>` or `TODO: <crate>::<path>::<name>`).
 */
export function tableRows(markdown) {
  const rows = [];
  for (const line of markdown.split(/\r?\n/)) {
    if (!line.startsWith('| `')) continue;
    const parts = line.slice(1, line.endsWith('|') ? -1 : undefined).split(/(?<!\\)\|/).map((s) => s.trim());
    if (parts.length !== 3) throw new Error(`a table row needs three cells (file, title, gate): ${line.slice(0, 120)}`);
    rows.push({ file: parts[0].replace(/^`|`$/g, ''), title: parts[1].replaceAll('\\|', '|'), gate: parts[2] });
  }
  return rows;
}

/** `crate::path::test` references: the crate directory under clients/desktop, the source path without `.rs`, the test fn. */
export function rustRefs(gate) {
  return [...gate.matchAll(/(?<![\w:`-])(dilla-[a-z-]+|parity)::([a-z0-9_/]+)::([a-z0-9_]+)(?![\w:])/g)].map((m) => ({ crate: m[1], path: m[2], name: m[3], text: m[0] }));
}

/** The gate's entries split on `;` (reasons never contain one). */
export const entries = (gate) => gate.split(';').map((s) => s.trim()).filter(Boolean);

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  for (const [file, title] of e2eTitles()) console.log(`${file}\t${title}`);
}
