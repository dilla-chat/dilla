// Source-level browser checks: real web Conversation/Card wrappers, without a testhost or a dist build.
import { after, before, test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, extname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { build } from 'esbuild';
import { chromium } from 'playwright';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
let browser, server, directory, url;
before(async () => {
  directory = await mkdtemp(join(tmpdir(), 'dilla-conversation-layout-'));
  await build({
    absWorkingDir: root, bundle: true, outfile: join(directory, 'fixture.js'), format: 'esm', jsx: 'automatic',
    loader: { '.woff2': 'file', '.woff': 'file', '.svg': 'dataurl' },
    alias: { '@dilla/media': resolve(root, 'packages/media/src/index.ts') },
    stdin: { resolveDir: root, loader: 'tsx', contents: `
      import React, { useReducer } from 'react';
      import { createRoot } from 'react-dom/client';
      import { CoreProvider } from './packages/web/src/core/context.tsx';
      import { Conversation, conversationReducer, INITIAL_UI } from './packages/web/src/screens/conversation/Conversation.tsx';
      import { nameBook } from './packages/web/src/screens/shell-model.ts';
      import { account, timelineItem } from './packages/web/src/test/fixtures.ts';
      import { PollDialog } from './packages/ui/src/PollDialog/PollDialog.tsx';
      import { POLL_DIALOG_LABELS } from './packages/ui/src/PollDialog/PollDialog.fixture.tsx';
      import { ToastHost, showToast } from './packages/ui/src/Toast/Toast.tsx';
      import './packages/ui/src/styles/base.css';
      import './packages/web/src/app.css';
      const me = account(), channel = 'aa'.repeat(16), original = '01'.repeat(16), answer = '02'.repeat(16);
      const image = { index: 0, size: 1234, mime: 'image/png', name: 'map.png', w: 560, h: 420, thumb: true, kind: 'image', tooLarge: false };
      const file = { index: 1, size: 1234, mime: 'text/plain', name: 'route.txt', w: null, h: null, thumb: false, kind: 'file', tooLarge: false };
      const items = [timelineItem({ key: 's1', msgId: original, seq: '1', body: 'original', attachments: [image, file] }),
        timelineItem({ key: 's2', msgId: answer, seq: '2', body: 'answer', reply: { msgId: original, state: 'ok', senderUser: me.user.id, excerpt: 'original', seq: '1', threadRoot: null } })];
      const canvas = document.createElement('canvas'); canvas.width = 560; canvas.height = 420;
      const thumbnail = await new Promise(resolve => canvas.toBlob(resolve, 'image/png'));
      const client = { get: key => key === 'account' ? me : undefined, subscribe: () => () => {},
        call: async request => request.m === 'openAttachment' ? { blob: thumbnail, name: 'map.png', mime: 'image/png' } : null };
      history.replaceState(null, '', '/dm/' + channel + location.search);
      function App() {
        const [ui, dispatch] = useReducer(conversationReducer, INITIAL_UI);
        const poll = new URLSearchParams(location.search).has('poll');
        return <CoreProvider client={client}><div className="d-root" data-theme="signal">
          <textarea aria-label="composer" />
          <div style={{ height: 500 }}><Conversation channelId={channel} logKey="layout" label="messages" timeline={{ channelId: channel, group: 'active', items, hasEarlier: false }}
            book={nameBook(me.user, [])} encodeWith={[]} broadcast={false} pinsTitle="Pins" ui={ui} dispatch={dispatch} loadingEarlier={false} onLoadEarlier={() => {}} onError={e => { throw e; }} /></div>
          <button onClick={() => showToast('Files over 25 MB can’t be sent from a browser')}>Show toast</button><ToastHost id={1} />
          {poll && <PollDialog labels={POLL_DIALOG_LABELS} open channelName="general" bot={{ name: 'Glenn' }} value={{ question: '', answers: ['', ''], duration: '24h', multi: false }} error={null} canPost={false} onChange={() => {}} onPost={() => {}} onCancel={() => {}} />}
        </div></CoreProvider>;
      }
      // Portals inherit tokens from the body, just as the shipping app does.
      document.body.dataset.theme = 'signal'; document.body.className = 'd-root';
      createRoot(document.getElementById('root')).render(<App />);
    ` },
  });
  server = createServer(async (request, response) => {
    const path = new URL(request.url, 'http://localhost').pathname;
    if (path.endsWith('.js') || path.endsWith('.css') || path.endsWith('.woff2')) {
      response.setHeader('content-type', extname(path) === '.css' ? 'text/css' : extname(path) === '.js' ? 'text/javascript' : 'font/woff2');
      response.end(await readFile(join(directory, path.slice(1))));
    } else response.end('<html><head><link rel="stylesheet" href="/fixture.css"></head><body><div id="root"></div><script type="module" src="/fixture.js"></script></body></html>');
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  url = `http://127.0.0.1:${server.address().port}`;
  browser = await chromium.launch({ headless: true });
});
after(async () => {
  await browser?.close();
  await new Promise(resolve => server ? server.close(resolve) : resolve());
  if (directory) await rm(directory, { recursive: true, force: true });
});

test('web file card stretches to the image height through the real Card wrapper', async () => {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' });
  try {
    await page.goto(url);
    await page.locator('.dw-attachments img').waitFor();
    await page.evaluate(() => document.fonts.ready);
    const geometry = await page.locator('.dw-attachments').evaluate(el => {
      const image = el.querySelector('.d-attachment-card__open').getBoundingClientRect();
      const file = el.querySelector('.d-attachment-card__file').getBoundingClientRect();
      return { imageHeight: image.height, fileHeight: file.height, imageTop: image.top, fileTop: file.top, fileWidth: file.width };
    });
    assert.equal(geometry.imageHeight, 210);
    assert.equal(geometry.fileHeight, 210);
    assert.equal(geometry.fileWidth, 300);
    assert.equal(geometry.fileTop, geometry.imageTop);
  } finally { await page.close(); }
});

test('pointer reply jump suppresses the inherited focus ring and keyboard restores it', async () => {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' });
  try {
    await page.goto(url);
    await page.getByRole('textbox', { name: 'composer' }).fill('draft');
    await page.locator('.d-message-row__reply button').click();
    const row = page.locator('.d-message-row[data-msg-id="' + '01'.repeat(16) + '"]');
    await row.waitFor();
    assert.equal(await row.evaluate(el => document.activeElement === el), true);
    assert.equal(await row.evaluate(el => getComputedStyle(el).outlineStyle), 'none');
    await page.keyboard.press('Shift');
    assert.equal(await row.evaluate(el => getComputedStyle(el).outlineStyle), 'solid');
    await page.locator('.d-message-row__reply button').focus();
    await page.keyboard.press('Enter');
    assert.equal(await row.evaluate(el => document.activeElement === el), true);
    assert.equal(await row.evaluate(el => getComputedStyle(el).outlineStyle), 'solid');
  } finally { await page.close(); }
});

test('poll dialog title begins 10px after the Bot icon on desktop and phone', async () => {
  const page = await browser.newPage({ reducedMotion: 'reduce' });
  try {
    // The fixture keeps the query when the router normalizes its initial path.
    await page.goto(url + '?poll');
    await page.locator('.d-poll-dialog dialog').waitFor();
    await page.evaluate(() => document.fonts.ready);
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 });
      const gap = await page.locator('.d-poll-dialog').evaluate(el => el.querySelector('.d-dialog__title').getBoundingClientRect().left - el.querySelector('.d-poll-dialog__bot').getBoundingClientRect().right);
      assert.equal(gap, 10);
    }
  } finally { await page.close(); }
});

test('poll inputs use an accent border for focus without a second ring', async () => {
  const page = await browser.newPage({ reducedMotion: 'reduce' });
  try {
    await page.goto(url + '?poll');
    const fields = page.locator('.d-poll-dialog input');
    await fields.first().waitFor();
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const field of [fields.nth(0), fields.nth(1)]) {
        await field.focus();
        const focused = await field.evaluate(el => {
          const style = getComputedStyle(el);
          return { border: style.borderColor, shadow: style.boxShadow, outline: style.outlineStyle };
        });
        assert.equal(focused.border, 'rgb(90, 168, 230)');
        assert.equal(focused.shadow, 'none');
        assert.equal(focused.outline, 'none');
        await field.evaluate(el => el.blur());
        assert.notEqual(await field.evaluate(el => getComputedStyle(el).borderColor), focused.border);
      }
    }
  } finally { await page.close(); }
});

test('phone toast uses the viewport width between 16px gutters', async () => {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' });
  try {
    await page.goto(url);
    await page.getByRole('button', { name: 'Show toast' }).click();
    const geometry = await page.locator('.d-np-toast').evaluate(el => {
      const r = el.getBoundingClientRect(), stack = el.parentElement.getBoundingClientRect();
      return { width: r.width, height: r.height, left: stack.left, right: stack.right };
    });
    assert.equal(geometry.left, 16);
    assert.equal(geometry.right, 374);
    assert.ok(geometry.width > 300, JSON.stringify(geometry));
    assert.ok(geometry.height < 65, JSON.stringify(geometry));
  } finally { await page.close(); }
});
