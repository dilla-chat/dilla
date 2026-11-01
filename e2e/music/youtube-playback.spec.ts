import { execFileSync } from 'node:child_process';
import { expect, test } from '../voice/support/peer-browser';
import { audible, audibleLevel, AUDIBLE_RMS, callSlice, heardCount, keyPath, joinVoice, self, snapshotDevices, voiceRow, WAIT } from '../voice/support/voice';
import { addServerVia, botRowMenu, channelRow, completeSignUp, composerBox, copy, makeInviteLink, makeRole, newAccount, openChannel, railItem, signUp } from '../web/support/app';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const SERVER = 'Music acceptance A';
const SERVER_TWO = 'Music acceptance B';
const DJ_ROLE = 'Music DJ';
const FRIEND = 'Bea';
const FRIEND_TWO = 'Cal';
const YOUTUBE_SAMPLE = 'https://www.youtube.com/watch?v=aqz-KE-bpKQ';
// Public, two-video playlist. Keep the expected order below so this exercises
// provider expansion and playback of a later imported item, not just URL routing.
const YOUTUBE_PLAYLIST = 'https://www.youtube.com/playlist?list=PLrAXtmErZgOeiKm4sgNOknGvNjby9efdf';
const QUALIFIED_YTDLP = '2026.10.21';

function toolVersion(command: string, args: string[]): string {
  try {
    return execFileSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }).trim().split('\n')[0] ?? '';
  } catch {
    throw new Error(`YouTube music E2E requires ${command} on PATH; install the qualified Bot provider tools first.`);
  }
}

test.beforeAll(() => {
  const ytdlp = toolVersion('yt-dlp', ['--version']);
  if (ytdlp !== QUALIFIED_YTDLP) {
    throw new Error(`YouTube music E2E requires yt-dlp ${QUALIFIED_YTDLP}, found ${ytdlp}. Use experiments/bot-providers/youtube-requirements.txt.`);
  }
  const node = toolVersion('node', ['--version']);
  const nodeMajor = Number(/^v(\d+)/u.exec(node)?.[1]);
  if (!Number.isInteger(nodeMajor) || nodeMajor < 22) throw new Error(`YouTube music E2E requires Node 22 or newer, found ${node}.`);
  const bwrap = toolVersion('bwrap', ['--version']);
  const python = toolVersion('python3', ['--version']);
  const ffmpeg = toolVersion('ffmpeg', ['-version']);
  console.info(`YouTube music E2E provider tools: yt-dlp ${ytdlp}, ${node}, ${bwrap}, ${python}, ${ffmpeg}`);
});

const dialog = (page: import('@playwright/test').Page, name: string, vars?: Record<string, string | number>) =>
  page.getByRole('dialog', { name: copy(name, vars), exact: true });
const settingsNav = (settings: import('@playwright/test').Locator, key: string) =>
  settings.getByRole('navigation', { name: copy('admin.nav'), exact: true })
    .getByRole('button', { name: copy(key), exact: true });
// Signal task 10c: the Music page's YouTube row (its status line and READY tag) and the Spotify account row.
const expectProviderRuntimeEvidence = async (settings: import('@playwright/test').Locator): Promise<void> => {
  const youtube = settings.locator('[data-setting="music-providers"]');
  await expect(youtube.getByRole('status')).toHaveText(copy('serverSettings.music.youtubeReady'), { timeout: 35_000 });
  // The fresh fixture has no Spotify account state, and the pinned provider tools above are present.
  await expect(settings.getByRole('group', { name: copy('workspace.providers.spotifyAccount'), exact: true }))
    .toContainText(copy('workspace.providers.notLinked'));
};
const memberRow = (page: import('@playwright/test').Page, server: string, name: string) =>
  page.getByRole('complementary', { name: copy('members.region', { server }), exact: true })
    .getByRole('listitem').filter({ hasText: name });

test('two communities run isolated YouTube playback with independent Bot identities and SFrame keys', async ({ peerBrowser }) => {
  // The full playback/isolation/restart flow includes many serial authenticated command round trips.
  test.setTimeout(720_000);
  const owner = await peerBrowser.open('owner');
  const dj = await peerBrowser.open('dj');
  const djTwo = await peerBrowser.open('dj-two');
  const page = owner.page;
  const djPage = dj.page;
  const djTwoPage = djTwo.page;
  const sandboxOrigin = new URL(process.env.DILLA_TEST_HOST!).origin;
  let djAuthorization = '';
  djPage.on('request', request => {
    if (!request.url().startsWith(`${sandboxOrigin}/`)) return;
    const authorization = request.headers()['authorization'];
    if (authorization?.startsWith('Bearer ')) djAuthorization = authorization;
  });
  const currentCallGroup = async (channel: string): Promise<string> => {
    expect(djAuthorization !== '', 'DJ has an authenticated sandbox session').toBe(true);
    const response = await djPage.request.get(`${sandboxOrigin}/v1/channels/${channel}/call`, {
      headers: { authorization: djAuthorization, accept: 'application/cbor' },
    });
    expect(response.ok()).toBe(true);
    const group = arr(decode(new Uint8Array(await response.body())))[0];
    expect(group).toBeInstanceOf(Uint8Array);
    return Buffer.from(group as Uint8Array).toString('hex');
  };
  const invite = process.env.DILLA_TESTKIT_INVITE?.split(',')[0];
  if (!invite) throw new Error('global setup did not export the test instance invite');
  const control = process.env.DILLA_TESTKIT_CONTROL;
  if (!control) throw new Error('global setup did not export the test instance control URL');

  const discovery = await fetch(`${process.env.DILLA_TEST_HOST}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(discovery.ok, 'test instance discovery').toBe(true);
  const instanceName = arr(decode(new Uint8Array(await discovery.arrayBuffer())))[5];
  if (typeof instanceName !== 'string' || instanceName === '') throw new Error('test instance has no name');

  const ownerAccount = await signUp(page, invite, instanceName, 'Ada', control);
  await addServerVia(page, 'create');
  const createServer = dialog(page, 'create.server.title');
  await createServer.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
  await page.keyboard.press('Enter');
  await expect(createServer).toBeHidden({ timeout: WAIT });
  await expect(page.getByRole('button', { name: copy('voice.open', { name: 'general' }), exact: true })).toBeVisible({ timeout: WAIT });

  await channelRow(page, 'general').click();

  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  let settings = dialog(page, 'admin.title');
  await settingsNav(settings, 'roles.tab').click();
  // Signal task 10c: DJ is the role's Control music (DJ) permission, set in the inline role editor.
  await makeRole(page, settings, DJ_ROLE, { perms: ['music'] });

  await settingsNav(settings, 'admin.nav.bot').click();
  await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.music'), exact: true }).click();
  await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
  const stepUp = dialog(page, 'factorSettings.stepUp.title');
  await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(ownerAccount.password);
  await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  await expect(settings.getByRole('status').filter({ hasText: copy('admin.bot.status.ready') }))
    .toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });

  await settingsNav(settings, 'workspace.music').click();
  await expectProviderRuntimeEvidence(settings);
  await expect(settings.getByRole('group', { name: copy('serverSettings.music.dj'), exact: true }))
    .toContainText(copy('serverSettings.music.djRoles', { roles: DJ_ROLE }), { timeout: WAIT });
  await page.keyboard.press('Escape');

  const inviteLink = await makeInviteLink(page, SERVER);
  await djPage.goto(new URL(inviteLink).pathname);
  await djPage.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
  await completeSignUp(djPage, newAccount(FRIEND, control), instanceName);
  await expect(djPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
    .getByRole('button', { name: SERVER, exact: true })).toBeVisible({ timeout: WAIT });

  // Create and configure the second community through the shipping client. Its installation is
  // independently managed and receives its own user/device identity and MLS group.
  await addServerVia(page, 'create');
  const createServerTwo = dialog(page, 'create.server.title');
  await createServerTwo.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER_TWO);
  await page.keyboard.press('Enter');
  await expect(createServerTwo).toBeHidden({ timeout: WAIT });
  await expect(page.getByRole('button', { name: copy('voice.open', { name: 'general' }), exact: true })).toBeVisible({ timeout: WAIT });
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  settings = dialog(page, 'admin.title');
  await settingsNav(settings, 'roles.tab').click();
  // Signal task 10c: DJ is the role's Control music (DJ) permission, set in the inline role editor.
  await makeRole(page, settings, DJ_ROLE, { perms: ['music'] });
  await settingsNav(settings, 'admin.nav.bot').click();
  await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.music'), exact: true }).click();
  await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
  const stepUpTwo = dialog(page, 'factorSettings.stepUp.title');
  await stepUpTwo.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(ownerAccount.password);
  await stepUpTwo.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
  await expect(settings.getByRole('status').filter({ hasText: copy('admin.bot.status.ready') }))
    .toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });
  await settingsNav(settings, 'workspace.music').click();
  await expectProviderRuntimeEvidence(settings);
  await expect(settings.getByRole('group', { name: copy('serverSettings.music.dj'), exact: true }))
    .toContainText(copy('serverSettings.music.djRoles', { roles: DJ_ROLE }), { timeout: WAIT });
  await page.keyboard.press('Escape');

  const inviteLinkTwo = await makeInviteLink(page, SERVER_TWO);
  await djTwoPage.goto(new URL(inviteLinkTwo).pathname);
  await djTwoPage.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
  await completeSignUp(djTwoPage, newAccount(FRIEND_TWO, control), instanceName);
  await expect(djTwoPage.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
    .getByRole('button', { name: SERVER_TWO, exact: true })).toBeVisible({ timeout: WAIT });

  // Give each community's DJ role to its local member only.
  for (const [server, name] of [[SERVER, FRIEND], [SERVER_TWO, FRIEND_TWO]] as const) {
    await page.getByRole('navigation', { name: copy('shell.rail.label'), exact: true })
      .getByRole('button', { name: server, exact: true }).click();
    const friend = memberRow(page, server, name);
    await expect(friend).toBeVisible({ timeout: WAIT });
    await friend.locator('.d-member-list__open').click();
    await page.getByRole('dialog', { name, exact: true })
      .getByRole('button', { name: copy('layout.profile.manageRoles'), exact: true }).click();
    const assign = dialog(page, 'roles.assign.title', { name });
    await assign.getByRole('checkbox', { name: DJ_ROLE, exact: true }).click();
    await expect(assign.getByRole('checkbox', { name: DJ_ROLE, exact: true })).toBeChecked({ timeout: WAIT });
    await expect(page.getByTestId('shell-announcer')).toHaveText(copy('roles.assign.given', { role: DJ_ROLE, name }), { timeout: WAIT });
    await assign.getByRole('button', { name: copy('roles.assign.close'), exact: true }).click();
  }

  await joinVoice(djPage, SERVER, 'general');
  await joinVoice(djTwoPage, SERVER_TWO, 'general');
  // Jonas 2026-10-28: results stay private; only a /play that starts the music adds Glenn's one public "started music" message
  // (with the live player card) in the channel where it ran.
  const command = async (musicPage: typeof page, server: string, commandText: string, expected: RegExp, announces = 0) => {
    await openChannel(musicPage, server, 'general');
    const commandId = commandText.slice(1).split(/\s/u, 1)[0] ?? '';
    const argument = commandText.slice(commandId.length + 2).trim();
    const composer = composerBox(musicPage, 'general');
    await composer.fill('/');
    const region = musicPage.getByRole('region', { name: 'Command composer', exact: true });
    const picker = region.getByRole('combobox', { name: 'Command', exact: true });
    await picker.fill(commandId);
    const option = region.getByRole('option').filter({ has: musicPage.getByText(`/${commandId}`, { exact: true }) });
    await expect(option).toBeVisible({ timeout: WAIT });
    await picker.press('ArrowDown');
    await picker.press('Enter');
    if (commandId === 'play') {
      const query = region.getByRole('combobox', { name: 'query-or-url', exact: true });
      await query.fill(argument);
      await expect(query).toHaveValue(argument);
    }
    const log = musicPage.getByRole('log', { name: 'messages in #general', exact: true });
    // A bot-authored row, grouped or not: MessageRow marks its author `data-bot` (the sr-only author of a grouped row too),
    // which also counts the grouped bot rows the old `[data-kind="bot"]` locator missed.
    const publicBotMessages = log.locator('.d-message-row:has(.d-message-row__author[data-bot])');
    const publicCount = await publicBotMessages.count();
    await region.getByRole('button', { name: 'Run command', exact: true }).click();
    const status = region.getByRole('status');
    await expect(status).toContainText(expected, { timeout: 90_000 });
    const result = await status.innerText();
    await expect(publicBotMessages).toHaveCount(publicCount + announces, { timeout: 30_000 });
    if (announces > 0) {
      const announcement = publicBotMessages.last();
      await expect(announcement).toContainText('started music in general');
      await expect(announcement.getByRole('group', { name: 'Music player', exact: true })).toBeVisible();
    }
    await region.getByRole('button', { name: 'Cancel', exact: true }).click();
    await voiceRow(musicPage, 'general').click();
    return result;
  };
  await command(djPage, SERVER, `/play ${YOUTUBE_PLAYLIST}`, /Track added to the music queue\./, 1);
  await command(djTwoPage, SERVER_TWO, `/play ${YOUTUBE_SAMPLE}`, /Track added to the music queue\./, 1);
  await command(djPage, SERVER, `/play ${YOUTUBE_SAMPLE}`, /Track added to the music queue\./);
  const queueA = await command(djPage, SERVER, '/queue', /Now playing:/);
  expect(queueA).toContain('Deep Learning State of the Art (2020)');
  expect(queueA).toContain('Deep Learning Basics: Introduction and Overview');
  const queueB = await command(djTwoPage, SERVER_TWO, '/queue', /Now playing:/);
  expect(queueB).not.toContain('Deep Learning State of the Art (2020)');

  const botDeviceFor = async (musicPage: typeof page) => {
    const botTile = musicPage.locator('.dw-voice-stage .d-call-stage__tile').filter({ has: musicPage.locator('.d-call-tile__tag', { hasText: 'bot' }) });
    await expect(botTile).toHaveCount(1, { timeout: 90_000 });
    const tileID = await botTile.getAttribute('data-tile-id');
    const botDevice = tileID?.split(':')[0] ?? '';
    expect(botDevice).toMatch(/^[0-9a-f]{32}$/);
    return botDevice;
  };
  const requireAudible = async (musicPage: typeof page, botDevice: string,
    previousFrames?: number, tracksBefore?: string[]) => {
    try {
      await audible(musicPage);
    } catch (error) {
      const diagnostic = await musicPage.evaluate(({ rms, verifiedFrames, previousFrames, tracksBefore }) => {
        const windowState = window as unknown as {
          __dw?: { contexts?: AudioContext[]; remoteTracks?: Set<string>; remoteAnalysers?: AnalyserNode[] };
        };
        return {
          rms,
          verifiedFrames,
          contexts: (windowState.__dw?.contexts ?? []).map((context) => context.state),
          remoteTrackIDs: [...(windowState.__dw?.remoteTracks ?? [])].sort(),
          remoteAnalyserCount: windowState.__dw?.remoteAnalysers?.length ?? 0,
          previousFrames,
          framesAddedSincePrevious: previousFrames === undefined ? null : verifiedFrames - previousFrames,
          trackIDsAddedSincePrevious: tracksBefore === undefined ? null :
            [...(windowState.__dw?.remoteTracks ?? [])].filter((trackID) => !tracksBefore.includes(trackID)).sort(),
        };
      }, { rms: await audibleLevel(musicPage), verifiedFrames: await heardCount(musicPage, botDevice), previousFrames,
        tracksBefore });
      throw new Error(`${String(error)}\nSafe audio diagnostic (no IDs or media payloads): ${JSON.stringify(diagnostic)}`);
    }
  };
  const botDeviceA = await botDeviceFor(djPage);
  const botDeviceB = await botDeviceFor(djTwoPage);
  // Separate communities must have separate managed Bot device identities.
  expect(botDeviceA).not.toBe(botDeviceB);
  const callA = await callSlice(djPage);
  const callB = await callSlice(djTwoPage);
  expect(callA?.channelId).not.toBeNull();
  expect(callB?.channelId).not.toBeNull();
  expect(callA?.channelId).not.toBe(callB?.channelId);
  expect(callA?.callId).not.toBe(callB?.callId);
  expect(callA?.epoch).not.toBeNull();
  expect(callB?.epoch).not.toBeNull();
  const djSelf = await self(djPage);
  const djTwoSelf = await self(djTwoPage);
  await expect.poll(() => snapshotDevices(djPage, callA!.channelId!), { timeout: WAIT }).toEqual([botDeviceA, djSelf.device].sort());
  await expect.poll(() => snapshotDevices(djTwoPage, callB!.channelId!), { timeout: WAIT }).toEqual([botDeviceB, djTwoSelf.device].sort());
  // The browser's media worker authenticates this provider's SFrame with the call key; the separate analyser proves the
  // decrypted remote audio reached playback. A UI queue row or an SFU track alone cannot satisfy either assertion.
  let verifiedFrames = 0;
  await expect.poll(async () => {
    verifiedFrames = await heardCount(djPage, botDeviceA);
    return verifiedFrames;
  }, { timeout: 90_000, intervals: [250, 500, 1_000] }).toBeGreaterThan(0);
  await expect.poll(() => heardCount(djTwoPage, botDeviceB), { timeout: 90_000, intervals: [250, 500, 1_000] }).toBeGreaterThan(0);
  await requireAudible(djPage, botDeviceA);
  await requireAudible(djTwoPage, botDeviceB);
  for (const musicPage of [djPage, djTwoPage]) {
    const result = await keyPath(musicPage);
    expect(result.callKeys).toBeGreaterThan(0);
    expect(result.messages).toBeGreaterThan(0);
    expect(result.findings).toEqual([]);
  }

  // Verify the first imported item is actually playing before advancing within
  // the playlist. Then require another authenticated media frame on item two.
  const framesBeforePlaylistSkip = await heardCount(djPage, botDeviceA);
  const tracksBeforePlaylistSkip = await djPage.evaluate(() =>
    [...((window as unknown as { __dw: { remoteTracks: Set<string> } }).__dw.remoteTracks)].sort());
  await command(djPage, SERVER, '/skip', /Skipped the current track/);
  expect(await command(djPage, SERVER, '/queue', /(?:Now playing|Paused):/)).toContain('Deep Learning Basics: Introduction and Overview');
  await expect.poll(() => heardCount(djPage, botDeviceA), { timeout: 90_000, intervals: [250, 500, 1_000] })
    .toBeGreaterThan(framesBeforePlaylistSkip);
  await requireAudible(djPage, botDeviceA, framesBeforePlaylistSkip, tracksBeforePlaylistSkip);

  await command(djPage, SERVER, '/pause', /Music paused/);
  await expect.poll(() => audibleLevel(djPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeLessThan(AUDIBLE_RMS);
  await expect.poll(() => audibleLevel(djTwoPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeGreaterThan(AUDIBLE_RMS);
  await command(djPage, SERVER, '/resume', /Music resumed/);
  await expect.poll(() => audibleLevel(djPage), { timeout: 90_000, intervals: [250, 500, 1_000] }).toBeGreaterThan(AUDIBLE_RMS);
  // Stopping A leaves B's independent queue and audio running.
  await command(djPage, SERVER, '/stop', /Music stopped/);
  await expect.poll(() => audibleLevel(djPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeLessThan(AUDIBLE_RMS);
  await expect.poll(() => audibleLevel(djTwoPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeGreaterThan(AUDIBLE_RMS);
  // End A's old call completely and create a new call group for the same
  // channel. A retained Bot group must not block playback in its replacement.
  const callChannel = (await callSlice(djPage))?.channelId;
  expect(callChannel).toBeTruthy();
  const oldGroup = await currentCallGroup(callChannel!);
  // Stop intentionally retains the Bot's authorized room. End this sandbox call
  // through the current human leaf's public API, rather than assuming it is alone.
  const ended = await djPage.request.delete(`${sandboxOrigin}/v1/calls/${callChannel}`, {
    headers: { authorization: djAuthorization },
  });
  expect(ended.status()).toBe(204);
  await expect.poll(() => snapshotDevices(djPage, callChannel!), { timeout: WAIT }).toEqual([]);
  await joinVoice(djPage, SERVER, 'general');
  expect(await currentCallGroup(callChannel!)).not.toBe(oldGroup);
  await command(djPage, SERVER, '/resume', /Music resumed/);
  await expect.poll(() => audibleLevel(djPage), { timeout: 90_000, intervals: [250, 500, 1_000] }).toBeGreaterThan(AUDIBLE_RMS);

  // Disable only community A's Bot through its owner controls. Community B must keep its
  // authenticated Bot stream and audible output while A's stream disappears.
  await railItem(page, SERVER).click();
  await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
  settings = dialog(page, 'admin.title');
  await settingsNav(settings, 'admin.nav.bot').click();
  await botRowMenu(page, settings, 'serverSettings.bot.menu.disable');
  await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.disabled') }))
    .toHaveText(copy('admin.bot.status.disabled'), { timeout: WAIT });
  await expect(settings.getByRole('status').filter({ hasText: copy('admin.bot.disabledNotice') }))
    .toHaveText(copy('admin.bot.disabledNotice'), { timeout: WAIT });
  await expect.poll(() => snapshotDevices(djPage, callA!.channelId!), { timeout: 90_000 }).not.toContain(botDeviceA);
  await expect.poll(() => audibleLevel(djPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeLessThan(AUDIBLE_RMS);
  await expect.poll(() => heardCount(djTwoPage, botDeviceB), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeGreaterThan(0);
  await expect.poll(() => audibleLevel(djTwoPage), { timeout: WAIT, intervals: [250, 500, 1_000] }).toBeGreaterThan(AUDIBLE_RMS);

  await command(djTwoPage, SERVER_TWO, '/stop', /Music stopped/);

  // Confirm the human's E2EE worker observed authenticated Bot media and that its IPC boundary never exposed a call key.
  expect(await djPage.locator('.dw-voice-stage .d-call-tile[data-own="true"]').count()).toBe(1);
  expect(await djTwoPage.locator('.dw-voice-stage .d-call-tile[data-own="true"]').count()).toBe(1);
});
