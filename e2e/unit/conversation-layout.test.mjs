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
import { checkPollFieldFocus } from '../../packages/ui/scripts/story-a11y.ts';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
let browser, server, directory, url;
before(async () => {
  directory = await mkdtemp(join(tmpdir(), 'dilla-conversation-layout-'));
  await build({
    absWorkingDir: root, bundle: true, outfile: join(directory, 'fixture.js'), format: 'esm', jsx: 'automatic',
    loader: { '.woff2': 'file', '.woff': 'file', '.svg': 'dataurl' },
    alias: { '@dilla/media': resolve(root, 'packages/media/src/index.ts') },
    stdin: { resolveDir: root, loader: 'tsx', contents: `
      import React, { useReducer, useState } from 'react';
      import { createRoot } from 'react-dom/client';
      import { CoreProvider } from './packages/web/src/core/context.tsx';
      import { Conversation, conversationReducer, INITIAL_UI } from './packages/web/src/screens/conversation/Conversation.tsx';
      import { nameBook } from './packages/web/src/screens/shell-model.ts';
      import { account, timelineItem } from './packages/web/src/test/fixtures.ts';
      import { PhoneLayoutProvider } from './packages/ui/src/internal/phoneLayout.ts';
      import { PollDialog } from './packages/ui/src/PollDialog/PollDialog.tsx';
      import { Dialog } from './packages/ui/src/Dialog/Dialog.tsx';
      import { Button } from './packages/ui/src/Button/Button.tsx';
      import { Icon } from './packages/ui/src/Icon/Icon.tsx';
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
        const confirm = new URLSearchParams(location.search).has('confirm');
        const [pollDraft, setPollDraft] = useState({ question: '', answers: [new URLSearchParams(location.search).has('overlong-answer') ? 'x'.repeat(101) : '', ''], duration: '24h', multi: false });
        const phoneReveal = new URLSearchParams(location.search).has('phone-reveal');
        const longReply = new URLSearchParams(location.search).get('long-reply');
        const phone = phoneReveal || longReply === 'phone';
        const historyRows = Array.from({ length: 45 }, (_, index) => timelineItem({
          key: 'long' + index, msgId: (index + 100).toString(16).padStart(32, '0'), seq: String(index + 100), ts: index * 600,
          senderUser: me.user.id, body: 'Older history row ' + index + ': enough content to overflow the actual message log and keep the reply far below its original.',
        }));
        const shownItems = longReply ? [
          ...historyRows.slice(0, 10), timelineItem({ ...items[0], attachments: [] }), ...historyRows.slice(10), items[1],
        ] : phoneReveal ? Array.from({ length: 30 }, (_, index) => timelineItem({
          key: 'h' + index, msgId: (index + 10).toString(16).padStart(32, '0'), seq: String(index + 10), ts: index * 600,
          senderUser: me.user.id, body: 'history row ' + index + ': keep actions visible after this tap',
        })) : items;
        return <CoreProvider client={client}><div className="d-root" data-theme="signal">
          <textarea aria-label="composer" />
          <PhoneLayoutProvider value={phone}><div data-layout={phone ? 'phone' : undefined} style={{ height: phone ? 360 : 500 }}><Conversation channelId={channel} logKey="layout" label="messages" timeline={{ channelId: channel, group: 'active', items: shownItems, hasEarlier: false }}
            book={nameBook(me.user, [])} encodeWith={[]} broadcast={false} pinsTitle="Pins" ui={ui} dispatch={dispatch} loadingEarlier={false} onLoadEarlier={() => {}} onError={e => { throw e; }} /></div></PhoneLayoutProvider>
          <button onClick={() => showToast('Files over 25 MB can’t be sent from a browser')}>Show toast</button>
          <button onClick={() => showToast('Waiting for uploads to finish')}>Show short toast</button><ToastHost id={1} />
          {poll && <PollDialog labels={POLL_DIALOG_LABELS} open channelName="general" bot={{ name: 'Glenn' }} value={pollDraft} error={null} canPost={false} onChange={setPollDraft} onPost={() => {}} onCancel={() => {}} />}
          {confirm && <Dialog open kind="confirm" title="Confirm moderation" closeLabel="Cancel" onClose={() => {}} footer={<><Button variant="danger"><Icon name="user-x" size={14} />Kick</Button><Button variant="danger"><Icon name="ban" size={14} />Ban</Button></>} />}
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

test('native confirmation Kick, Ban and Cancel retain the prototype widths and 34px height on desktop and phone', async () => {
  const page = await browser.newPage({ reducedMotion: 'reduce' });
  try {
    await page.goto(url + '?confirm');
    await page.locator('dialog[open]').waitFor();
    await page.evaluate(() => document.fonts.ready);
    for (const width of [1280, 390]) {
      await page.setViewportSize({ width, height: 844 });
      const buttons = await page.locator('.d-dialog__footer .d-btn').evaluateAll(buttons => buttons.map(button => {
        const label = button.querySelector('.d-btn__label');
        const text = document.createRange();
        text.selectNode(label.lastChild);
        const word = text.getBoundingClientRect();
        const icon = label.querySelector('svg')?.getBoundingClientRect();
        const rect = button.getBoundingClientRect();
        return { name: button.textContent, width: rect.width, height: rect.height, text: word.width,
          border: getComputedStyle(button).borderLeftWidth, gap: icon ? word.left - icon.right : null };
      }));
      for (const button of buttons) {
        // Prototype dc.html:553: Cancel has 14px side padding; actions have
        // 16px side padding, a 14px icon and 6px gap; neither has a border.
        const expectedWidth = button.text + (button.name === 'Cancel' ? 28 : 52);
        assert.ok(Math.abs(button.width - expectedWidth) < .02, `${width}px ${button.name}: ${button.width}px versus prototype ${expectedWidth}px`);
        assert.equal(button.height, 34, `${width}px ${button.name} height`);
        assert.equal(button.border, '0px', `${width}px ${button.name} border`);
        if (button.name !== 'Cancel') assert.equal(button.gap, 6, `${width}px ${button.name} icon gap`);
      }
    }
  } finally { await page.close(); }
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

for (const width of [1280, 390]) for (const mode of ['pointer', 'keyboard']) {
  test(`${width}px ${mode} reply jump retains the prototype 80px feed offset and full visibility after active-row rendering`, async () => {
    const page = await browser.newPage({ viewport: { width, height: 844 }, reducedMotion: 'reduce' });
    try {
      await page.goto(url + '?long-reply=' + (width === 390 ? 'phone' : 'desktop'));
      await page.locator('.d-message-row__reply button').waitFor();
      await page.evaluate(() => document.fonts.ready);
      assert.ok(await page.getByRole('log').evaluate(el => el.scrollHeight > el.clientHeight * 2), 'the regression must exercise a genuinely overflowing log');
      const reply = page.locator('.d-message-row__reply button');
      if (mode === 'pointer') await reply.click();
      else { await reply.focus(); await page.keyboard.press('Enter'); }
      const row = page.locator('.d-message-row[data-msg-id="' + '01'.repeat(16) + '"]');
      await page.waitForFunction(() => document.querySelector('.d-message-row[data-flash="true"]') !== null);
      // Let native scroll events and the layout/ResizeObserver work caused by active/flash rendering finish.
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      const got = await row.evaluate(el => {
        const row = el.getBoundingClientRect(), log = el.closest('[role="log"]').getBoundingClientRect();
        return { top: row.top, bottom: row.bottom, logTop: log.top, logBottom: log.bottom,
          focused: document.activeElement === el, flash: el.dataset.flash, outline: getComputedStyle(el).outlineStyle };
      });
      assert.equal(got.focused, true);
      assert.equal(got.flash, 'true');
      assert.ok(got.top >= got.logTop - 1 && got.bottom <= got.logBottom + 1, `jump target must remain fully inside the log: ${JSON.stringify(got)}`);
      assert.ok(Math.abs(got.top - got.logTop - 80) <= 1, `jump target must retain the prototype 80px feed offset: ${JSON.stringify(got)}`);
      assert.equal(got.outline, mode === 'pointer' ? 'none' : 'solid');
    } finally { await page.close(); }
  });
}

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

for (const [name, index, shadow] of [
  ['question', 0, 'rgba(90, 168, 230, 0.15) 0px 0px 0px 3px'],
  ['answer', 1, 'none'],
]) test(`poll ${name} matches the native prototype focus border and ring for pointer and keyboard`, async () => {
  const page = await browser.newPage({ reducedMotion: 'reduce' });
  try {
    await page.goto(url + '?poll');
    const fields = page.locator('.d-poll-dialog input');
    await fields.first().waitFor();
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 });
      const field = fields.nth(index);
      for (const mode of ['pointer', 'keyboard']) {
        if (mode === 'pointer') await field.click();
        else {
          await fields.first().focus();
          await page.keyboard.press('Tab');
          if (index === 0) await page.keyboard.press('Shift+Tab');
        }
        const focused = await field.evaluate(el => {
          const style = getComputedStyle(el);
          return { active: document.activeElement === el, border: style.borderColor, shadow: style.boxShadow,
            outline: style.outlineStyle, offset: style.outlineOffset };
        });
        assert.equal(focused.active, true, `${width}px ${mode}`);
        assert.equal(focused.border, 'rgb(90, 168, 230)');
        assert.equal(focused.shadow, shadow, `${width}px ${mode}`);
        assert.equal(focused.outline, 'none');
        assert.equal(focused.offset, '0px');
        await field.evaluate(el => el.blur());
        assert.notEqual(await field.evaluate(el => getComputedStyle(el).borderColor), focused.border);
        assert.equal(await field.evaluate(el => getComputedStyle(el).boxShadow), 'none');
      }
    }
  } finally { await page.close(); }
});

test('overlong poll answer takes the native accent focus border for pointer and keyboard and restores danger on blur', async () => {
  const page = await browser.newPage({ reducedMotion: 'reduce' });
  try {
    await page.goto(url + '?poll&overlong-answer');
    const fields = page.locator('.d-poll-dialog input');
    const answer = fields.nth(1);
    await answer.waitFor();
    assert.equal((await answer.inputValue()).length, 101);
    for (const width of [1440, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const mode of ['pointer', 'keyboard']) {
        await fields.first().focus();
        assert.equal(await answer.evaluate(el => getComputedStyle(el).borderColor), 'rgb(255, 110, 110)');
        if (mode === 'pointer') await answer.click();
        else await page.keyboard.press('Tab');
        const focused = await answer.evaluate(el => {
          const s = getComputedStyle(el);
          return { active: document.activeElement === el, border: s.borderColor, shadow: s.boxShadow, outline: s.outlineStyle, offset: s.outlineOffset };
        });
        assert.equal(focused.active, true, `${width}px ${mode}`);
        assert.equal(focused.border, 'rgb(90, 168, 230)', `${width}px ${mode}`);
        assert.equal(focused.shadow, 'none');
        assert.equal(focused.outline, 'none');
        assert.equal(focused.offset, '0px');
        await fields.first().click();
        assert.equal(await answer.evaluate(el => getComputedStyle(el).borderColor), 'rgb(255, 110, 110)');
      }
      await checkPollFieldFocus(page);
    }
  } finally { await page.close(); }
});

test('phone toast keeps the over-25-MB copy on one intrinsic line at 390px and clamps narrower screens', async () => {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' });
  try {
    await page.goto(url);
    await page.evaluate(() => document.fonts.ready);
    await page.getByRole('button', { name: 'Show short toast', exact: true }).click();
    const short = await page.locator('.d-np-toast').boundingBox();
    assert.equal(short.height, 38);
    await page.getByRole('button', { name: 'Show toast', exact: true }).click();
    const read = () => page.locator('.d-np-toast').evaluate(el => {
      const r = el.getBoundingClientRect();
      return { width: r.width, height: r.height, left: r.left, right: r.right,
        overflows: el.scrollWidth > el.clientWidth + 1 };
    });
    const full = await read();
    assert.ok(Math.abs(full.width - 367) < .5, JSON.stringify(full));
    assert.equal(full.height, 38);
    assert.ok(Math.abs((full.left + full.right) / 2 - 195) < .5, JSON.stringify(full));
    assert.equal(full.overflows, false);
    await page.setViewportSize({ width: 320, height: 844 });
    const narrow = await read();
    assert.equal(narrow.width, 304);
    assert.equal(narrow.left, 8);
    assert.equal(narrow.right, 312);
    assert.ok(narrow.height > 38 && narrow.height < 65, JSON.stringify(narrow));
    assert.equal(narrow.overflows, false);
  } finally { await page.close(); }
});


test('phone reveal after a native wheel survives layout scrolling until a new wheel', async () => {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 }, reducedMotion: 'reduce' });
  try {
    // Keep old and new events inside the production 800ms window even on a slow CI host; timers and rendering remain real.
    await page.clock.setFixedTime(new Date('2026-10-31T16:00:00Z'));
    await page.goto(url + '?phone-reveal');
    const log = page.getByRole('log');
    const row = page.locator('.d-message-row').nth(4);
    await row.waitFor();
    await page.evaluate(() => document.fonts.ready);
    assert.ok(await log.evaluate(el => el.scrollHeight > el.clientHeight + 200));
    await log.evaluate(el => new Promise(resolve => {
      el.addEventListener('scroll', () => requestAnimationFrame(() => resolve()), { once: true });
      el.scrollTop = 0;
    }));
    await log.hover();
    await page.mouse.wheel(0, 80);
    await page.waitForFunction(() => document.querySelector('[role="log"]').scrollTop > 0);
    await row.locator('.d-message-row__body').click();
    assert.equal(await row.getAttribute('data-revealed'), 'true');
    await log.evaluate(el => new Promise(resolve => {
      el.addEventListener('scroll', () => requestAnimationFrame(() => resolve()), { once: true });
      el.scrollTop += 12;
    }));
    await page.mouse.move(380, 700); // reveal paint must survive after pointer hover leaves the row
    const revealed = await row.evaluate(el => {
      const style = getComputedStyle(el), toolbar = el.querySelector('.d-message-row__toolbar');
      return { on: el.dataset.revealed, fill: style.backgroundColor, stripe: style.boxShadow,
        toolbar: toolbar && getComputedStyle(toolbar).visibility };
    });
    assert.equal(revealed.on, 'true');
    assert.equal(revealed.fill, 'rgb(13, 16, 20)');
    assert.match(revealed.stripe, /rgb\(90, 168, 230\).*inset/);
    assert.equal(revealed.toolbar, 'visible');
    const before = await log.evaluate(el => el.scrollTop);
    await log.hover();
    await page.mouse.wheel(0, 80);
    await page.waitForFunction(top => document.querySelector('[role="log"]').scrollTop > top, before);
    await page.waitForFunction(() => document.querySelectorAll('.d-message-row[data-revealed="true"]').length === 0);
    assert.equal(await row.locator('.d-message-row__toolbar').count(), 0);
  } finally { await page.close(); }
});
