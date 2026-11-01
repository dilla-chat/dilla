import { test } from 'node:test';
import assert from 'node:assert/strict';
import fsp from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { THEMES, VIEWPORT, selectStories, storyUrl, outFile, contentType, resolveInRoot, resolveExistingInRoot, parseViewport, splitViewportArg, PHONE_VIEWPORT, declaredViewport, shootsAt } from './shoot-stories.mjs';

const index = {
  v: 5,
  entries: {
    'form-textfield--empty': { type: 'story', id: 'form-textfield--empty', title: 'Form/TextField', name: 'Empty' },
    'form-textfield--docs': { type: 'docs', id: 'form-textfield--docs', title: 'Form/TextField', name: 'Docs' },
    'ceremony-splash--loading': { type: 'story', id: 'ceremony-splash--loading', title: 'Ceremony/Splash', name: 'Loading' },
    'primitives-button--default': { type: 'story', id: 'primitives-button--default', title: 'Primitives/Button', name: 'Default' },
  },
};

test('selects the stories whose id starts with a prefix, sorted, and never a docs entry', () => {
  assert.deepEqual(selectStories(index, ['form-', 'ceremony-']), { ids: ['ceremony-splash--loading', 'form-textfield--empty'], unmatched: [] });
});

test('names every prefix that matched no story', () => {
  assert.deepEqual(selectStories(index, ['form-', 'shell-']), { ids: ['form-textfield--empty'], unmatched: ['shell-'] });
});

test('builds the iframe URL with the theme global and the story view mode', () => {
  assert.equal(storyUrl(6016, 'form-textfield--empty', 'high-contrast'),
    'http://127.0.0.1:6016/iframe.html?id=form-textfield--empty&globals=theme:high-contrast&viewMode=story');
});

test('shoots three themes at 1280 by 800', () => {
  assert.deepEqual(THEMES, ['signal', 'light', 'high-contrast']);
  assert.deepEqual(VIEWPORT, { width: 1280, height: 800 });
});

test('names each file <story id>.<theme>.png inside the out dir', () => {
  assert.equal(outFile('/tmp/dw/shots', 'form-textfield--empty', 'light'), '/tmp/dw/shots/form-textfield--empty.light.png');
});

test('serves the Storybook files with their media types', () => {
  assert.equal(contentType('/iframe.html'), 'text/html; charset=utf-8');
  assert.equal(contentType('/assets/iframe-abc.js'), 'text/javascript; charset=utf-8');
  assert.equal(contentType('/assets/x.mjs'), 'text/javascript; charset=utf-8');
  assert.equal(contentType('/assets/x.css'), 'text/css; charset=utf-8');
  assert.equal(contentType('/index.json'), 'application/json');
  assert.equal(contentType('/favicon.svg'), 'image/svg+xml');
  assert.equal(contentType('/a.woff2'), 'font/woff2');
  assert.equal(contentType('/a.png'), 'image/png');
  assert.equal(contentType('/a.unknown'), 'application/octet-stream');
});

test('maps / to index.html and refuses a path that leaves the root', () => {
  assert.equal(resolveInRoot('/srv/sb', '/'), '/srv/sb/index.html');
  assert.equal(resolveInRoot('/srv/sb', '/assets/a.js'), '/srv/sb/assets/a.js');
  assert.equal(resolveInRoot('/srv/sb', '/../etc/passwd'), null);
  assert.equal(resolveInRoot('/srv/sb', '/%2e%2e/etc/passwd'), null);
  assert.equal(resolveInRoot('/srv/sb', '/%E0%A4%A'), null);
});

test('refuses a symlink from Storybook output to a file outside the root', async () => {
  const temp = await fsp.mkdtemp(path.join(os.tmpdir(), 'dilla-story-root-'));
  try {
    const root = path.join(temp, 'storybook-static');
    await fsp.mkdir(root);
    const outside = path.join(temp, 'outside.txt');
    await fsp.writeFile(outside, 'outside');
    await fsp.symlink(outside, path.join(root, 'linked.txt'));
    await fsp.writeFile(path.join(root, 'safe.txt'), 'safe');
    assert.equal(await resolveExistingInRoot(root, '/linked.txt'), null);
    assert.equal(await resolveExistingInRoot(root, '/safe.txt'), path.join(root, 'safe.txt'));
  } finally {
    await fsp.rm(temp, { recursive: true, force: true });
  }
});

test('the final CLI argument selects an actual 360 by 740 context', () => {
  assert.deepEqual(parseViewport(undefined), { width: 1280, height: 800 });
  assert.deepEqual(parseViewport('--viewport=360x740'), { width: 360, height: 740 });
  assert.throws(() => parseViewport('--viewport=0x740'), /viewport/);
  assert.throws(() => parseViewport('--viewport=360x0'), /viewport/);
  assert.deepEqual(splitViewportArg(['trust-safetycompare--', '--viewport=360x740']), { prefixes: ['trust-safetycompare--'], viewport: { width: 360, height: 740 } });
  assert.throws(() => splitViewportArg(['--viewport=360x740', 'trust-safetycompare--']), /final/);
});

test('a story declaring phoneViewport is shot only at 360 by 740, as the test runner visits it', () => {
  assert.deepEqual(PHONE_VIEWPORT, { width: 360, height: 740 });
  assert.deepEqual(declaredViewport({ phoneViewport: true }), { width: 360, height: 740 });
  assert.equal(declaredViewport({}), null);
  assert.equal(declaredViewport(undefined), null);
  assert.equal(declaredViewport({ phoneViewport: 'yes' }), null);
  assert.equal(shootsAt({ phoneViewport: true }, VIEWPORT), false);
  assert.equal(shootsAt({ phoneViewport: true }, { width: 360, height: 740 }), true);
  assert.equal(shootsAt({ phoneViewport: true }, { width: 360, height: 800 }), false);
  assert.equal(shootsAt({}, VIEWPORT), true);
  assert.equal(shootsAt(undefined, { width: 360, height: 740 }), true);
});
