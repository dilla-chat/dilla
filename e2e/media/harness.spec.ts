// The harness smoke: three isolated Chromium contexts connect straight to the test
// host's LiveKit with debug tokens (plan MD-13), publish the fake microphone and camera through the
// stub manager's pass-through transforms, and each sees every remote track decode. Every later
// media test and spike stands on this. Desktop ships Chromium on every OS.
import { type Page } from '@playwright/test';
import { expect, test } from './support/test.ts';
import { CONTROL } from './support/testhost.ts';
import { debugToken, deviceIdentity, sfuInfo, waitForDecode, type HarnessWindow } from './support/lk.ts';

test.skip(({ browserName }) => browserName !== 'chromium', 'desktop media uses Chromium');

async function join(page: Page, url: string, room: string): Promise<void> {
  const { token } = await debugToken(CONTROL, room, deviceIdentity());
  await page.goto('/');
  await page.waitForFunction(() => 'harness' in window);
  await page.evaluate(async ([u, t]) => {
    const h = (window as unknown as HarnessWindow).harness;
    await h.connect(u, t, { e2ee: 'stub', stub: { kind: 'pass' } });
    await h.publish({ mic: true, camera: true });
  }, [url, token] as const);
}

test('three isolated Chromium contexts decode each other through the in-process SFU', async ({ browser }) => {
  const { url } = await sfuInfo(CONTROL);
  const room = `harness-${Date.now()}`;
  const contexts = await Promise.all(Array.from({ length: 3 }, () => browser.newContext({ permissions: ['camera', 'microphone'] })));
  try {
    const pages = await Promise.all(contexts.map(context => context.newPage()));
    for (const p of pages) await join(p, url, room);

    for (const p of pages) {
      // Two remote participants, a camera and a microphone each: wait for all four tracks, not for the
      // first participant's (a slow runner subscribes the second one seconds later; PR #7).
      const stats = await waitForDecode(p, 10, 30_000, 4);
      expect(stats.filter((s) => s.kind === 'video')).toHaveLength(2);
      expect(stats.filter((s) => s.kind === 'audio')).toHaveLength(2);
      const stub = await p.evaluate(() => (window as unknown as HarnessWindow).harness.stubStats());
      expect(stub.transforms).toBeGreaterThanOrEqual(6); // 2 senders + 4 receivers
      expect(stub.errors).toBe(0);
    }

    const probes = await pages[2].evaluate(() => (window as unknown as HarnessWindow).harness.renderProbe(3_000));
    expect(probes.filter((p) => p.kind === 'video')).toHaveLength(2);
    expect(probes.filter((p) => p.kind === 'audio')).toHaveLength(2);
    for (const p of probes) {
      if (p.kind === 'video') expect(p.frames, `video from ${p.participantIdentity}`).toBeGreaterThan(0);
      else expect(p.rms, `audio from ${p.participantIdentity}`).toBeGreaterThan(0.01);
    }
  } finally { await Promise.all(contexts.map(context => context.close())); }
});
