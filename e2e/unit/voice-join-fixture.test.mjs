import test from 'node:test';
import assert from 'node:assert/strict';
// @playwright/test, as every e2e spec: its browser build is the one CI installs.
import { chromium } from '@playwright/test';

async function inPage(run) {
  // channel 'chromium' as in the e2e configs: CI installs the full Chromium, not the headless shell.
  const browser = await chromium.launch({ headless: true, channel: 'chromium' });
  try { await run(await browser.newPage()); } finally { await browser.close(); }
}

test('voice fixture observes a delayed automatic join without waiting for a missing Join button', async () => {
  const { waitForVoiceJoin } = await import('../ux/voice-fixture.mjs');
  await inPage(async page => {
    await page.setContent('<main class="dw-voice-stage" data-call-phase="joining">Connecting to harbour</main><div class="d-voice-dock" data-phase="connected">Call dock</div>');
    const joined = waitForVoiceJoin(page, { timeout: 2000 });
    await page.waitForTimeout(100);
    await page.locator('main').evaluate(el => { el.dataset.callPhase = 'in'; el.textContent = 'In harbour'; });
    await joined;
    assert.equal(await page.getByRole('button').count(), 0);
  });
});

test('voice fixture clicks the actual visible stage Join voice CTA only once', async () => {
  const { waitForVoiceJoin } = await import('../ux/voice-fixture.mjs');
  await inPage(async page => {
    await page.setContent('<main class="dw-voice-stage" data-call-phase="idle"><p>Harbour stage</p><button>Join voice</button></main><div class="d-voice-dock" data-phase="connected">Call dock</div>');
    await page.locator('button').evaluate(el => el.addEventListener('click', () => {
      window.joinClicks = (window.joinClicks ?? 0) + 1;
      el.closest('main').dataset.callPhase = 'joining';
      setTimeout(() => { el.closest('main').dataset.callPhase = 'in'; el.remove(); }, 80);
    }));
    await waitForVoiceJoin(page, { timeout: 2000 });
    assert.equal(await page.evaluate(() => window.joinClicks), 1);
  });
});

test('voice fixture retains blocked phase and visible reason in bounded failure diagnostics', async () => {
  const { waitForVoiceJoin } = await import('../ux/voice-fixture.mjs');
  await inPage(async page => {
    await page.setContent('<main class="dw-voice-stage" data-call-phase="blocked">Voice permission denied</main>');
    await assert.rejects(waitForVoiceJoin(page, { timeout: 100 }), /phase=blocked.*Voice permission denied/s);
  });
});

test('voice fixture rechecks connection when an automatic join removes the fallback CTA during its click', async () => {
  const { waitForVoiceJoin } = await import('../ux/voice-fixture.mjs');
  await inPage(async page => {
    await page.setContent('<main class="dw-voice-stage" data-call-phase="idle"><p>Harbour stage</p><button>Join voice</button></main><div class="d-voice-dock" data-phase="connected">Call dock</div>');
    await page.locator('button').evaluate(el => el.addEventListener('pointerover', () => {
      el.closest('main').dataset.callPhase = 'in';
      el.remove();
    }));
    await waitForVoiceJoin(page, { timeout: 2500 });
    assert.equal(await page.locator('main').getAttribute('data-call-phase'), 'in');
  });
});

test('voice fixture accepts a connected dock that is hidden while the active stage is in call', async () => {
  const { waitForVoiceJoin } = await import('../ux/voice-fixture.mjs');
  await inPage(async page => {
    await page.setContent('<main class="dw-voice-stage" data-call-phase="in">Björn (you), Mute, Share, Leave</main><div class="d-voice-dock" data-phase="connected" hidden></div>');
    await waitForVoiceJoin(page, { timeout: 5_000 });
    assert.equal(await page.locator('.d-voice-dock').isVisible(), false, 'collapsed dock is a valid connected stage layout');
  });
});
