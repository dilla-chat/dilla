// web3.8 WS7 (R26-R28): the call stage as the other browser sees it: one tile per source, auto-focus of a shared screen,
// a focus change that never restarts a stream, Escape back to the grid, full screen, and the 1440x900 shots.
//
// This spec must not call `VoicePeer.callJoin` (M6): ring.spec moves the instance clock first in the same worker, and a
// native peer never enters the LiveKit room, so it would render no media tile. Everyone here is a browser context (alice,
// bob, carl, dana), which is also how the four-person grid is made (pre-flight C-8). Chromium only, like share.spec
// (DEV-63 / NV-10): the camera and screen are the fakes of share.spec, `chromiumMediaArgs` and `writeFixtures` from
// e2e/media/support/fixtures (the fake camera feed, and the fake UI flag that picks the screen when the share button is
// clicked); a share starts from the person's own click.
import { mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Locator, Page } from '@playwright/test';
import { expect, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  VoicePeer, WAIT, connected, copy, dmPair, heard, joinVoice, onboard, rings, screenTile, self, stage, startShare, stripButton, tile,
} from './support/voice';
import { expectAccessible } from '../web/support/app';
import { REPO_ROOT } from '../web/support/host';
import { writeFixtures } from '../media/support/fixtures';

watchContexts(test);
// The config launches Chromium with the same fixtures; writing them again is idempotent and names the files this spec relies on.
writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));

const SHOTS = join(REPO_ROOT, 'target', 'ui-screens', 'web3.8');
const VIEWPORT = { width: 1440, height: 900 };

const tiles = (page: Page): Locator => stage(page).locator('li.d-call-stage__tile');
const focusButton = (tileLocator: Locator): Locator => tileLocator.locator('.d-call-tile__focus');
/** The `li` of a `.d-call-tile`. */
const itemOf = (tileLocator: Locator): Locator => tileLocator.locator('xpath=ancestor::li[1]');
const widthOf = async (l: Locator): Promise<number> => (await l.boundingBox())?.width ?? 0;
const fullscreenId = (page: Page) => page.evaluate(() => document.fullscreenElement?.getAttribute('data-tile-id') ?? null);
const videoWidthOf = (l: Locator) => l.locator('video').first().evaluate((v: HTMLVideoElement) => v.videoWidth);

test('the stage shows a tile per camera and screen, focuses a shared screen, and keeps every stream running', async ({ peerBrowser, browserName }) => {
  test.skip(browserName !== 'chromium', 'DEV-63 / NV-10: screen and camera capture are proven on Chromium only');
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice', { viewport: VIEWPORT });
  const bob = await peerBrowser.open('bob', { viewport: VIEWPORT });
  const carl = await peerBrowser.open('carl', { viewport: VIEWPORT });
  const dana = await peerBrowser.open('dana', { viewport: VIEWPORT });
  try {
    await peer.setup();
    for (const [person, name] of [[alice, 'alice'], [bob, 'bob'], [carl, 'carl'], [dana, 'dana']] as const) await onboard(person, peer, name);
    await peer.seat(4); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    mkdirSync(SHOTS, { recursive: true });
    const a = await self(alice.page);

    await test.step('bob alone: a grid of one tile', async () => {
      await joinVoice(bob.page, peer.communityName, peer.voiceName);
      await expect(tiles(bob.page)).toHaveCount(1, { timeout: WAIT });
      await bob.page.screenshot({ path: join(SHOTS, 'stage-grid-1.png') });
    });

    await test.step('alice joins: a grid of two', async () => {
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await heard(bob.page, a.device);
      await expect(tiles(bob.page)).toHaveCount(2);
      await expect(tile(bob.page, 'alice')).toHaveAttribute('data-source', 'avatar');
      await bob.page.screenshot({ path: join(SHOTS, 'stage-grid-2.png') });
    });

    const controls = alice.page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true });
    const shared = screenTile(bob.page, 'alice');
    const camera = tile(bob.page, 'alice');

    await test.step('1. alice shares her camera and her screen', async () => {
      const cameraOn = controls.locator('[data-control="camera"]');
      await cameraOn.click();
      await expect(cameraOn).toHaveAttribute('data-on', 'true', { timeout: WAIT });
      // Camera keeps its word, so its state is aria-pressed (Signal §5.6).
      await expect(controls.getByRole('button', { name: copy('callStage.bar.camera'), exact: true })).toHaveAttribute('aria-pressed', 'true');
      const share = controls.locator('[data-control="share"]');
      // Task 8c: the share picker's options: 720p at 15 fps. Before that, the default 1080p at 30 fps estimates 4.5 Mb/s (the
      // instance's default cap is 8000 kb/s, fix round 1).
      await controls.locator('[data-control="share"]').click();
      await expect(alice.page.getByText(copy('callStage.share.estimate', { mbps: '4.5' }), { exact: true })).toBeVisible();
      await alice.page.keyboard.press('Escape');
      await startShare(alice.page, ['720p', '15']);
      await expect(share).toHaveAttribute('data-on', 'true', { timeout: WAIT });
      // The capture took the options: no frame over 720 lines, at most 15 frames a second.
      await expect.poll(() => alice.page.evaluate(() => {
        const v = document.querySelector<HTMLVideoElement>('.d-call-tile[data-own][data-source="screen"] video');
        return (v?.srcObject as MediaStream | null)?.getVideoTracks()[0]?.getSettings().height ?? 0;
      }), { timeout: WAIT }).toBeGreaterThan(0);
      const settings = await alice.page.evaluate(() => {
        const v = document.querySelector<HTMLVideoElement>('.d-call-tile[data-own][data-source="screen"] video');
        return (v?.srcObject as MediaStream | null)?.getVideoTracks()[0]?.getSettings() ?? null;
      });
      expect(settings?.height ?? 9999, 'the captured screen’s height').toBeLessThanOrEqual(720);
      expect(settings?.frameRate ?? 999, 'the captured screen’s frame rate').toBeLessThanOrEqual(15);
    });

    await test.step('2. bob sees two tiles for alice; the screen is focused and takes most of the stage', async () => {
      await expect(camera).toHaveAttribute('data-source', 'camera', { timeout: WAIT });
      await expect(camera).toHaveAttribute('data-media', 'true', { timeout: WAIT });
      await expect(shared).toHaveAttribute('data-media', 'true', { timeout: WAIT });
      await expect(tiles(bob.page)).toHaveCount(3);
      await expect(focusButton(shared)).toHaveAttribute('aria-pressed', 'true');
      await expect(focusButton(camera)).toHaveAttribute('aria-pressed', 'false');
      const stageWidth = await widthOf(bob.page.locator('.d-call-stage'));
      // The tile's width eases over 260ms (Signal §5.6), so it is read once it has settled.
      await expect.poll(() => widthOf(itemOf(shared)), { timeout: WAIT, message: 'the focused screen tile’s width against the stage' })
        .toBeGreaterThanOrEqual(stageWidth * 0.7);
      await expect.poll(() => videoWidthOf(shared), { timeout: WAIT }).toBeGreaterThan(0);
      await expect.poll(() => videoWidthOf(camera), { timeout: WAIT }).toBeGreaterThan(0);
      // Task 8c: the focused share's stats as bob receives it ("720p · 15 fps" at most), and alice's own tile's sent codec.
      const stat = itemOf(shared).locator('.d-call-tile__stat');
      await expect(stat).toHaveText(/^\d+p · \d+ fps$/, { timeout: WAIT });
      // inbound-rtp framesPerSecond counts the frames decoded in the last second, so the jitter buffer's catch-up after a
      // stall on a loaded runner reads above the sender's 15 (PR #121 CI read 19 once). The capture's own rate is pinned in
      // step 1; here the received rate must settle at the cap, which an uncapped share would never do.
      const statNumbers = async () => ((await stat.textContent()) ?? '').match(/\d+/g)!.map(Number);
      await expect.poll(async () => (await statNumbers())[0], { timeout: WAIT, message: 'the received screen’s height' }).toBeLessThanOrEqual(720);
      await expect.poll(async () => (await statNumbers())[1], { timeout: WAIT, message: 'the received screen’s frame rate' }).toBeLessThanOrEqual(16);
      await expect(alice.page.locator('.d-call-tile[data-own]:not([data-source="screen"])').locator('xpath=ancestor::li[1]').locator('.d-call-tile__stat'))
        .toHaveText('opus 48k', { timeout: WAIT });
      // Fix round 1: alice's ping on bob's view of her person tile is the RTT her own page publishes (dilla.rtt, through the
      // SFU with the token's own-metadata right), not bob's link.
      await expect(itemOf(camera).locator('.d-call-tile__stat')).toHaveText(/^\d{1,4} ms$/, { timeout: WAIT });
      await bob.page.screenshot({ path: join(SHOTS, 'stage-focused.png') });
      await expectAccessible(bob.page, 'the focused stage');
    });

    await test.step('3. a click on alice’s camera tile swaps the focus and the screen’s video stays the same element', async () => {
      const video = await shared.locator('video').first().elementHandle();
      expect(video, 'the screen tile’s video').not.toBeNull();
      const before = await videoWidthOf(shared);
      expect(before).toBeGreaterThan(0);
      await focusButton(camera).click();
      await expect(focusButton(camera)).toHaveAttribute('aria-pressed', 'true');
      await expect(focusButton(shared)).toHaveAttribute('aria-pressed', 'false');
      const after = await video!.evaluate((v: HTMLVideoElement) => ({ connected: v.isConnected, width: v.videoWidth }));
      expect(after.connected, 'the captured screen video is still in the document').toBe(true);
      expect(after.width, 'and still has a picture').toBeGreaterThan(0);
      // Not the same intrinsic width: adaptive stream and simulcast move between layers as the bandwidth estimate ramps
      // (PR #128 CI read 640, then 1280, across this swap). The tile must still show the same element with a picture.
      expect(await shared.locator('video').first().evaluate((v, el) => v === el, video)).toBe(true);
      expect(await videoWidthOf(shared)).toBeGreaterThan(0);
    });

    await test.step('4. Escape returns to the grid; every tile is the same width', async () => {
      await bob.page.keyboard.press('Escape');
      await expect(focusButton(camera)).toHaveAttribute('aria-pressed', 'false');
      await expect(focusButton(shared)).toHaveAttribute('aria-pressed', 'false');
      const items = tiles(bob.page);
      await expect(items).toHaveCount(3); // alice’s camera and screen, and bob’s own (pre-flight C-25)
      await expect(bob.page.locator('.d-call-stage')).not.toHaveAttribute('data-focused-stage', 'true');
      // Signal §5.6: a tile's width eases over 260ms (the prototype's width transition), so the grid is read once it has settled.
      await expect.poll(async () => {
        const widths = await Promise.all([0, 1, 2].map((i) => widthOf(items.nth(i))));
        return Math.max(...widths) - Math.min(...widths);
      }, { timeout: WAIT, message: 'every tile is the same width' }).toBeLessThanOrEqual(1);
      await expectAccessible(bob.page, 'the grid of three');
    });

    await test.step('5. the full-screen button makes the focused tile full screen; a double-click leaves it', async () => {
      await focusButton(shared).click();
      await expect(focusButton(shared)).toHaveAttribute('aria-pressed', 'true');
      await itemOf(shared).locator('.d-call-tile__fullscreen').click();
      await expect.poll(() => fullscreenId(bob.page), { timeout: WAIT }).toBe(`${a.device}:screen`);
      await focusButton(shared).dblclick();
      await expect.poll(() => fullscreenId(bob.page), { timeout: WAIT }).toBeNull();
    });

    await test.step('alice stops her camera and screen; the focus and the tiles go', async () => {
      await controls.locator('[data-control="share"]').click();
      await controls.locator('[data-control="camera"]').click();
      await expect(shared).toHaveCount(0, { timeout: WAIT });
      await expect(camera).toHaveAttribute('data-source', 'avatar', { timeout: WAIT });
      await expect(tiles(bob.page)).toHaveCount(2);
      await expect(bob.page.locator('li.d-call-stage__tile[data-focused="true"]')).toHaveCount(0);
    });

    await test.step('carl and dana join as two more browsers: a grid of four', async () => {
      await joinVoice(carl.page, peer.communityName, peer.voiceName);
      await joinVoice(dana.page, peer.communityName, peer.voiceName);
      await expect(tiles(bob.page)).toHaveCount(4, { timeout: WAIT });
      await expect(tile(bob.page, 'carl')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await expect(tile(bob.page, 'dana')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await bob.page.screenshot({ path: join(SHOTS, 'stage-grid-4.png') });
      await expectAccessible(bob.page, 'the grid of four');
    });
  } finally {
    await peerBrowser.close(dana);
    await peerBrowser.close(carl);
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

// The DM call box is not the channel stage: it keeps its compact grid (no stage mode, no focus), so a shared screen shows
// at the box's normal tile size. Without that it was a 2 px high green line in the 40 % box (WS7 review, critical 1).
test('a DM call box keeps compact tiles when the other person shares their screen', async ({ peerBrowser, browserName }) => {
  test.skip(browserName !== 'chromium', 'DEV-63 / NV-10: screen capture is proven on Chromium only');
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice', { viewport: VIEWPORT });
  const bob = await peerBrowser.open('bob', { viewport: VIEWPORT });
  try {
    await dmPair(alice, bob, peer);
    await stripButton(alice.page, 'call.start').click();
    await connected(alice.page);
    await rings(bob.page).getByRole('button', { name: copy('call.ring.answer'), exact: true }).click();
    await connected(bob.page);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    const share = stage(alice.page).getByRole('button', { name: copy('voice.shareOn'), exact: true });
    await share.click();
    await expect(share).toHaveAttribute('aria-pressed', 'true', { timeout: WAIT });

    const shared = screenTile(bob.page, 'alice');
    await expect(shared).toHaveAttribute('data-media', 'true', { timeout: WAIT });
    await expect.poll(() => videoWidthOf(shared), { timeout: WAIT }).toBeGreaterThan(0);
    const mainHeight = (await bob.page.getByRole('main').boundingBox())?.height ?? 0;
    expect(mainHeight).toBeGreaterThan(0);
    const video = await shared.locator('video').first().boundingBox();
    expect(video?.height ?? 0, 'the shared screen’s video height').toBeGreaterThan(20);
    await expect(stage(bob.page).locator('[data-tiles="stage"]')).toHaveCount(0);
    await expect(stage(bob.page).locator('li[data-focused="true"]')).toHaveCount(0);
    await expect(stage(bob.page).locator('.d-call-tile__fullscreen')).toHaveCount(0);
    for (const box of await tiles(bob.page).evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height))) {
      expect(box, 'a DM box tile’s height against 40 % of the pane').toBeLessThanOrEqual(mainHeight * 0.4);
      expect(box).toBeGreaterThan(20);
    }
    await bob.page.screenshot({ path: join(SHOTS, 'dm-share.png') });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});
