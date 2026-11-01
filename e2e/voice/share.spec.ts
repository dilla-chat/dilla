// VOICE-E2E (web-3 task 10): a screen share and a camera, on and off, as the other browser sees them. Chromium only:
// headless Firefox has no screen capture to pick and its fake camera is not a capture Playwright can grant (media plan
// DEV-63; NV-10 records screen share as a Chromium-measured path). Screen share starts from the person's own click
// (`getDisplayMedia` needs that activation); the fake UI flag picks the screen. #37's share without sound also runs in
// Firefox, on a drawn capture in place of the picker (see `silentDisplayMedia`).
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Locator, Page } from '@playwright/test';
import { expect, mediaWorkers, test } from './support/peer-browser';
import { watchContexts } from './support/leaks';
import {
  AUDIBLE_RMS, POLL, VoicePeer, WAIT, audible, audibleLevel, copy, framesPass, heard, joinVoice, noiseOff, onboard, screenTile, self,
  sentCount, stage, startShare, tile,
} from './support/voice';
import { chromiumMediaArgs, writeFixtures } from '../media/support/fixtures';

watchContexts(test);

// #37 (T4): what the share asks the browser for (T1): the capture constraints without processing and with the call's own
// playback left out (`restrictOwnAudio`), and every audio hint stated, never left to a browser default.
// Task 8c: the share starts from the picker with Tab picked (its sound on, the default), so the capture asks for the tab's sound
// at 1080p and 30 fps. Firefox's picker has no source or sound row (it ignores both), so it asks for no sound.
const SHARE_OPTIONS = {
  video: { displaySurface: 'browser', width: { max: 1920 }, height: { max: 1080 }, frameRate: { ideal: 30, max: 30 } },
  audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
  systemAudio: 'include', windowAudio: 'system', selfBrowserSurface: 'exclude', surfaceSwitching: 'exclude',
};
const SHARE_OPTIONS_FIREFOX = {
  video: { displaySurface: 'monitor', width: { max: 1920 }, height: { max: 1080 }, frameRate: { ideal: 30, max: 30 } },
  audio: false,
  systemAudio: 'exclude', windowAudio: 'exclude', selfBrowserSurface: 'exclude', surfaceSwitching: 'exclude',
};

/**
 * An init script (it may close over nothing): `getDisplayMedia` records the options it was called with on
 * `globalThis.__dmOpts` and returns a share without sound, as a whole-screen or window share on Linux (and any share in
 * Firefox) comes back. Chromium: the fake picker's stream (which carries a fake audio track) with its audio tracks stopped
 * and removed, counted on `__dmStripped`. Firefox (`substitute`): headless Firefox has no picker to pick a screen (DEV-63),
 * so the capture is a drawn canvas, video only, which is what Firefox returns for any surface (it captures no share audio
 * at all, Mozilla bug 1541425).
 */
function silentDisplayMedia(substitute: boolean): void {
  const w = globalThis as unknown as { __dmOpts?: unknown; __dmStripped: number };
  w.__dmStripped = 0;
  const native = MediaDevices.prototype.getDisplayMedia;
  MediaDevices.prototype.getDisplayMedia = async function (this: MediaDevices, options?: DisplayMediaStreamOptions) {
    w.__dmOpts = JSON.parse(JSON.stringify(options ?? null)) as unknown;
    if (substitute) {
      const canvas = document.createElement('canvas');
      canvas.width = 640;
      canvas.height = 360;
      const ctx = canvas.getContext('2d')!;
      let n = 0;
      const draw = setInterval(() => {
        n += 1;
        ctx.fillStyle = n % 2 === 0 ? '#204060' : '#406020';
        ctx.fillRect(0, 0, canvas.width, canvas.height);
      }, 50);
      const stream = canvas.captureStream(15);
      stream.getVideoTracks()[0]?.addEventListener('ended', () => { clearInterval(draw); });
      return stream;
    }
    const stream = await native.call(this, options);
    for (const t of stream.getAudioTracks()) {
      stream.removeTrack(t);
      t.stop();
      w.__dmStripped += 1;
    }
    return stream;
  };
}

/** This page's own encrypted frames in one E2EE slot (0 microphone, 1 camera, 2 screen, 3 screen audio), every key summed. */
async function slotSent(page: Page, slot: 0 | 1 | 2 | 3): Promise<number> {
  let n = 0;
  for (const wk of mediaWorkers(page)) {
    const v = await wk.evaluate((s) => {
      const st = (globalThis as unknown as { __dillaMediaStats?: { encrypted: Record<string, Record<string, number>> } }).__dillaMediaStats;
      let sum = 0;
      for (const slots of Object.values(st?.encrypted ?? {})) sum += slots[String(s)] ?? 0;
      return sum;
    }, slot).catch(() => 0);
    n = Math.max(n, v);
  }
  return n;
}

/** Frames of one received track (by its MediaStreamTrack id) that this page's media worker decrypted and authenticated. */
async function verifiedTrack(page: Page, trackId: string): Promise<number> {
  let n = 0;
  for (const wk of mediaWorkers(page)) {
    const v = await wk.evaluate((id) => {
      const st = (globalThis as unknown as { __dillaMediaStats?: { verifiedByTrack?: Record<string, number> } }).__dillaMediaStats;
      return st?.verifiedByTrack?.[id] ?? 0;
    }, trackId).catch(() => 0);
    n = Math.max(n, v);
  }
  return n;
}

type Played = { track: string; hz: number; gain: number; level: number };

/**
 * From here on, every remote call track the page plays through WebAudio (livekit's webAudioMix: a source into a gain node
 * into the destination) is watched: its gain node, and an analyser before and after that gain. Installed at run time, before
 * the tracks it watches are attached (`AudioNode.prototype.connect` is looked up at each call).
 */
async function watchGains(page: Page): Promise<void> {
  await page.evaluate(() => {
    type Watched = { track: string; gain: GainNode; pre: AnalyserNode; post: AnalyserNode };
    const w = window as unknown as { __gains?: Watched[]; __dw: { remoteTracks: Set<string> } };
    if (w.__gains !== undefined) return;
    const watched: Watched[] = [];
    w.__gains = watched;
    const connect = AudioNode.prototype.connect as (this: AudioNode, ...a: unknown[]) => unknown;
    AudioNode.prototype.connect = function (this: AudioNode, ...a: unknown[]) {
      const out = connect.call(this, ...a);
      const dest = a[0];
      if (this instanceof MediaStreamAudioSourceNode && dest instanceof GainNode) {
        const remote = this.mediaStream.getAudioTracks().find((t) => w.__dw.remoteTracks.has(t.id));
        if (remote !== undefined) {
          const pre = this.context.createAnalyser();
          const post = this.context.createAnalyser();
          pre.fftSize = 4096;
          post.fftSize = 2048;
          connect.call(this, pre);
          connect.call(dest, post);
          watched.push({ track: remote.id, gain: dest, pre, post });
        }
      }
      return out;
    } as typeof AudioNode.prototype.connect;
  });
}

/** The watched tracks the page still plays (a live track in one of its media elements): each one's id, its tone (the loudest
 *  frequency before the gain, in Hz), its gain now, and the loudest RMS it played after the gain over a short window (eight
 *  reads 50 ms apart, as `audibleLevel`: one read can fall in a gap of the fake microphone's tone). */
async function played(page: Page): Promise<Played[]> {
  return page.evaluate(async () => {
    type Watched = { track: string; gain: GainNode; pre: AnalyserNode; post: AnalyserNode };
    const w = window as unknown as { __gains: Watched[] };
    const live = new Set<string>();
    for (const el of document.querySelectorAll('audio, video')) {
      const src = (el as HTMLMediaElement).srcObject;
      if (src instanceof MediaStream) for (const t of src.getAudioTracks()) if (t.readyState === 'live') live.add(t.id);
    }
    const last = new Map<string, Watched>();
    for (const g of w.__gains) if (live.has(g.track)) last.set(g.track, g); // a re-attach makes a new gain node: the newest counts
    const watched = [...last.values()];
    const levels = watched.map(() => 0);
    for (let n = 0; n < 8; n++) {
      watched.forEach((g, i) => {
        const buf = new Float32Array(g.post.fftSize);
        g.post.getFloatTimeDomainData(buf);
        let sum = 0;
        for (const x of buf) sum += x * x;
        levels[i] = Math.max(levels[i]!, Math.sqrt(sum / buf.length));
      });
      await new Promise((r) => setTimeout(r, 50));
    }
    return watched.map((g, i) => {
      const spectrum = new Float32Array(g.pre.frequencyBinCount);
      g.pre.getFloatFrequencyData(spectrum);
      let peak = 1;
      for (let k = 2; k < spectrum.length; k++) if (spectrum[k]! > spectrum[peak]!) peak = k;
      return { track: g.track, hz: (peak * g.pre.context.sampleRate) / g.pre.fftSize, gain: g.gain.gain.value, level: levels[i]! };
    });
  });
}
/** The played track whose tone is `hz` (±60 Hz), or null. Alice's shared tab plays 440 Hz; her fake microphone sends 1 kHz. */
async function toneOf(page: Page, hz: number): Promise<Played | null> {
  return (await played(page)).find((g) => Math.abs(g.hz - hz) < 60) ?? null;
}

test('a screen share and a camera reach the other browser verified, and stop', async ({ peerBrowser, browserName }) => {
  test.skip(browserName !== 'chromium', 'DEV-63 / NV-10: screen and camera capture are proven on Chromium only');
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    const a = await self(alice.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    const seen = tile(bob.page, 'alice');
    await expect(seen).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(bob.page, a.device);
    const controls = alice.page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true });
    // Signal §5.6: the stage's tile is the camera or the screen itself (no glyph for them on the tile, as the prototype); the
    // sidebar's voice row still says it (camera, LIVE), so that is where bob reads alice's camera and share.
    const word = (key: string) => rowState(bob.page, 'alice', key);
    const video = seen.locator('video.d-call-tile__video');

    await test.step('share the screen after a click, then stop', async () => {
      const share = controls.locator('[data-control="share"]');
      await startShare(alice.page);
      await expect(share).toHaveAttribute('data-on', 'true', { timeout: WAIT });
      // web3.8: the screen is a tile of its own; alice's person tile stays an avatar (she shares no camera).
      const shared = screenTile(bob.page, 'alice');
      await expect(word('screen')).toBeVisible({ timeout: WAIT });
      await expect(seen).toHaveAttribute('data-source', 'avatar');
      // Only a verified stream is shown (L-UI-61…69): the tile renders the video once bob verified alice's frames.
      await expect(shared).toHaveAttribute('data-media', 'true', { timeout: WAIT });
      await expect.poll(() => shared.locator('video.d-call-tile__video').evaluate((v: HTMLVideoElement) => v.videoWidth), { timeout: WAIT }).toBeGreaterThan(0);
      await share.click();
      await expect(word('screen')).toHaveCount(0, { timeout: WAIT });
      await expect(shared).toHaveCount(0, { timeout: WAIT });
    });

    await test.step('camera on, then off', async () => {
      const camera = controls.locator('[data-control="camera"]');
      await camera.click();
      await expect(word('video')).toBeVisible({ timeout: WAIT });
      await expect(seen).toHaveAttribute('data-media', 'true', { timeout: WAIT });
      await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.videoWidth), { timeout: WAIT }).toBeGreaterThan(0);
      await expect(camera).toHaveAttribute('data-on', 'true');
      await camera.click();
      await expect(word('video')).toHaveCount(0, { timeout: WAIT });
      await expect(seen).not.toHaveAttribute('data-media', 'true', { timeout: WAIT });
    });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

/** The silent-share notice key this Chromium's own platform string calls for (the e2e's independent reading of #37 FX1). */
async function noticeKeyOf(page: Page): Promise<'voice.shareAudio.none.tabOnly' | 'voice.shareAudio.none.tabOrSystem'> {
  const platform = await page.evaluate(() => (navigator as Navigator & { userAgentData?: { platform?: string } }).userAgentData?.platform || navigator.userAgent);
  return /Windows|CrOS|Chrom(e|ium) ?OS|Mac/i.test(platform) ? 'voice.shareAudio.none.tabOrSystem' : 'voice.shareAudio.none.tabOnly';
}

// #37 (T4, D3): a share the browser gave no sound tells both sides: the sharer gets the stage notice that says how to share
// sound, and the viewer's screen tile carries the glyph-only `no sound` state (the volume-off icon, its word as the name).
test('a share without sound says so on both sides', async ({ peerBrowser, browserName }) => {
  test.skip(browserName !== 'chromium' && browserName !== 'firefox', 'a screen share is measured on Chromium and Firefox only');
  const firefox = browserName === 'firefox';
  const peer = peerBrowser.track(await VoicePeer.start());
  const alice = await peerBrowser.open('alice');
  const bob = await peerBrowser.open('bob');
  try {
    await alice.context.addInitScript(silentDisplayMedia, firefox);
    // The notice names what this browser can do (#37): Firefox shares no sound; Chromium a tab's, and on Windows, ChromeOS and
    // macOS the system's too. Playwright's desktop Chrome profile claims a Windows UA, so the platform is read from the page.
    const noticeKey = firefox ? 'voice.shareAudio.none.unsupported' : await noticeKeyOf(alice.page);
    await peer.setup();
    await onboard(alice, peer, 'alice');
    await onboard(bob, peer, 'bob');
    await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
    const a = await self(alice.page);
    await joinVoice(alice.page, peer.communityName, peer.voiceName);
    await joinVoice(bob.page, peer.communityName, peer.voiceName);
    await expect(tile(bob.page, 'alice')).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
    await heard(bob.page, a.device);
    const share = alice.page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true })
      .locator('[data-control="share"]');
    const notice = stage(alice.page).locator('.d-call-stage__notice[role="status"]');
    const shared = screenTile(bob.page, 'alice');
    const mark = stateWord(shared, 'no-sound');
    // Before the share: no notice (D4, nothing before the picker).
    await expect(notice).not.toContainText(copy(noticeKey));

    await startShare(alice.page, firefox ? [] : [copy('callStage.share.tab')]);
    await expect(share).toHaveAttribute('data-on', 'true', { timeout: WAIT });
    const opts = await alice.page.evaluate(() => (globalThis as unknown as { __dmOpts?: unknown }).__dmOpts);
    expect(opts, 'the options the share gave getDisplayMedia').toEqual(firefox ? SHARE_OPTIONS_FIREFOX : SHARE_OPTIONS);
    if (!firefox) {
      // The fake picker's stream carried sound, so the silence below is the wrapper's, as a Linux window share's is.
      expect(await alice.page.evaluate(() => (globalThis as unknown as { __dmStripped: number }).__dmStripped), 'audio tracks stripped')
        .toBeGreaterThan(0);
    }
    // The sharer: one polite notice, in the stage's status region, that says how to share sound.
    await expect(notice).toContainText(copy(noticeKey), { timeout: WAIT });
    // The viewer: the screen arrives verified, and its tile says it carries no sound, as an icon whose word is its name.
    await expect(shared).toHaveAttribute('data-media', 'true', { timeout: WAIT });
    await expect(mark).toBeVisible({ timeout: WAIT });
    await expect(mark).toHaveAttribute('role', 'img');
    await expect(mark).toHaveAccessibleName(copy('voice.state.noSound'));
    await expect(mark.locator('svg[data-icon="volume-off"]')).toHaveCount(1);
    await expect(shared.getByRole('img', { name: copy('voice.state.noSound'), exact: true })).toHaveCount(1);
    // Nothing to mute on a stream without sound (fix round 1: its person menu has no volume and no Mute for me).
    await shared.click({ button: 'right' });
    const silentMenu = bob.page.getByRole('menu', { name: copy('callStage.menu.label', { name: 'alice' }), exact: true });
    await expect(silentMenu).toBeVisible();
    await expect(silentMenu.getByRole('menuitemcheckbox')).toHaveCount(0);
    await bob.page.keyboard.press('Escape');
    // E2EE: the screen's frames went out encrypted (slot 2), and no screen-audio frame was sent at all (slot 3).
    expect(await slotSent(alice.page, 2), 'screen frames alice encrypted').toBeGreaterThan(0);
    expect(await slotSent(alice.page, 3), 'screen-audio frames alice encrypted (none published)').toBe(0);

    // Stop: the notice, the mark and the tile go.
    await share.click();
    await expect(share).not.toHaveAttribute('data-on', { timeout: WAIT });
    await expect(notice).not.toContainText(copy(noticeKey), { timeout: WAIT });
    await expect(mark).toHaveCount(0, { timeout: WAIT });
    await expect(shared).toHaveCount(0, { timeout: WAIT });
  } finally {
    await peerBrowser.close(bob);
    await peerBrowser.close(alice);
    await peer.close();
  }
});

// web3.4 V4: the share's sound, not only its picture. With the fake picker off, Chromium picks the tab that
// `--auto-select-tab-capture-source-by-title` names instead of the screen, and that tab's sound comes with it as the
// share's audio track. Playwright launches Chromium with `--mute-audio`, which silences every tab and so the captured
// sound too: this test drops it (measured: a tab capture reads an RMS of 0 with it, about 0.6 without). Playwright
// allows launch options only per file or worker, so the test launches a browser of its own with these switches; the
// rest of the suite keeps the worker's browser and its fake picker. That browser follows the config's `headless`; a
// headed run (`--headed`) plays the tab's 440 Hz tone and the fake microphone's 1 kHz tone aloud on this machine.
const TONE_TITLE = 'dilla-share-tone';
const fixtures = writeFixtures(join(tmpdir(), 'dilla-media-fixtures'));
const stateWord = (seen: Locator, key: string): Locator => seen.locator(`.d-call-tile__state[data-state="${key}"]`);
/** A person's state on the sidebar's voice row (camera, LIVE): the stage's tiles show the camera and the screen themselves. */
const rowState = (page: Page, name: string, key: string): Locator => page.locator('.d-voice-row__person')
  .filter({ has: page.locator('.d-voice-row__person-name', { hasText: new RegExp(`^${name}$`) }) })
  .locator(`.d-voice-row__state[data-state="${key}"]`);

test('a shared tab’s sound is heard by the other browser', async ({ peerBrowser, browserName, playwright, channel, headless }) => {
  test.skip(browserName !== 'chromium', 'DEV-63 / NV-10: screen capture, and a tab picked by its title, are Chromium only');
  const tabPicking = await playwright.chromium.launch({
    ...(channel === undefined ? {} : { channel }),
    headless,
    args: [
      ...chromiumMediaArgs(fixtures).filter((a) => a !== '--use-fake-ui-for-media-stream'),
      `--auto-select-tab-capture-source-by-title=${TONE_TITLE}`,
    ],
    ignoreDefaultArgs: ['--mute-audio'],
  });
  // The people close (the app's leave included) before their browser does, whatever threw.
  try {
    const peer = peerBrowser.track(await VoicePeer.start());
    const alice = await peerBrowser.open('alice', { browser: tabPicking });
    const bob = await peerBrowser.open('bob', { browser: tabPicking });
    try {
      // The tab alice shares: a 440 Hz tone (the fake microphone sends 1 kHz), under the title the switch picks.
      const tab = await alice.context.newPage();
      await tab.setContent(`<!doctype html><title>${TONE_TITLE}</title><p>tone</p>`);
      await tab.evaluate(() => {
        const ctx = new AudioContext();
        const osc = ctx.createOscillator();
        osc.frequency.value = 440;
        osc.connect(ctx.destination);
        osc.start();
      });
      await alice.page.bringToFront();
      await peer.setup();
      await onboard(alice, peer, 'alice');
      await onboard(bob, peer, 'bob');
      await peer.seat(2); // no hello: a voice tile names a member the page loaded after it (task 10, concern 4)
      const a = await self(alice.page);
      // #37 T5: alice's voice must be audible beside her stream (noise suppression would silence the fake microphone's tone),
      // and bob's page watches the gain of every call track it plays from before the first one is attached.
      await noiseOff(alice.page);
      await watchGains(bob.page);
      await joinVoice(alice.page, peer.communityName, peer.voiceName);
      await joinVoice(bob.page, peer.communityName, peer.voiceName);
      const seen = tile(bob.page, 'alice');
      await expect(seen).toHaveAttribute('data-voice-state', 'ok', { timeout: WAIT });
      await heard(bob.page, a.device);

      // Alice's microphone goes quiet, so the only sound of hers bob can hear is the share's.
      await alice.page.locator('.d-user-panel').getByRole('button', { name: copy('voice.mute'), exact: true }).click();
      await expect(stateWord(seen, 'muted')).toBeVisible({ timeout: WAIT });
      // Positive control for `audible` below: once the mute has reached the frames, bob's page plays nothing of alice's.
      await framesPass(alice.page, await sentCount(alice.page), 25);
      expect(await audibleLevel(bob.page), 'the level bob’s page plays of alice’s muted microphone').toBeLessThan(AUDIBLE_RMS);

      const share = alice.page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true })
        .locator('[data-control="share"]');
      // Task 8c: the picker's Tab with its sound (on by default).
      await startShare(alice.page, [copy('callStage.share.tab')]);
      await expect(share).toHaveAttribute('data-on', 'true', { timeout: WAIT });
      await expect(rowState(bob.page, 'alice', 'screen')).toBeVisible({ timeout: WAIT });
      await expect(screenTile(bob.page, 'alice')).toHaveAttribute('data-media', 'true', { timeout: WAIT });
      await audible(bob.page);
      // #37 (T4): a share with sound carries no mark for the viewer and no notice for the sharer.
      const shared = screenTile(bob.page, 'alice');
      await expect(stateWord(shared, 'no-sound')).toHaveCount(0);
      await expect(shared.getByRole('img', { name: copy('voice.state.noSound'), exact: true })).toHaveCount(0);
      // Whichever of the three notices this platform would say (#37 FX1): none of them shows.
      for (const key of ['voice.shareAudio.none.unsupported', 'voice.shareAudio.none.tabOnly', 'voice.shareAudio.none.tabOrSystem'] as const) {
        await expect(stage(alice.page).locator('.d-call-stage__notice[role="status"]')).not.toContainText(copy(key));
      }
      // E2EE: the tab's sound went out in the screen-audio slot, encrypted, and the very track bob plays at 440 Hz is one his
      // media worker decrypted and authenticated frame by frame.
      expect(await slotSent(alice.page, 3), 'screen-audio frames alice encrypted').toBeGreaterThan(0);
      let stream: Played | null = null;
      await expect.poll(async () => { stream = await toneOf(bob.page, 440); return stream?.level ?? 0; },
        { ...POLL, message: 'bob plays the shared tab’s 440 Hz tone' }).toBeGreaterThan(AUDIBLE_RMS);
      const streamTrack = (stream as Played | null)?.track ?? '';
      expect(await verifiedTrack(bob.page, streamTrack), 'frames of the stream bob plays that his media worker verified').toBeGreaterThan(0);

      await test.step('bob mutes alice’s stream from its tile: her stream goes silent, her voice plays on', async () => {
        // Alice speaks again: her voice (the fake microphone's 1 kHz) reaches bob beside the stream.
        await alice.page.locator('.d-user-panel').getByRole('button', { name: copy('voice.mute'), exact: true }).click();
        await expect(stateWord(seen, 'muted')).toHaveCount(0, { timeout: WAIT });
        await expect.poll(async () => (await toneOf(bob.page, 1000))?.level ?? 0, { ...POLL, message: 'bob plays alice’s voice' })
          .toBeGreaterThan(AUDIBLE_RMS);
        // Fix round 1 (Jonas): the stream's own Mute for me is in the screen tile's person menu (a right-click).
        await shared.click({ button: 'right' });
        const mute = bob.page.getByRole('menuitemcheckbox', { name: copy('callStage.menu.muteForMe'), exact: true });
        await expect(mute).toHaveAttribute('aria-checked', 'false');
        await mute.click();
        await expect(mute).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
        // The stream's gain falls to 0 and nothing of it plays; the voice keeps its gain and its sound.
        await expect.poll(async () => (await played(bob.page)).find((g) => g.track === streamTrack)?.gain ?? -1,
          { ...POLL, message: 'the gain of alice’s stream on bob’s page' }).toBeLessThan(0.001);
        await expect.poll(async () => (await played(bob.page)).find((g) => g.track === streamTrack)?.level ?? -1,
          { ...POLL, message: 'the level bob’s page plays of alice’s muted stream' }).toBeLessThan(AUDIBLE_RMS);
        await expect.poll(async () => {
          const voice = await toneOf(bob.page, 1000);
          return voice !== null && voice.track !== streamTrack && voice.gain > 0.9 && voice.level > AUDIBLE_RMS;
        }, { ...POLL, message: 'alice’s voice, a track of its own, keeps its gain and plays' }).toBe(true);
        // Still muted while the voice played: the stream did not come back.
        expect((await played(bob.page)).find((g) => g.track === streamTrack)?.gain ?? -1, 'the gain of alice’s muted stream').toBeLessThan(0.001);
        // The mute is bob's choice, not the stream's: no `no sound` mark.
        await expect(stateWord(shared, 'no-sound')).toHaveCount(0);
        // Unmute, from the keyboard this time: the stream plays again.
        await mute.focus();
        await mute.press('Space');
        await expect(mute).toHaveAttribute('aria-checked', 'false', { timeout: WAIT });
        await bob.page.keyboard.press('Escape');
        await expect.poll(async () => (await played(bob.page)).find((g) => g.track === streamTrack)?.level ?? 0,
          { ...POLL, message: 'the level bob’s page plays of alice’s unmuted stream' }).toBeGreaterThan(AUDIBLE_RMS);
      });

      await share.click();
      await expect(rowState(bob.page, 'alice', 'screen')).toHaveCount(0, { timeout: WAIT });
    } finally {
      await peerBrowser.close(bob);
      await peerBrowser.close(alice);
      await peer.close();
    }
  } finally {
    await tabPicking.close();
  }
});
