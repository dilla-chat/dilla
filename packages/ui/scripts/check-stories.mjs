// Runs the existing accessibility and DOM checks against every built Storybook story.
// The Storybook 10 loader cannot run inside Jest's module sandbox, so this uses
// Playwright directly while preserving the checks in story-a11y.ts.
import { chromium } from 'playwright';
import { VIEWPORT, declaredViewport, storyParameters } from './shoot-stories.mjs';
import { checkStory } from './story-a11y.ts';

const base = process.argv[2] === '--url' ? process.argv[3] : (process.argv[2] ?? 'http://127.0.0.1:6006');
const url = new URL(base);
if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || !url.port) {
  throw new Error('usage: check-stories.mjs http://127.0.0.1:PORT');
}

const indexResponse = await fetch(new URL('/index.json', url));
if (!indexResponse.ok) throw new Error(`Storybook index returned ${indexResponse.status}`);
const index = await indexResponse.json();
const stories = Object.values(index.entries).filter(entry => entry.type === 'story')
  .sort((a, b) => a.id.localeCompare(b.id));
if (stories.length === 0) throw new Error('Storybook index has no stories');

const browser = await chromium.launch({ headless: true });
let failures = 0;
try {
  const browserContext = await browser.newContext({ viewport: VIEWPORT });
  try {
    const parameters = await storyParameters(browserContext, Number(url.port), stories.map(story => story.id));
    for (const story of stories) {
      const page = await browserContext.newPage();
      try {
        await page.setViewportSize(declaredViewport(parameters.get(story.id)) ?? VIEWPORT);
        await page.addInitScript(() => {
          let preview;
          Object.defineProperty(window, '__STORYBOOK_PREVIEW__', {
            configurable: true,
            get: () => preview,
            set(value) {
              preview = value;
              value.channel.on('storyFinished', result => { window.__dillaStoryResult = result; });
              // A throwing play function (or render) still ends in storyFinished with status 'success' in this Storybook: its
              // assertions only count if the exception is caught here (task 8c fix round 1).
              const threw = event => error => { (window.__dillaStoryErrors ??= []).push(`${event}: ${String(error?.message ?? error?.description ?? error)}`); };
              value.channel.on('playFunctionThrewException', threw('play'));
              value.channel.on('storyThrewException', threw('render'));
              value.channel.on('storyErrored', threw('error'));
            },
          });
        });
        const iframe = new URL('/iframe.html', url);
        iframe.searchParams.set('id', story.id);
        iframe.searchParams.set('viewMode', 'story');
        await page.goto(iframe.href, { waitUntil: 'load', timeout: 60_000 });
        await page.waitForFunction(id => window.__dillaStoryResult?.storyId === id, story.id, { timeout: 60_000 });
        const result = await page.evaluate(() => window.__dillaStoryResult);
        if (result.status !== 'success') throw new Error(`Storybook reported ${result.status} for ${story.id}`);
        const errors = await page.evaluate(() => window.__dillaStoryErrors ?? []);
        if (errors.length > 0) throw new Error(`${story.id} threw: ${errors.join(' | ')}`);
        await page.waitForSelector('#storybook-root > *', { state: 'attached', timeout: 60_000 });
        await page.evaluate(() => document.fonts.ready);
        // Axe should inspect the settled colours; the flash story starts with a
        // transient highlight whose contrast is intentionally changing.
        await page.evaluate(() => Promise.race([
          Promise.all(document.getAnimations().filter(animation =>
            animation.effect?.getTiming().iterations !== Infinity,
          ).map(animation => animation.finished.catch(() => {}))),
          new Promise(resolve => setTimeout(resolve, 2_000)),
        ]));
        await checkStory(page, story);
        // Task 10b fix round 1: a press on the sensitivity meter sets the value under the pointer (the slider's thumb is 2px,
        // so the native value and the drawn line agree at every point, ends included).
        if (story.id === 'settings-sensitivitymeter--manual-drag') {
          const input = page.locator('.d-sens__input');
          const box = await input.boundingBox();
          for (const want of [-80, -20, -2]) {
            await page.mouse.click(box.x + box.width * (want + 100) / 100, box.y + box.height / 2);
            const got = Number(await input.inputValue());
            if (Math.abs(got - want) > 1) throw new Error(`SensitivityMeter: a press at ${want} dB set ${got}`);
            await page.waitForTimeout(400); // the line moves over --duration-normal (220ms)
            const line = await page.locator('.d-sens__line').boundingBox();
            const centre = line.x + line.width / 2;
            const expected = box.x + box.width * (got + 100) / 100;
            if (Math.abs(centre - expected) > 1.5) throw new Error(`SensitivityMeter: the line is at ${centre}, the value at ${expected}`);
          }
        }
        // Wave B Task 6: private result geometry and the prototype's chip colours, on both layouts.
        if (story.id.startsWith('conversation-botresult--')) {
          const got = await page.evaluate(() => {
            const row = document.querySelector('.d-bot-result');
            const chip = row.querySelector('.d-bot-result__status');
            const cs = getComputedStyle(chip);
            const avatar = row.querySelector('.d-message-row__avatar');
            return { status: row.dataset.status, chipHeight: chip.getBoundingClientRect().height,
              font: cs.fontSize, bg: cs.backgroundColor, fg: cs.color, radius: cs.borderRadius,
              avatar: [avatar.getBoundingClientRect().width, avatar.getBoundingClientRect().height],
              spin: getComputedStyle(row.querySelector('.d-bot-result__icon') ?? chip).animationDuration,
              tileRadius: row.querySelector('.d-bot-result__tile') ? getComputedStyle(row.querySelector('.d-bot-result__tile')).borderRadius : null,
              icon: chip.querySelector('[data-icon]')?.getAttribute('data-icon'),
              columns: getComputedStyle(row).gridTemplateColumns.split(' ').slice(0, 2),
              phone: document.querySelector('[data-layout="phone"]') !== null };
          });
          if (got.chipHeight !== 18 || got.font !== '10.5px' || got.radius !== '4px') throw new Error(`BotResult: wrong chip geometry ${JSON.stringify(got)}`);
          if (got.avatar.some(size => size !== 30)) throw new Error(`BotResult: wrong avatar ${got.avatar}`);
          if (got.columns[0] !== (got.phone ? '4px' : '44px') || got.columns[1] !== '30px') throw new Error(`BotResult: wrong row columns ${got.columns}`);
          const fills = { pending: 'rgba(90, 168, 230, 0.12)', success: 'rgba(124, 255, 142, 0.1)', failed: 'rgba(255, 110, 110, 0.1)', cancelled: 'rgba(255, 209, 106, 0.1)' };
          if (got.bg !== fills[got.status]) throw new Error(`BotResult: wrong fill ${got.bg}`);
          if (got.tileRadius !== null && got.tileRadius !== '4px') throw new Error(`BotResult: wrong D tile radius ${got.tileRadius}`);
          if (got.status === 'cancelled' && (got.fg !== 'rgb(255, 209, 106)' || got.icon !== 'triangle-alert')) throw new Error(`BotResult: wrong Not confirmed ink/icon ${got.fg} ${got.icon}`);
          if (got.status === 'pending' && got.spin !== '0.9s') throw new Error(`BotResult: wrong spinner duration ${got.spin}`);
        }
        // Wave B Task 2: the command picker's prototype geometry (a 260px list, rows of at least 40px, the chip rings) and
        // no keyboard hints on a phone.
        if (story.id.startsWith('conversation-commandpicker--')) {
          const got = await page.evaluate(() => {
            const style = selector => { const el = document.querySelector(selector); return el === null ? null : getComputedStyle(el); };
            return {
              listMax: style('.d-cmd-list__scroll')?.maxHeight ?? null,
              rows: [...document.querySelectorAll('.d-cmd-row')].map(el => el.getBoundingClientRect().height),
              ring: style('.d-cmd-field[data-active="true"]')?.boxShadow ?? null,
              invalid: document.querySelector('.d-cmd-field[data-invalid="true"]') !== null,
              hints: [...document.querySelectorAll('.d-cmd-list__hint, .d-cmd-bar__hint')].map(el => getComputedStyle(el).display),
              phone: document.querySelector('[data-layout="phone"]') !== null,
            };
          });
          if (got.listMax !== null && got.listMax !== '260px') throw new Error(`CommandPicker: the list's max height is ${got.listMax}, want 260px`);
          if (got.rows.some(h => h < 40)) throw new Error(`CommandPicker: a command row is under 40px (${got.rows.join(', ')})`);
          const wantRing = got.invalid ? 'rgba(255, 110, 110, 0.15) 0px 0px 0px 3px' : 'rgba(90, 168, 230, 0.12) 0px 0px 0px 3px';
          if (got.ring !== null && got.ring !== wantRing) throw new Error(`CommandPicker: the active chip's ring is ${got.ring}, want ${wantRing}`);
          if (got.phone && got.hints.some(display => display !== 'none')) throw new Error('CommandPicker: a keyboard hint shows on a phone');
          if (!got.phone && got.hints.some(display => display === 'none')) throw new Error('CommandPicker: a keyboard hint is hidden on a desktop');
        }
        // Wave B parity fix: a long room name ends in an ellipsis inside the person menu's 236px; the menu does not scroll sideways
        // and every keycap and head count lies inside it.
        if (story.id === 'voice-personcontextmenu--you-long-room-name' || story.id === 'voice-personcontextmenu--you') {
          const got = await page.evaluate(() => {
            const popup = document.querySelector('.d-person-menu');
            if (popup === null) return null;
            const box = popup.getBoundingClientRect();
            return {
              width: box.width, scrolls: popup.scrollWidth > popup.clientWidth,
              hints: [...popup.querySelectorAll('.d-action-menu__hint')].map(el => { const r = el.getBoundingClientRect(); return r.width > 0 && r.right <= box.right; }),
              truncated: [...popup.querySelectorAll('[data-room="true"] .d-action-menu__label')].some(el => el.scrollWidth > el.clientWidth),
            };
          });
          if (got === null) throw new Error('PersonContextMenu: no menu is open');
          if (got.scrolls || got.width !== 236) throw new Error(`PersonContextMenu: the menu is ${got.width}px wide and ${got.scrolls ? 'scrolls sideways' : 'fits'}, want 236px without scrolling`);
          if (got.hints.length < 4 || got.hints.some(ok => !ok)) throw new Error(`PersonContextMenu: a keycap or head count is out of view (${got.hints.join(', ')})`);
          if (story.id.endsWith('long-room-name') && !got.truncated) throw new Error('PersonContextMenu: the long room name is not truncated');
        }
        process.stdout.write(`PASS ${story.id}\n`);
      } catch (error) {
        failures += 1;
        process.stderr.write(`FAIL ${story.id}: ${error?.stack ?? error}\n`);
      } finally {
        await page.close();
      }
    }
  } finally {
    await browserContext.close();
  }
} finally {
  await browser.close();
}
process.stdout.write(`${stories.length - failures}/${stories.length} stories passed\n`);
if (failures > 0) process.exitCode = 1;
