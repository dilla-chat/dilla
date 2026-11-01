import test from 'node:test';
import assert from 'node:assert/strict';
import { validateSpec, storyUrl, patchDataProps, DEFAULT_GLOBALS, padClip, localFontCss } from './shoot.mjs';

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
