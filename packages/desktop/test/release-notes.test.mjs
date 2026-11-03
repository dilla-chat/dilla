import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, writeFile, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { Parser } from 'commonmark';
import { inertTitle, previousDesktopVersion, generateNotes } from '../scripts/release-notes.mjs';

const REPO = '/repos/dilla-chat/dilla';
const SHA = 'a'.repeat(40);
const script = resolve(import.meta.dirname, '../scripts/release-notes.mjs');

/** Parses with the CommonMark reference implementation and returns the paragraph's children. */
function parse(markdown) {
 const document = new Parser().parse(markdown), walker = document.walker(), types = [];
 for (let event; (event = walker.next());) if (event.entering) types.push(event.node.type);
 const paragraph = document.firstChild, children = [];
 for (let node = paragraph?.firstChild; node; node = node.next) children.push(node);
 return { types, paragraph, children };
}
function assertSingleCodeSpan(output, literal) {
 const { types, paragraph, children } = parse(output);
 assert.equal(paragraph?.type, 'paragraph', output);
 assert.equal(children.length, 1, output);
 assert.equal(children[0].type, 'code', output);
 for (const forbidden of ['link', 'image', 'html_inline', 'html_block', 'heading', 'code_block', 'list']) assert.ok(!types.includes(forbidden), `${forbidden} in ${output}`);
 if (literal !== undefined) assert.equal(children[0].literal, literal);
 return children[0].literal;
}

test('inertTitle neutralises links, images, html, fences, headings, mentions, cross-repo references and e-mail autolinks', () => {
 for (const input of ['[x](http://e)', '![i](u)', '<img src=x onerror=1>', '```js', '```', '# h', '@octocat', 'owner/repo#1', 'a@b.co', '&lt;', '<https://e.example>', 'see https://e.example and #12', '&#64;octocat', '* item', '> quote', '| a | b |']) {
  const output = inertTitle(input);
  assert.ok(!output.includes('\n'), input);
  assertSingleCodeSpan(output, input);
 }
});

test('inertTitle handles a title containing backtick runs of every length', () => {
 for (let n = 1; n <= 12; n++) {
  const run = '`'.repeat(n);
  for (const input of [`a${run}b`, `${run}start`, `end${run}`, run, `${run} mid ${'`'.repeat(n + 1)} x`]) {
   const output = inertTitle(input);
   assertSingleCodeSpan(output, input);
  }
 }
});

test('inertTitle flattens newlines and control characters', () => {
 assertSingleCodeSpan(inertTitle('a\nb\r\nc\u0000d\u0007e\tf\u2028g\u202eh\u0085i'), 'a b c d e f g h i');
 assertSingleCodeSpan(inertTitle('  spaced   out\n\n# heading  '), 'spaced out # heading');
 assertSingleCodeSpan(inertTitle(''), ' ');
});

test('inertTitle truncation never splits an entity or a surrogate pair', () => {
 const lone = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
 for (const entity of ['&amp;', '&#64;', '&#x40;', '&lt;']) for (let pad = 190; pad <= 200; pad++) {
  const literal = assertSingleCodeSpan(inertTitle('a'.repeat(pad) + entity + 'b'.repeat(50)));
  assert.ok([...literal].length <= 200, 'max code points');
  assert.ok(literal.endsWith('…'));
  const body = literal.slice(0, -1);
  assert.ok(!/&#?[A-Za-z0-9]*$/.test(body) || body.endsWith(entity), `${pad} ${entity}: ${body.slice(-8)}`);
 }
 const emoji = assertSingleCodeSpan(inertTitle('😀'.repeat(300)));
 assert.equal([...emoji].length, 200);
 assert.ok(!lone.test(emoji));
 assert.equal(assertSingleCodeSpan(inertTitle('x'.repeat(200))), 'x'.repeat(200), 'exactly max is not truncated');
 assert.equal(assertSingleCodeSpan(inertTitle('abcdef', 4)), 'abc…');
});

test('previousDesktopVersion picks the numerically previous release (beta.10 after beta.9)', () => {
 const tags = ['desktop-v0.1.0-beta.9', 'desktop-v0.1.0-beta.10', 'desktop-v0.1.0-beta.2', 'desktop-v0.0.9', 'desktop-beta', 'desktop-v0.1.0-beta.11', 'desktop-v01.0.0', 'v0.1.0', 'desktop-v0.1.0'];
 assert.equal(previousDesktopVersion(tags, '0.1.0-beta.10'), 'desktop-v0.1.0-beta.9');
 assert.equal(previousDesktopVersion(tags, '0.1.0-beta.12'), 'desktop-v0.1.0-beta.11');
 assert.equal(previousDesktopVersion(tags, '0.1.0'), 'desktop-v0.1.0-beta.11', 'a stable release follows its betas');
 assert.equal(previousDesktopVersion(tags, '0.1.1'), 'desktop-v0.1.0');
 assert.equal(previousDesktopVersion(tags, '0.0.9'), null);
 assert.equal(previousDesktopVersion([], '1.0.0'), null);
 assert.throws(() => previousDesktopVersion(tags, '1.0'), /version/i);
});

/** A PR object that throws on any property but the ones the generator may read. */
function poisonedPull(fields) {
 const allowed = new Set(['number', 'title', 'merged_at', 'base']);
 return new Proxy(fields, { get(target, key) {
  if (typeof key === 'symbol') return target[key];
  if (!allowed.has(key)) throw new Error('generateNotes read PR field ' + String(key));
  return target[key];
 } });
}
function notesApi({ commits, pulls }) {
 const calls = [];
 const api = async path => {
  calls.push(path);
  const compare = /^\/repos\/dilla-chat\/dilla\/compare\/(desktop-v[^/]+)\.\.\.([a-f0-9]{40})\?per_page=100&page=(\d+)$/.exec(path);
  if (compare) { const page = Number(compare[3]); return { commits: commits.slice((page - 1) * 100, page * 100).map(sha => ({ sha })) }; }
  const pull = /^\/repos\/dilla-chat\/dilla\/commits\/([a-f0-9]{40})\/pulls\?per_page=100$/.exec(path);
  if (pull) return (pulls[pull[1]] ?? []).map(poisonedPull);
  throw new Error('unexpected ' + path);
 };
 api.calls = calls;
 return api;
}
const commit = n => n.toString(16).padStart(40, '0');
const merged = (number, title, ref = 'main') => ({ number, title, merged_at: '2026-11-01T00:00:00Z', base: { ref }, body: 'SECRET BODY', user: { login: 'x' } });

test('generateNotes lists numbers and titles only and never reads a PR body', async () => {
 const api = notesApi({ commits: [commit(1), commit(2)], pulls: { [commit(1)]: [merged(7, 'Fix [link](http://e) @octocat')], [commit(2)]: [merged(3, 'Add `code` <b>')] } });
 const notes = await generateNotes({ api, version: '0.1.0-beta.2', sha: SHA, previous: 'desktop-v0.1.0-beta.1' });
 assert.equal(notes, `# Dilla desktop 0.1.0-beta.2\n\n- #3 ${inertTitle('Add `code` <b>')}\n- #7 ${inertTitle('Fix [link](http://e) @octocat')}\n`);
 assert.ok(!notes.includes('SECRET'));
 assert.equal(api.calls[0], `${REPO}/compare/desktop-v0.1.0-beta.1...${SHA}?per_page=100&page=1`);
});

test('generateNotes skips unmerged PRs and PRs not targeting main', async () => {
 const api = notesApi({ commits: [commit(1)], pulls: { [commit(1)]: [{ ...merged(1, 'open'), merged_at: null }, merged(2, 'to branch', 'feature'), merged(4, 'kept')] } });
 const notes = await generateNotes({ api, version: '1.0.0', sha: SHA, previous: 'desktop-v0.9.0' });
 assert.equal(notes, `# Dilla desktop 1.0.0\n\n- #4 ${inertTitle('kept')}\n`);
});

test('generateNotes dedupes a PR across commits', async () => {
 const api = notesApi({ commits: [commit(1), commit(2), commit(3)], pulls: { [commit(1)]: [merged(5, 'same')], [commit(2)]: [merged(5, 'same')], [commit(3)]: [merged(6, 'other'), merged(5, 'same')] } });
 const notes = await generateNotes({ api, version: '1.0.0', sha: SHA, previous: 'desktop-v0.9.0' });
 assert.equal(notes.match(/^- #5 /gm).length, 1);
 assert.equal(notes.split('\n').filter(line => line.startsWith('- ')).length, 2);
});

test('generateNotes caps at 250', async () => {
 const commits = Array.from({ length: 260 }, (_, i) => commit(i + 1));
 const pulls = Object.fromEntries(commits.map((sha, i) => [sha, [merged(i + 1, `pr ${i + 1}`)]]));
 const api = notesApi({ commits, pulls });
 const notes = await generateNotes({ api, version: '1.0.0', sha: SHA, previous: 'desktop-v0.9.0' });
 const lines = notes.split('\n').filter(line => line.startsWith('- '));
 assert.equal(lines.length, 251);
 assert.equal(lines.at(-1), '- and 10 more merged pull requests');
 assert.ok(api.calls.includes(`${REPO}/compare/desktop-v0.9.0...${SHA}?per_page=100&page=3`), 'compare is paged');
});

test('generateNotes for a first release names no PRs', async () => {
 const api = notesApi({ commits: [commit(1)], pulls: { [commit(1)]: [merged(1, 'x')] } });
 assert.equal(await generateNotes({ api, version: '0.1.0-beta.1', sha: SHA, previous: null }), '# Dilla desktop 0.1.0-beta.1\n\nFirst desktop release.\n');
 assert.equal(api.calls.length, 0);
 await assert.rejects(generateNotes({ api, version: '0.1.0-beta.1', sha: SHA, previous: 'desktop-v1; rm' }), /previous/i);
 await assert.rejects(generateNotes({ api, version: '0.1.0-beta.1', sha: 'HEAD', previous: null }), /sha/i);
});

test('the CLI refuses to overwrite its output', async () => {
 const directory = await mkdtemp(join(tmpdir(), 'dilla-notes-'));
 try {
  const output = join(directory, 'release-notes.md');
  await writeFile(output, 'existing');
  const result = spawnSync(process.execPath, [script, 'generate', '0.1.0-beta.1', SHA, output], { env: { PATH: process.env.PATH, GH_TOKEN: 'unused-token' }, encoding: 'utf8', timeout: 30000 });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /EEXIST/);
  assert.equal(await readFile(output, 'utf8'), 'existing');
  const usage = spawnSync(process.execPath, [script, 'generate', '0.1.0-beta.1', SHA], { env: { PATH: process.env.PATH }, encoding: 'utf8', timeout: 30000 });
  assert.notEqual(usage.status, 0);
 } finally { await rm(directory, { recursive: true, force: true }); }
});

test('rendered notes contain no anchor except #n', { skip: process.env.GH_TOKEN ? false : 'needs GH_TOKEN (run once by hand)' }, async () => {
 const hostile = ['[x](http://e)', '![i](https://e.example/i.png)', '<a href="http://e">a</a>', '<img src=x onerror=1>', '@octocat', 'owner/repo#1', 'dilla-chat/dilla#1', 'a@b.co', '&#64;octocat', 'https://e.example', 'www.e.example', 'GH-1', '#1', '`code` ``', ':smile:', 'abc1234def5678'];
 const text = '# Dilla desktop 1.0.0\n\n' + hostile.map((title, i) => `- #${i + 1} ${inertTitle(title)}\n`).join('');
 const response = await fetch('https://api.github.com/markdown', { method: 'POST', redirect: 'error', headers: { Authorization: `Bearer ${process.env.GH_TOKEN}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' }, body: JSON.stringify({ text, mode: 'gfm', context: 'dilla-chat/dilla' }) });
 assert.equal(response.status, 200);
 const html = await response.text();
 assert.ok(!/<img/i.test(html));
 // The fixed "# Dilla desktop" heading gets GitHub's own heading anchor; every title line is checked.
 const list = html.slice(html.search(/<ul\b/i));
 assert.ok(list.length > 0 && (list.match(/<li>/gi) ?? []).length === hostile.length);
 for (const [anchor, inner] of list.matchAll(/<a\b[^>]*>([\s\S]*?)<\/a>/gi)) assert.match(inner, /^#\d+$/, anchor);
 for (const item of html.matchAll(/<li>([\s\S]*?)<\/li>/gi)) assert.match(item[1], /^(?:<a\b[^>]*>#\d+<\/a>|#\d+) <code>[\s\S]*<\/code>$/, item[1]);
});
