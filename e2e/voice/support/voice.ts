// The voice suite's page contract and its native peer (L-E2E-31 helpers): `joinVoice`, `tileState`, `audioRunning`,
// `heard`, `notificationsSeen`, and reads of what the recorders of peer-browser.ts saw. Every wait waits for a state —
// `data-voice-state`, `data-call-phase`, `data-ring`, `data-media-state`, the voice snapshot, a media counter — never
// for a duration (lesson c), and none asserts on how late LiveKit shows a joiner (media card 23).
import { randomInt } from 'node:crypto';
import { expect, test, type Locator, type Page } from '@playwright/test';
import type { CallState, Ring, TrustSummary, VoiceState } from '../../../packages/client-core/src/state/types';
import {
  WebDriver, testHostUrl, testkitEnv, type PeerActor, type PeerCallCheck, type PeerCallJoined, type PeerCallMembers,
  type PeerCallRead, type PeerCallStarted, type PeerSetup,
} from '../../web/support/driver';
import { addServerVia,
  channelsNav, closeSettings, copy, dmComposer, dmsNav, escapeRegExp, expectAccessible, homeTile, expectShell, instanceInvite, messageRow, namedRow,
  openSettings, railItem, railNav, sendText, signUp, type Account, type SettingsSection,
} from '../../web/support/app';
import { WAIT, mediaWorkers, type Person } from './peer-browser';

export { WAIT, closeSettings, copy, instanceInvite };
/** The shell is up again (after a reload): the rail shows and no onboarding step does. */
export async function expectShellReady(page: Page): Promise<void> { await expectShell(page); }
export const POLL = { timeout: WAIT, intervals: [100, 250, 500, 1000] };

/**
 * The instance clock's one invariant (task-10 review M6). The test host's fake clock is frozen, which is what keeps the
 * room sweep's absence timer and the 30 s admission window still while a native peer holds a call-group leaf it never
 * takes into the room. `advanceClock` moves it for good (no reset), so a native peer must never join a call after it
 * moved in this worker process (workers: 1 shares it across spec files). A run that breaks this — a new spec, a shard,
 * a reorder — fails here, by name, instead of being swept at random.
 */
let instanceClockMoved: string | null = null;

// ---------------------------------------------------------------------------------------------------
// The native peer: a web-driver process against the voice host (8481), one per role in a test.

export class VoicePeer {
  communityName = '';
  setupAnswer: PeerSetup | null = null;
  private closed = false;
  private constructor(readonly driver: WebDriver, readonly seed: number) {}

  static async start(): Promise<VoicePeer> {
    const seed = randomInt(1, 0xff_ffff);
    return new VoicePeer(await WebDriver.start(testHostUrl(), testkitEnv(), seed), seed);
  }

  /** A community with the text channel `general` (its group registered) and the voice channel `general-voice`
   *  (L-E2E-30 `setup {voice}`). */
  async setup(o: { password?: string } = {}): Promise<PeerSetup & { voice_channel_id: string }> {
    this.communityName = `v-${this.seed.toString(16)}`;
    const s = await this.driver.setup({ community: this.communityName, channel: 'general', voice: true, ...o });
    if (s.voice_channel_id === null) throw new Error('setup {voice: true} answered no voice channel');
    this.setupAnswer = s;
    await this.driver.register();
    return s as PeerSetup & { voice_channel_id: string };
  }
  /** Commits the browsers' Adds to `general` until its roster holds the peer and `browsers` more devices: each browser
   *  then knows the others by name (the members slice) and holds the peer's identity (trust). */
  async seat(browsers: number): Promise<string[]> { return this.waitForDevices(browsers + 1, this.setupDone.channel_id); }
  readonly voiceName = 'general-voice';
  readonly textName = 'general';
  get setupDone(): PeerSetup {
    if (this.setupAnswer === null) throw new Error('setup first');
    return this.setupAnswer;
  }
  register(): Promise<{ group_id: string; epoch: number }> { return this.driver.register(); }
  callJoin(channel: string, actor: PeerActor = 'peer'): Promise<PeerCallJoined> {
    if (instanceClockMoved !== null) {
      throw new Error(`a native peer joins a call after the instance clock moved (${instanceClockMoved}): see M6 in voice.ts`);
    }
    return this.driver.callJoin(channel, actor);
  }
  callLeave(channel: string, actor: PeerActor = 'peer'): Promise<void> { return this.driver.callLeave(channel, actor); }
  callMembers(channel: string, actor: PeerActor = 'peer'): Promise<PeerCallMembers> { return this.driver.callMembers(channel, actor); }
  callStart(channel: string, actor: PeerActor = 'peer'): Promise<PeerCallStarted> { return this.driver.callStart(channel, actor); }
  callCheck(channel: string, actor: PeerActor = 'peer'): Promise<PeerCallCheck> { return this.driver.callCheck(channel, actor); }
  callRead(channel: string, actor: PeerActor = 'peer'): Promise<PeerCallRead> { return this.driver.callRead(channel, actor); }
  syncDm(channelId: string): Promise<{ members: number }> { return this.driver.syncDm(channelId); }
  callDecline(call: string, actor: PeerActor = 'peer'): Promise<void> { return this.driver.callDecline(call, actor); }
  forgeDevice(): Promise<{ device_id: string; version: number }> { return this.driver.forgeDevice(); }
  enrol(): Promise<{ device_id: string }> { return this.driver.enrol(); }
  rotateUmk(): Promise<{ user: string; version: number }> { return this.driver.rotateUmk(); }
  typing(channel: string, on: boolean): Promise<void> { return this.driver.typing(channel, on); }
  openDm(userId: string): Promise<{ channel_id: string; group_id: string; created: boolean }> { return this.driver.openDm(userId); }
  async syncDmMembers(channelId: string): Promise<number> { return (await this.driver.syncDm(channelId)).members; }
  sync(channel?: string): Promise<{ members: number }> { return this.driver.sync(channel); }
  async devices(channel?: string): Promise<string[]> { return (await this.driver.members(channel)).devices; }
  /** Closes the driver process once; the peerBrowser fixture closes every tracked peer again, harmlessly. */
  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    await this.driver.close();
  }

  /** Syncs until the text group's roster holds `count` devices (the peer commits the browsers' Adds). */
  async waitForDevices(count: number, channel?: string): Promise<string[]> {
    let devices: string[] = [];
    await expect.poll(async () => {
      await this.sync(channel);
      devices = await this.devices(channel);
      return devices.length;
    }, POLL).toBe(count);
    return devices;
  }

  /** Polls the actor's call-group roster (each poll syncs and commits outstanding proposals) until `match` holds. */
  async waitForCallMembers(channel: string, match: (devices: string[]) => boolean, what: string, actor: PeerActor = 'peer'): Promise<string[]> {
    let devices: string[] = [];
    await expect.poll(async () => {
      devices = (await this.callMembers(channel, actor)).devices;
      return match(devices);
    }, { ...POLL, message: `the ${actor}'s call roster ${what}` }).toBe(true);
    return devices;
  }
}

/** The name the app shows for the peer: its display name, else its username. */
export function peerName(s: PeerSetup): string { return s.display !== '' ? s.display : s.username; }

// ---------------------------------------------------------------------------------------------------
// Onboarding and navigation.

let cachedInstance: string | null = null;
/** The instance name of `GET /v1/instance` (element 5), which onboarding shows. */
export async function instanceName(): Promise<string> {
  if (cachedInstance !== null) return cachedInstance;
  const { arr, decode } = await import('../../../packages/client-core/src/cbor/index');
  const res = await fetch(`${testHostUrl()}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(res.status, 'GET /v1/instance').toBe(200);
  const name = arr(decode(new Uint8Array(await res.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('GET /v1/instance: element 5 is not the instance name');
  cachedInstance = name;
  return name;
}

/** Signs a fresh account up in this person's page and joins the peer's community with its invite. */
export async function onboard(person: Person, peer: VoicePeer, display: string): Promise<Account> {
  const account = await signUp(person.page, instanceInvite(), await instanceName(), display, process.env.DILLA_TESTKIT_CONTROL);
  const page = person.page;
  await addServerVia(page, 'join');
  const dialog = page.getByRole('dialog', { name: copy('join.title'), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await dialog.getByLabel(copy('join.invite'), { exact: true }).fill(peer.setupDone.invite_code);
  await dialog.getByRole('button', { name: copy('join.submit'), exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: WAIT });
  await expect(railItem(page, peer.communityName)).toBeVisible({ timeout: WAIT });
  await expectShell(page);
  await openText(page, peer.communityName, peer.textName);
  return account;
}

/**
 * Seats the browsers in `general` and has each say hello there, so every page knows every other by name: a page loads a
 * community's member list once, and looks a sender up again only when a message names one it lacks (L-TS-09). The DM
 * picker needs that (it lists the loaded members). A voice tile does not: a participant the list lacks is looked up
 * (task 10, concern 4), so the voice specs only seat the browsers (`seat`).
 */
export async function introduce(peer: VoicePeer, people: readonly Person[], browsersSeated = people.length): Promise<void> {
  await peer.seat(browsersSeated);
  const said = people.map((p) => `hello from ${p.name} ${randomInt(0, 0xff_ffff).toString(16)}`);
  for (const [i, p] of people.entries()) await sendText(p.page, peer.textName, said[i]);
  for (const p of people) {
    for (const text of said) await expect(messageRow(p.page, peer.textName, text)).toBeVisible({ timeout: WAIT });
  }
}

/** Opens a DM with `name` from the `@` tile's `message someone` picker (a member of the open server); answers its id. */
export async function openDmWith(page: Page, community: string, name: string): Promise<string> {
  await railItem(page, community).click();
  await homeTile(page).click();
  await page.getByRole('button', { name: copy('shell.dms.new'), exact: true }).click();
  const picker = page.getByRole('dialog', { name: copy('shell.dms.newTitle'), exact: true });
  await picker.getByRole('listitem').filter({ hasText: name }).getByRole('button', { name: copy('shell.dms.open'), exact: true }).click();
  await expect(page).toHaveURL(/\/dm\/[0-9a-f]{32}$/, { timeout: WAIT });
  await expect(dmComposer(page)).toBeEnabled({ timeout: WAIT });
  return new URL(page.url()).pathname.split('/')[2];
}
/** Alice and bob in the peer's server, their DM opened by alice and open on both sides. Answers the DM. The instance
 *  clock stays where the test host froze it (minutes behind the pages): a ring's 30 s run from its arrival on the page,
 *  never from expires_ts read on the page's clock (task 10, concern 5). */
export async function dmPair(alice: Person, bob: Person, peer: VoicePeer): Promise<string> {
  await peer.setup();
  await onboard(alice, peer, 'alice');
  await onboard(bob, peer, 'bob');
  await introduce(peer, [alice, bob]);
  const dm = await openDmWith(alice.page, peer.communityName, 'bob');
  await openDm(bob.page, dm, 'alice');
  return dm;
}

/** A 1 s, 48 kHz, 16-bit mono WAV of a 660 Hz tone, built in the page. */
export async function buildWav(page: Page): Promise<Buffer> {
  const b64 = await page.evaluate(() => {
    const rate = 48_000;
    const samples = rate;
    const buf = new ArrayBuffer(44 + samples * 2);
    const v = new DataView(buf);
    const ascii = (at: number, s: string) => { for (let i = 0; i < s.length; i++) v.setUint8(at + i, s.charCodeAt(i)); };
    ascii(0, 'RIFF'); v.setUint32(4, 36 + samples * 2, true); ascii(8, 'WAVE');
    ascii(12, 'fmt '); v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
    v.setUint32(24, rate, true); v.setUint32(28, rate * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
    ascii(36, 'data'); v.setUint32(40, samples * 2, true);
    for (let i = 0; i < samples; i++) v.setInt16(44 + i * 2, Math.round(Math.sin((2 * Math.PI * 660 * i) / rate) * 0.4 * 32767), true);
    const bytes = new Uint8Array(buf);
    let bin = '';
    for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
    return btoa(bin);
  });
  return Buffer.from(b64, 'base64');
}

/** axe (serious and critical, target-size on) in the three themes, then at 360×740 in the first; restores both. */
export async function axeEverywhere(page: Page, state: string): Promise<void> {
  const viewport = page.viewportSize();
  const theme = await page.evaluate(() => document.documentElement.dataset.theme ?? null);
  try {
    for (const name of ['signal', 'light', 'high-contrast'] as const) {
      await page.evaluate((n) => { document.documentElement.dataset.theme = n; }, name);
      await expectAccessible(page, `${state} in the ${name} theme`);
    }
    await page.evaluate((n) => { document.documentElement.dataset.theme = n; }, 'signal');
    await page.setViewportSize({ width: 360, height: 740 });
    await expectAccessible(page, `${state} at 360×740`);
  } finally {
    if (viewport !== null) await page.setViewportSize(viewport);
    await page.evaluate((n) => { if (n === null) delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = n; }, theme);
  }
}

/**
 * Opens the DM `dmId` from the `@` tile's list, by its row's name (the other person's), once the page lists it, and waits until
 * this device has joined the DM's group (the composer is enabled only while the group is `active`). A DM is listed as
 * soon as its Welcome is seen, while its group is still `none`, and the worker drops a typing frame of a group that is
 * not active (controller.ts incomingTypingFrame): a one-shot signal sent before the join is lost for good.
 */
export async function openDm(page: Page, dmId: string, name: string): Promise<void> {
  await homeTile(page).click();
  await namedRow(dmsNav(page), name).click();
  await expect(page).toHaveURL(new RegExp(`/dm/${dmId}$`), { timeout: WAIT });
  await expect(dmComposer(page)).toBeEnabled({ timeout: WAIT });
}
/**
 * Ruling Z (Jonas 2026-10-27): the open DM's `call` / `join call` is its row's context-menu item (and Control/⌘+Shift+Enter), not a
 * header button. `click()` right-clicks the open DM's row and picks it.
 */
export function stripButton(page: Page, key: 'call.start' | 'call.join'): { click(): Promise<void> } {
  return {
    async click() {
      await dmsNav(page).locator('.d-chrow[aria-current="page"]').click({ button: 'right' });
      await page.getByRole('menuitem', { name: copy(key), exact: true }).click();
    },
  };
}
/** The incoming rings on screen (L-UI-70), each with `data-ring` `incoming` or `busy`. */
export function rings(page: Page): Locator { return page.locator('.d-ring-banner__ring'); }

/** Moves the instance clock `seconds` forward (POST /debug/clock), for good: no native peer joins a call after it. */
export async function advanceClock(seconds: number): Promise<void> {
  instanceClockMoved = `+${seconds} s`;
  const control = process.env.DILLA_TESTKIT_CONTROL;
  const res = await fetch(`${control}/debug/clock`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ seconds }),
  });
  expect(res.status, await res.text()).toBe(204);
}

/**
 * Holds back (`on`) or restores the `participant_left` webhook on its way from LiveKit to the instance
 * (POST /debug/sfu/withhold). A departure withheld while it is on is lost for good: turn it off before the
 * departure that must be seen.
 */
export async function withholdLeft(on: boolean): Promise<void> {
  const control = process.env.DILLA_TESTKIT_CONTROL;
  const res = await fetch(`${control}/debug/sfu/withhold`, {
    method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ event: 'participant_left', on }),
  });
  expect(res.status, await res.text()).toBe(204);
}

/** Settings → Voice; resolves with the dialog. */
export async function openVoiceSettings(page: Page): Promise<Locator> {
  return openSettings(page, 'voice' as SettingsSection);
}

/** Settings → Voice → noise suppression off (the fake microphone's steady tone is what RNNoise removes), then closes. */
export async function noiseOff(page: Page): Promise<void> {
  const dialog = await openVoiceSettings(page);
  const toggle = dialog.getByRole('radiogroup', { name: copy('settings.voice.noise'), exact: true }).getByRole('radio', { name: copy('settings.voice.off'), exact: true });
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await closeSettings(page);
}

/** Settings → Voice → push to talk with `code` as its key (one of PUSH_TO_TALK_KEYS), then closes Settings. */
export async function pushToTalk(page: Page, code: string): Promise<void> {
  const dialog = await openVoiceSettings(page);
  const ptt = dialog.getByRole('radiogroup', { name: copy('settings.voice.mode'), exact: true })
    .getByRole('radio', { name: copy('settings.voice.ptt'), exact: true });
  await ptt.click();
  await expect(ptt).toHaveAttribute('aria-checked', 'true', { timeout: WAIT });
  await dialog.getByRole('button', { name: copy('settings.voice.pttSet'), exact: true }).click();
  // Ruling AX (Signal task 10b fix round 1): while listening the key box says "Press a key…" and the button is Cancel.
  await expect(dialog.locator('.d-voice-settings__kbd')).toHaveText(copy('signalSettings.voice.pttListening'), { timeout: WAIT });
  await expect(dialog.getByRole('button', { name: copy('signalSettings.voice.pttCancel'), exact: true })).toBeFocused();
  await page.keyboard.press(code);
  await expect(dialog.locator('.d-voice-settings__kbd')).toHaveText(code, { timeout: WAIT });
  await closeSettings(page);
}

/** The sidebar row of a voice channel (`voice {name}`). */
export function voiceRow(page: Page, name: string): Locator {
  return page.getByRole('button', { name: copy('voice.open', { name }), exact: true }).first();
}

/** Opens the community, then its text channel, and waits until its composer is shown. */
export async function openText(page: Page, community: string, channel: string): Promise<void> {
  await railItem(page, community).click();
  await namedRow(channelsNav(page), channel).click();
  await expect(page.getByRole('textbox', { name: copy('shell.composer.label', { channel }), exact: true })).toBeVisible({ timeout: WAIT });
}

/** Opens a voice channel's view from the sidebar. On a desktop a click on the row also joins (web3.9, R24), so a spec that
 *  needs the stage before the join reaches it by the route, not through this helper. */
export async function openVoice(page: Page, community: string, name: string): Promise<void> {
  await railItem(page, community).click();
  await voiceRow(page, name).click();
  await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
}

/** Opens a voice channel's stage by its route and does not join (web3.9, R-L1): a click on the row would join on a desktop. The
 *  community comes from the rail tile, the channel from the native peer's setup. */
export async function openVoiceOnly(page: Page, community: string, peer: VoicePeer): Promise<void> {
  const voiceId = (peer.setupDone as { voice_channel_id?: string | null }).voice_channel_id;
  if (typeof voiceId !== 'string') throw new Error('the peer set up no voice channel');
  await railItem(page, community).click();
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}/, { timeout: WAIT });
  const communityId = new URL(page.url()).pathname.split('/')[2] ?? '';
  await page.evaluate(([c, v]) => {
    window.history.pushState(null, '', `/c/${c}/${v}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }, [communityId, voiceId]);
  await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
}

/** The voice dock (flow 06), present while this device is in voice: the channel name over the link stats and three icon buttons; no meter or level text (web3.9 R26). */
export function dock(page: Page): Locator { return page.locator('.dw-voice-dock'); }
/** The call stage or voice view of the open route. */
export function stage(page: Page): Locator { return page.locator('.dw-voice-stage'); }
/** The worker's call phase as the stage of the open route shows it (`data-call-phase`). */
export function phaseOf(page: Page): Locator { return page.locator('[data-call-phase]').first(); }

/** `joinVoice`: opens the voice channel, presses `join voice`, and waits until the call phase is `in` and the page's
 *  session is connected (the dock's `data-phase`; leave and the controls act on a connected session). */
export async function joinVoice(page: Page, community: string, name: string): Promise<void> {
  await railItem(page, community).click();
  // The row's trailing join icon (web3.9) joins without the row's own click; it shows while the row is hovered or focused.
  const row = voiceRow(page, name);
  await row.hover();
  await row.locator('xpath=ancestor::li[1]').getByRole('button', { name: copy('voice.join'), exact: true }).click();
  await expect(page.locator('.dw-voice-stage')).toBeVisible({ timeout: WAIT });
  await connected(page);
}
/** The call phase is `in` and the dock shows a connected session. */
export async function connected(page: Page): Promise<void> {
  await expect(page.locator('[data-call-phase]').first()).toHaveAttribute('data-call-phase', 'in', { timeout: WAIT });
  await expect(dock(page).locator('.d-voice-dock')).toHaveAttribute('data-phase', 'connected', { timeout: WAIT });
}

/** Another person's tile (their camera or avatar, never their screen) on this page's stage, by the name it shows. */
export function tile(page: Page, name: string): Locator {
  return stage(page).locator('.d-call-tile:not([data-own]):not([data-source="screen"])').filter({
    has: page.locator('.d-call-tile__name', { hasText: new RegExp(`^${escapeRegExp(name)}$`) }),
  });
}
/** Another person's screen tile on this page's stage: the tile named `stage.screen` filled with their name (web3.8 R27). */
export function screenTile(page: Page, name: string): Locator {
  return stage(page).locator('.d-call-tile[data-source="screen"]:not([data-own])').filter({
    has: page.locator('.d-call-tile__name', { hasText: new RegExp(`^${escapeRegExp(copy('stage.screen', { name }))}$`) }),
  });
}
/**
 * Task 8c: starts this page's screen share from a channel's call bar: Share opens the share picker, `pick` names the choices
 * to click there by their words (e.g. 'Tab', '720p', '15'), and Go live starts it (the click is getDisplayMedia's activation).
 */
export async function startShare(page: Page, pick: readonly string[] = []): Promise<void> {
  const controls = page.getByRole('toolbar', { name: copy('voice.controls.voice'), exact: true });
  await controls.locator('[data-control="share"]').click();
  const picker = page.getByRole('dialog', { name: copy('callStage.share.title'), exact: true });
  await expect(picker).toBeVisible();
  for (const word of pick) await picker.getByRole('radio', { name: word, exact: true }).click();
  await picker.getByRole('button', { name: copy('callStage.share.go'), exact: true }).click();
  await expect(picker).toHaveCount(0);
}
/** `tileState`: the `data-voice-state` of that tile ('connecting', 'not-heard', 'ok' or 'unverified'). */
export async function tileState(page: Page, name: string): Promise<string | null> {
  return tile(page, name).getAttribute('data-voice-state', { timeout: WAIT });
}

// ---------------------------------------------------------------------------------------------------
// What the recorders saw (peer-browser.ts installRecorders).

type Recorded = {
  slices: Record<string, unknown>;
  keyPath: { messages: number; callKeys: number; findings: string[] };
  tones: { hz: number; state: string }[];
  ringTimers: Set<number>;
  blobs: { created: string[]; revoked: string[] };
};

export async function slice<T>(page: Page, name: string): Promise<T | undefined> {
  return page.evaluate((n) => (window as unknown as { __dw: Recorded }).__dw.slices[n], name) as Promise<T | undefined>;
}
export const callSlice = (page: Page) => slice<CallState>(page, 'call');
export const voiceSlice = (page: Page) => slice<VoiceState>(page, 'voice');
export const ringsSlice = (page: Page) => slice<Ring[]>(page, 'rings');

/** This page's own user and device, from the core worker's `account` slice. */
export async function self(page: Page): Promise<{ user: string; device: string }> {
  let out: { user: string; device: string } | null = null;
  await expect.poll(async () => {
    const a = await slice<{ user: { id: string } | null; deviceId: string | null }>(page, 'account');
    out = a?.user && a.deviceId ? { user: a.user.id, device: a.deviceId } : null;
    return out !== null;
  }, POLL).toBe(true);
  return out as unknown as { user: string; device: string };
}

/** The devices of a channel's live call in this page's voice snapshot (the worker's `voice` slice). */
export async function snapshotDevices(page: Page, channelId: string): Promise<string[]> {
  return ((await voiceSlice(page))?.[channelId]?.participants ?? []).map((p) => p.deviceId).sort();
}

/** Waits until this page's trust slice holds `userId` as changed (a new key no comparison verified yet). */
export async function changedSeen(page: Page, userId: string): Promise<void> {
  await expect.poll(async () => {
    const row = (await slice<TrustSummary[]>(page, 'trust'))?.find((r) => r.userId === userId);
    return row !== undefined && (row.changedUmk !== null || row.blocked === 'E_UMK_CHANGED');
  }, { ...POLL, message: `the trust slice holds ${userId} as changed` }).toBe(true);
}

/** The key-path record: how many `ret` and `slice` messages were scanned, the `callKeys` pushes seen, and findings. */
export async function keyPath(page: Page): Promise<Recorded['keyPath']> {
  return page.evaluate(() => (window as unknown as { __dw: Recorded }).__dw.keyPath);
}

/** `audioRunning`: the page made at least one AudioContext, and every one not closed is running (Firefox needs the
 *  PulseAudio sink headlessly; a suspended context reads silent everywhere). */
export async function audioRunning(page: Page): Promise<void> {
  await expect.poll(() => page.evaluate(() => {
    const ctxs = (window as unknown as { __dw: { contexts: AudioContext[] } }).__dw.contexts.filter((c) => c.state !== 'closed');
    return ctxs.length > 0 && ctxs.every((c) => c.state === 'running') ? 'running' : ctxs.map((c) => c.state).join(',') || 'none';
  }), { ...POLL, message: 'AudioContext.state' }).toBe('running');
}

/** The oscillator tones this page started, with their frequency and context state. */
export async function tones(page: Page): Promise<{ hz: number; state: string }[]> {
  return page.evaluate(() => (window as unknown as { __dw: Recorded }).__dw.tones);
}
/** The ring's pings (1000 Hz) this page played. */
export async function ringPings(page: Page): Promise<number> { return (await tones(page)).filter((t) => t.hz === 1000).length; }
/** Every DM (channel id) the worker's `rings` slice ever listed a ring for on this page. */
export async function ringChannelsSeen(page: Page): Promise<string[]> {
  return page.evaluate(() => [...(window as unknown as { __dw: { ringChannels: Set<string> } }).__dw.ringChannels].sort());
}
/** True while the ring's 2 s timer runs (sounds.ts startRing; cleared by stopRing). */
export async function ringTimerLive(page: Page): Promise<boolean> {
  return page.evaluate(() => (window as unknown as { __dw: Recorded }).__dw.ringTimers.size > 0);
}

/** `notificationsSeen`: each desktop notification the page raised, as `{title, tag, closed}`. */
export async function notificationsSeen(page: Page): Promise<{ title: string; tag: string; closed: boolean }[]> {
  return page.evaluate(() => (window as unknown as { __dillaNotifications: { title: string; tag: string; closed: boolean }[] })
    .__dillaNotifications.map((n) => ({ title: n.title, tag: n.tag, closed: n.closed })));
}

/** Reports the tab as hidden (or visible again) to the page, with a `visibilitychange`. */
export async function setHidden(page: Page, hidden: boolean): Promise<void> {
  await page.evaluate((h) => (window as unknown as { __dwSetHidden(h: boolean): void }).__dwSetHidden(h), hidden);
}

/** The `blob:` URLs the page minted and revoked. */
export async function blobs(page: Page): Promise<{ created: string[]; revoked: string[] }> {
  return page.evaluate(() => (window as unknown as { __dw: Recorded }).__dw.blobs);
}

/** Frames the media worker authenticated and bound to `deviceHex` (`__dillaMediaStats.verified`, N6). It throws when no
 *  media worker answers (none running, or every read failed), so "no worker" never reads as "0 frames" (task-10 review
 *  I3); a poll retries a throw. */
export async function heardCount(page: Page, deviceHex: string): Promise<number> {
  let n: number | null = null;
  for (const wk of mediaWorkers(page)) {
    const v = await wk.evaluate((d) => {
      const s = (globalThis as unknown as { __dillaMediaStats?: { verified: Record<string, number> } }).__dillaMediaStats;
      return s === undefined ? null : s.verified?.[d] ?? 0;
    }, deviceHex).catch(() => null);
    if (v !== null) n = Math.max(n ?? 0, v);
  }
  if (n === null) throw new Error(`no media worker answered for frames of ${deviceHex}`);
  return n;
}
/** `heard`: waits until this page's media worker has verified frames of `deviceHex`. */
export async function heard(page: Page, deviceHex: string): Promise<number> {
  await expect.poll(() => heardCount(page, deviceHex), { ...POLL, message: `frames verified as ${deviceHex}` }).toBeGreaterThan(0);
  return heardCount(page, deviceHex);
}
/** The RMS level above which a remote track counts as sound (the fake microphone sends a steady 1 kHz tone). */
export const AUDIBLE_RMS = 0.005;
/** The loudest time-domain RMS any remote analyser (peer-browser.ts: a remote track that reached a WebAudio source) read
 *  over a short window; 0 when no remote track reached the audio graph at all. */
export async function audibleLevel(page: Page): Promise<number> {
  return page.evaluate(async () => {
    const analysers = (window as unknown as { __dw: { remoteAnalysers: AnalyserNode[] } }).__dw.remoteAnalysers;
    let max = 0;
    for (let i = 0; i < 8; i++) {
      for (const a of analysers) {
        const buf = new Float32Array(a.fftSize);
        a.getFloatTimeDomainData(buf);
        let sum = 0;
        for (const x of buf) sum += x * x;
        max = Math.max(max, Math.sqrt(sum / buf.length));
      }
      await new Promise((r) => setTimeout(r, 50));
    }
    return max;
  });
}
/** `audible` (web3.3): waits until a remote call track plays into this page's audio graph with sound in it. `heard`
 *  proves the media worker decrypted frames; only this proves the page plays them. */
export async function audible(page: Page): Promise<number> {
  let level = 0;
  await expect.poll(async () => { level = await audibleLevel(page); return level; },
    { ...POLL, message: 'a remote call track reached the audio graph with sound' }).toBeGreaterThan(AUDIBLE_RMS);
  return level;
}
/** This page's own encrypted frames (`__dillaMediaStats.encrypted`, every key and slot summed): its own frame clock. */
export async function sentCount(page: Page): Promise<number> {
  let n = 0;
  for (const wk of mediaWorkers(page)) {
    const v = await wk.evaluate(() => {
      const s = (globalThis as unknown as { __dillaMediaStats?: { encrypted: Record<string, Record<string, number>> } }).__dillaMediaStats;
      let sum = 0;
      for (const slots of Object.values(s?.encrypted ?? {})) for (const x of Object.values(slots)) sum += x;
      return sum;
    }).catch(() => 0);
    n = Math.max(n, v);
  }
  return n;
}
/** Waits until `page` has encrypted `frames` more frames than `from` (a frame clock, never a sleep). */
export async function framesPass(page: Page, from: number, frames: number): Promise<number> {
  let now = from;
  await expect.poll(async () => { now = await sentCount(page); return now - from; }, { ...POLL, message: `${frames} own frames encrypted` })
    .toBeGreaterThanOrEqual(frames);
  return now;
}

// ---------------------------------------------------------------------------------------------------
// What a page sends as audio (moved here from join-muted.spec.ts by web3.9 E-AFK, which pins the AFK call's silence with it).

/** The page-side record of `watchAudio`. `captured` counts the microphone captures (a getUserMedia stream with an audio
 *  track) since the watch was installed or last reset. */
interface Watch { pcs: RTCPeerConnection[]; handed: { at: string; enabled: boolean }[]; mics: WeakSet<MediaStreamTrack>; opened: number; captured: number }

/**
 * From here on: every peer connection the page makes, and every audio track handed to one (addTransceiver, addTrack, or a
 * sender's replaceTrack, which LiveKit uses for the noise processor's output and a capture restart), with whether the
 * track was enabled at the moment it was handed over; `opened`, how often a microphone track the page captured actually
 * turned enabled (LiveKit enables the track it publishes; the session must not let it while muted); and `captured`, the
 * microphone captures. A second call on the same page installs nothing again: it empties `handed` and zeroes `opened` and
 * `captured`, so what follows is measured alone (the peer connections and captured tracks seen so far stay watched).
 */
export async function watchAudio(person: { page: Page }): Promise<void> {
  await person.page.evaluate(() => {
    const w = window as unknown as { __watch?: Watch };
    if (w.__watch !== undefined) {
      w.__watch.handed = [];
      w.__watch.opened = 0;
      w.__watch.captured = 0;
      return;
    }
    const rec: Watch = { pcs: [], handed: [], mics: new WeakSet(), opened: 0, captured: 0 };
    w.__watch = rec;
    const enabled = Object.getOwnPropertyDescriptor(MediaStreamTrack.prototype, 'enabled')!;
    Object.defineProperty(MediaStreamTrack.prototype, 'enabled', {
      ...enabled,
      set(this: MediaStreamTrack, v: boolean) {
        const was = enabled.get!.call(this) as boolean;
        enabled.set!.call(this, v);
        if (!was && (enabled.get!.call(this) as boolean) && rec.mics.has(this)) rec.opened += 1;
      },
    });
    const md = navigator.mediaDevices;
    const capture = md.getUserMedia.bind(md);
    md.getUserMedia = async (c?: MediaStreamConstraints) => {
      const stream = await capture(c);
      if (stream.getAudioTracks().length > 0) rec.captured += 1;
      for (const t of stream.getAudioTracks()) rec.mics.add(t);
      return stream;
    };
    const hand = (at: string, track: MediaStreamTrack | null | undefined) => {
      if (track?.kind === 'audio') rec.handed.push({ at, enabled: track.enabled });
    };
    const Native = window.RTCPeerConnection;
    const proto = Native.prototype;
    const addTransceiver = proto.addTransceiver;
    proto.addTransceiver = function (this: RTCPeerConnection, trackOrKind: MediaStreamTrack | string, init?: RTCRtpTransceiverInit) {
      if (typeof trackOrKind !== 'string') hand('addTransceiver', trackOrKind);
      return addTransceiver.call(this, trackOrKind as MediaStreamTrack, init);
    } as typeof proto.addTransceiver;
    const addTrack = proto.addTrack;
    proto.addTrack = function (this: RTCPeerConnection, track: MediaStreamTrack, ...streams: MediaStream[]) {
      hand('addTrack', track);
      return addTrack.call(this, track, ...streams);
    };
    const replaceTrack = RTCRtpSender.prototype.replaceTrack;
    RTCRtpSender.prototype.replaceTrack = function (this: RTCRtpSender, track: MediaStreamTrack | null) {
      hand('replaceTrack', track);
      return replaceTrack.call(this, track);
    };
    class Watched extends Native {
      constructor(config?: RTCConfiguration) { super(config); rec.pcs.push(this); }
    }
    window.RTCPeerConnection = Watched as typeof RTCPeerConnection;
  });
}

/** What the page's open peer connections send as audio now: each audio sender's track ('on', 'off', or 'none'), the
 *  outbound audio packets and bytes summed over every connection, the media-source audio energy, or null where there is
 *  none (Firefox has no media-source `totalAudioEnergy`; a listen-only call has no audio source at all), how many watched
 *  connections are `connected`, and how many outbound audio streams their stats report. */
export async function audioOut(person: { page: Page }): Promise<{
  senders: string[]; packets: number; bytes: number; energy: number | null; connected: number; outbound: number;
}> {
  return person.page.evaluate(async () => {
    const w = window as unknown as { __watch: Watch };
    const senders: string[] = [];
    let packets = 0;
    let bytes = 0;
    let energy: number | null = null;
    let connected = 0;
    let outbound = 0;
    for (const pc of w.__watch.pcs) {
      if (pc.connectionState === 'closed') continue;
      if (pc.connectionState === 'connected') connected += 1;
      for (const t of pc.getTransceivers()) {
        if (t.receiver.track.kind !== 'audio' || t.currentDirection === 'recvonly' || t.direction === 'recvonly' || t.currentDirection === 'stopped') continue;
        const track = t.sender.track;
        senders.push(track === null ? 'none' : track.enabled && track.readyState === 'live' ? 'on' : 'off');
      }
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; packetsSent?: number; bytesSent?: number; totalAudioEnergy?: number }) => {
        if (r.type === 'outbound-rtp' && r.kind === 'audio') { outbound += 1; packets += r.packetsSent ?? 0; bytes += r.bytesSent ?? 0; }
        if (r.type === 'media-source' && r.kind === 'audio' && typeof r.totalAudioEnergy === 'number') energy = (energy ?? 0) + r.totalAudioEnergy;
      });
    }
    return { senders, packets, bytes, energy, connected, outbound };
  });
}

/**
 * A muted microphone still lets Opus send its discontinuous-transmission frames: on chromium about ten a second of some
 * twenty bytes each (a disabled track is silence, and silence is what DTX sends); the encoder's first second may send a
 * little more. A live one with the fake microphone's 1 kHz tone sends about 9 KB a second, with audio energy. The proof
 * that nothing went out is: every hand-off disabled, no captured track ever enabled (`opened`), no audio energy at the
 * sender where the browser measures it, and only DTX-sized frames after a settle period.
 */
const DTX_BYTES_PER_PACKET = 40;
const SETTLE_MS = 1_000;
/** The unmuted control: a live tone sends far more than DTX in half a second. */
const LIVE_BYTES_PER_HALF_S = 1_000;

/** Every audio track she handed over was disabled when it was handed, no captured microphone track ever turned enabled,
 *  each audio sender's track is disabled now, no audio energy has reached a sender, and once settled only DTX frames go
 *  out, over `holdMs` (1 s unless given).
 */
export async function sendsNoAudio(person: { page: Page }, o: { holdMs?: number } = {}): Promise<void> {
  await expect.poll(async () => (await audioOut(person)).senders.join(','), { ...POLL, message: 'her audio senders' }).toMatch(/^(off|none)(,(off|none))*$/);
  const watch = await person.page.evaluate(() => {
    const w = (window as unknown as { __watch: Watch }).__watch;
    return { handed: w.handed, opened: w.opened, captured: w.captured };
  });
  expect(watch.handed.length, 'an audio track was handed to a peer connection').toBeGreaterThan(0);
  expect(watch.handed.filter((h) => h.enabled), 'audio tracks handed over enabled').toEqual([]);
  expect(watch.opened, 'times a microphone track turned enabled').toBe(0);
  const first = await audioOut(person);
  if (first.energy === null) {
    // No media-source energy in this browser (Firefox): the proof rests on the disabled hand-offs and `opened == 0`
    // above, and the frame size below. Said in the report, never checked as a vacuous 0.
    test.info().annotations.push({ type: 'audio-energy', description: 'not reported by this browser; hand-offs, opened and frame size are the proof' });
  } else {
    expect(first.energy, 'audio energy that ever reached her sender').toBe(0);
  }
  await person.page.waitForTimeout(SETTLE_MS);
  const before = await audioOut(person);
  await person.page.waitForTimeout(o.holdMs ?? 1_000);
  const after = await audioOut(person);
  if (after.energy !== null) expect(after.energy, 'audio energy while muted').toBe(0);
  const bytes = after.bytes - before.bytes;
  const packets = after.packets - before.packets;
  expect(packets === 0 ? 0 : bytes / packets, 'bytes per audio packet while muted, once settled (DTX frames only)').toBeLessThanOrEqual(DTX_BYTES_PER_PACKET);
}

/** The microphone is back (after a mute, a rejoin or a device change): since the watch was (re)set it was captured, an
 *  enabled audio track was handed to a peer connection, and an audio sender carries it live. Byte-free, so it holds with
 *  noise suppression on (which silences the fake tone). */
export async function micLive(person: { page: Page }): Promise<void> {
  await expect.poll(async () => {
    const w = await person.page.evaluate(() => {
      const r = (window as unknown as { __watch: Watch }).__watch;
      return { captured: r.captured, enabled: r.handed.filter((h) => h.enabled).length };
    });
    const out = await audioOut(person);
    return w.captured >= 1 && w.enabled >= 1 && out.senders.includes('on');
  }, { ...POLL, message: 'the microphone is captured, handed over enabled and sent' }).toBe(true);
}

/** The control for the measure above: once she unmutes, the tone goes out (live-sized bytes, and energy where reported). */
export async function sendsAudio(person: { page: Page }): Promise<void> {
  await expect.poll(async () => {
    const a = await audioOut(person);
    await person.page.waitForTimeout(500);
    const b = await audioOut(person);
    const louder = a.energy === null || b.energy === null || b.energy > a.energy;
    return louder && b.bytes - a.bytes > LIVE_BYTES_PER_HALF_S;
  }, { ...POLL, message: 'her unmuted microphone sends the tone' }).toBe(true);
}
