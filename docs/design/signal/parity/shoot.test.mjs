import test from 'node:test';
import assert from 'node:assert/strict';
import { validateSpec, storyUrl, patchDataProps, DEFAULT_GLOBALS, padClip, localFontCss, appUrl, sideOf, pairFile, parseArgs, expandText, orderPairs, uploadFixture } from './shoot.mjs';

const good = { name: 'a', viewport: { width: 1280, height: 800 }, prototype: {}, story: { id: 'x--y' } };

test('validateSpec accepts a minimal pair', () => {
  assert.equal(validateSpec([good]).length, 1);
});

test('validateSpec rejects a pair without name or story.id', () => {
  assert.throws(() => validateSpec([{ ...good, name: undefined }]), /name is required/);
  assert.throws(() => validateSpec([{ ...good, story: {} }]), /story\.id is required/);
});

test('validateSpec rejects empty specs, path-like names, duplicates, bad viewports and bad steps', () => {
  assert.throws(() => validateSpec([]), /non-empty/);
  assert.throws(() => validateSpec([{ ...good, name: '../x' }]), /file stem/);
  assert.throws(() => validateSpec([good, good]), /duplicate/);
  assert.throws(() => validateSpec([{ ...good, viewport: { width: 0, height: 1 } }]), /viewport/);
  assert.throws(() => validateSpec([{ ...good, prototype: { steps: [{ click: 'a', hover: 'b' }] } }]), /exactly one/);
  assert.throws(() => validateSpec([{ ...good, prototype: { steps: [{ type: ['a'] }] } }]), /\[selector, text\]/);
});

test('storyUrl builds the iframe URL with the default globals encoded', () => {
  const u = new URL(storyUrl('http://127.0.0.1:6006', { id: 'shell-appshell--shell' }));
  assert.equal(u.pathname, '/iframe.html');
  assert.equal(u.searchParams.get('id'), 'shell-appshell--shell');
  assert.equal(u.searchParams.get('viewMode'), 'story');
  assert.equal(u.searchParams.get('globals'), DEFAULT_GLOBALS);
  assert.match(storyUrl('http://127.0.0.1:6006', { id: 'a' }), /globals=theme%3Asignal%3Bmotion%3Areduced/);
});

test('storyUrl encodes custom globals and ids', () => {
  const href = storyUrl('http://127.0.0.1:6006/', { id: 'a b', globals: 'theme:light;density:cozy' });
  assert.match(href, /id=a\+b/);
  assert.match(href, /globals=theme%3Alight%3Bdensity%3Acozy/);
});

test('patchDataProps sets defaults on known props and adds unknown ones, keeping the rest of the metadata', () => {
  const raw = JSON.stringify({ startAt: { editor: 'enum', default: 'signin' }, device: { editor: 'enum', default: 'auto' } });
  const out = JSON.parse(patchDataProps(raw, { startAt: 'app', extra: 1 }));
  assert.deepEqual(out.startAt, { editor: 'enum', default: 'app' });
  assert.deepEqual(out.device, { editor: 'enum', default: 'auto' });
  assert.deepEqual(out.extra, { default: 1 });
});

test('patchDataProps tolerates missing or broken metadata', () => {
  assert.deepEqual(JSON.parse(patchDataProps(null, { a: 1 })), { a: { default: 1 } });
  assert.deepEqual(JSON.parse(patchDataProps('{nope', { a: 1 })), { a: { default: 1 } });
});

test('padClip grows a box by the pad and stays on the page', () => {
  assert.deepEqual(padClip({ x: 20, y: 30, width: 10, height: 12 }, 8), { x: 12, y: 22, width: 26, height: 28 });
  assert.deepEqual(padClip({ x: 3, y: 0, width: 10, height: 12 }, 8), { x: 0, y: 0, width: 21, height: 20 });
});

test('localFontCss declares JetBrains Mono 400-700 and DM Serif Display italic from local files', () => {
  const seen = [];
  const css = localFontCss(f => { seen.push(f); return 'QQ=='; });
  assert.match(css, /font-family:'JetBrains Mono';font-style:normal;font-weight:400 700/);
  assert.match(css, /font-family:'DM Serif Display';font-style:italic;font-weight:400/);
  assert.ok(seen.some(f => f.includes('jetbrains-mono')) && seen.some(f => f.includes('dm-serif-display')));
  assert.doesNotMatch(css, /https?:/);
});

const app = { name: 'b', viewport: { width: 1280, height: 800 }, prototype: {}, app: { who: 'ada' } };

test('validateSpec accepts a pair with app and no story, and rejects both or neither', () => {
  assert.equal(validateSpec([app]).length, 1);
  assert.equal(validateSpec([app, good.name === 'a' ? { ...good, name: 'c' } : good]).length, 2);
  assert.throws(() => validateSpec([{ ...app, story: { id: 'x--y' } }]), /exactly one of story or app/);
  const { story, ...neither } = good;
  assert.throws(() => validateSpec([neither]), /exactly one of story or app/);
});

test('validateSpec rejects an unknown who and accepts every known one', () => {
  assert.throws(() => validateSpec([{ ...app, app: { who: 'liv' } }]), /who must be one of/);
  assert.throws(() => validateSpec([{ ...app, app: {} }]), /who must be one of/);
  for (const who of ['ada', 'bjorn', 'mira', 'phone', 'fresh', 'freshPhone']) assert.equal(validateSpec([{ ...app, app: { who } }]).length, 1);
});

test('validateSpec accepts goto and totp steps and rejects a malformed totp or goto', () => {
  const steps = [{ goto: '/welcome' }, { totp: ['input', 'ada'] }, { click: 'button' }];
  assert.equal(validateSpec([{ ...app, app: { who: 'fresh', path: '/x', steps, clip: '.a', reset: '/y' } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ totp: ['input'] }] } }]), /totp is \[selector, who\]/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ totp: ['input', 'zed'] }] } }]), /totp is \[selector, who\]/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'fresh', steps: [{ goto: 3 }] } }]), /goto is a path/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', path: 3 } }]), /app\.path/);
});

test('appUrl joins the public URL and expands {invite} from state.invitePath', () => {
  const state = { publicUrl: 'http://127.0.0.1:8611', invitePath: '/i/abc%20d' };
  assert.equal(appUrl(state, '/c/x?i={invite}'), 'http://127.0.0.1:8611/c/x?i=/i/abc%20d');
  assert.equal(appUrl(state, '{invite}'), 'http://127.0.0.1:8611/i/abc%20d');
  assert.equal(appUrl(state, '/'), 'http://127.0.0.1:8611/');
  assert.throws(() => appUrl({ publicUrl: 'http://x' }, '{invite}'), /invitePath/);
  assert.equal(appUrl({ ...state, serverPath: '/c/a/b' }, '{server}'), 'http://127.0.0.1:8611/c/a/b');
  assert.throws(() => appUrl(state, '{server}'), /serverPath/);
  assert.equal(appUrl({ ...state, serverPath: '/c/a/b' }, '{community}'), 'http://127.0.0.1:8611/c/a');
});

test('output file stems are .app.png for app pairs and .story.png for story pairs', () => {
  assert.equal(sideOf(app), 'app');
  assert.equal(sideOf(good), 'story');
  assert.equal(pairFile('out', app, sideOf(app)), 'out/b.app.png');
  assert.equal(pairFile('out', good, 'prototype'), 'out/a.prototype.png');
  assert.equal(pairFile('out', good, 'pair'), 'out/a.pair.png');
});

test('parseArgs reads --storybook and --app with an optional state file', () => {
  assert.deepEqual(parseArgs(['s.json', 'out']), { specFile: 's.json', outDir: 'out', storybook: 'http://127.0.0.1:6006', app: null });
  assert.equal(parseArgs(['s.json', 'out', '--app']).app, 'target/ui-screens/signal-b/state.json');
  assert.equal(parseArgs(['s.json', 'out', '--app', 'x/state.json']).app, 'x/state.json');
  assert.equal(parseArgs(['s.json', 'out', '--storybook', 'http://h:1']).storybook, 'http://h:1');
});

test('expandText fills {password:who} and {recoveryKey:who} and leaves other text alone', () => {
  const state = { people: { mira: { password: 'pw-mira', recoveryKey: 'AAAA BBBB' } } };
  assert.equal(expandText(state, 'x {password:mira} y'), 'x pw-mira y');
  assert.equal(expandText(state, '{recoveryKey:mira}'), 'AAAA BBBB');
  assert.equal(expandText(state, 'plain {other}'), 'plain {other}');
});

test('expandText throws on a missing key and does not echo any value', () => {
  const state = { people: { mira: { password: 'pw-mira-secret' } } };
  assert.throws(() => expandText(state, '{recoveryKey:mira}'), err => /recoveryKey for mira/.test(err.message) && !/pw-mira-secret/.test(err.message));
  assert.throws(() => expandText(state, '{password:bjorn}'), /password for bjorn/);
  assert.throws(() => expandText({}, '{password:mira}'), /password for mira/);
});

test('validateSpec accepts a dispatch step and a mutates flag, and rejects a non-boolean mutates', () => {
  assert.equal(validateSpec([{ ...app, mutates: true, app: { who: 'ada', steps: [{ dispatch: '.d-code-block__copy' }] } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, mutates: 'yes' }]), /mutates must be true or false/);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', steps: [{ dispatch: '.a', click: '.b' }] } }]), /exactly one/);
});

test('orderPairs runs mutating pairs last and keeps the order inside each group', () => {
  const p = (name, mutates) => ({ name, ...(mutates ? { mutates } : {}) });
  assert.deepEqual(orderPairs([p('a', true), p('b'), p('c', true), p('d')]).map(x => x.name), ['b', 'd', 'a', 'c']);
  assert.deepEqual(orderPairs([p('a'), p('b')]).map(x => x.name), ['a', 'b']);
});

test('validateSpec accepts an upload step with files and rejects one without', () => {
  assert.equal(validateSpec([{ ...app, app: { who: 'ada', steps: [{ upload: ['input[type=file]', 'a.png', 'b.txt'] }] } }]).length, 1);
  assert.throws(() => validateSpec([{ ...app, app: { who: 'ada', steps: [{ upload: ['input[type=file]'] }] } }]), /upload is \[selector, file/);
});

// Wave B task 3b: a step that reads the setup key a new account is shown and fills its code.
test('validateSpec takes a totpFrom step of two selectors and refuses any other shape', () => {
  const pair = { name: 'p', viewport: { width: 1280, height: 800 }, prototype: {}, app: { who: 'fresh', steps: [{ totpFrom: ['#factor-totp-key', '#factor-totp-code'] }] } };
  validateSpec([pair]);
  assert.throws(() => validateSpec([{ ...pair, app: { who: 'fresh', steps: [{ totpFrom: ['#k'] }] } }]), /totpFrom is \[setup key selector, code selector\]/);
  assert.throws(() => validateSpec([{ ...pair, app: { who: 'fresh', steps: [{ totpFrom: ['#k', ''] }] } }]), /totpFrom/);
});

test('Task 8 generated upload fixtures preserve exact cap sizes without committed binaries', () => {
  const file = uploadFixture('@bytes/26214401/huge.bin');
  assert.equal(file.name, 'huge.bin');
  assert.equal(file.buffer.length, 25 * 1024 * 1024 + 1);
  assert.equal(file.mimeType, 'application/octet-stream');
  assert.throws(() => uploadFixture('@bytes/0/empty'), /fixture/);
});

test('Task 8 manifest covers both viewports, conversation findings and standing screens', async () => {
  const { readFileSync } = await import('node:fs');
  const pairs = validateSpec(JSON.parse(readFileSync(new URL('./app/task-8.json', import.meta.url), 'utf8')));
  for (const state of ['threads-direct', 'over-25-mb-toast', 'waiting-toast', 'reaction-tooltip']) {
    for (const size of ['desktop', 'phone']) assert.ok(pairs.some(pair => pair.name === `task8-${state}-${size}`));
  }
  for (const gap of ['gap44', 'gap47', 'gap49', 'gap52', 'gap53', 'gap89', 'gap90', 'gap97']) {
    for (const width of [390, 1280]) assert.ok(pairs.some(pair => pair.name.startsWith(gap) && pair.viewport.width === width));
  }
  assert.equal(pairs.filter(pair => pair.name.startsWith('standing-')).length, 5);
});

test('rejects a null app before reading its upload options and names the pair', () => {
  assert.throws(() => validateSpec([{ name: 'null-app', viewport: { width: 390, height: 844 }, prototype: {}, app: null }]), /null-app.*app.who/);
});
