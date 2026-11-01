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
