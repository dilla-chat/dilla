import { expect, type Locator } from '@playwright/test';
import { advanceClock, composerBox, copy, expectAccessible, closeSettings, instanceInvite, joinCommunity, openChannel, openSettings, Peer, rowById, settingsDialog, signUp, WAIT } from './support/app';
import { test } from './support/second';

test('native edit is searched by prefix and deleted text disappears after a positive result', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const first = await peer.send('old lantern');
  await expect(rowById(page, peer.channelName, first.msg_id)).toContainText('old lantern', { timeout: WAIT });
  await peer.edit(first.msg_id, 'café lamp');
  await expect(rowById(page, peer.channelName, first.msg_id)).toContainText('café lamp', { timeout: WAIT });
  await page.keyboard.press('ControlOrMeta+K');
  const search = page.getByRole('combobox', { name: copy('search.placeholder') });
  await search.fill('cafe');
  await expect(page.getByRole('option', { name: /café lamp/ })).toBeVisible({ timeout: WAIT });
  // J1, R-S4: a desktop search is a centred overlay of at most 40rem over a shell that stays in the DOM, not a full-screen page.
  const overlay = page.locator('dialog.d-search-palette-dialog');
  const box = await overlay.boundingBox();
  const viewport = page.viewportSize();
  if (box === null || viewport === null) throw new Error('no search overlay box');
  expect(box.width).toBeLessThanOrEqual(641);
  expect(box.x).toBeGreaterThan(100);
  expect(box.x + box.width / 2).toBeCloseTo(viewport.width / 2, 0);
  await expect(page.locator('dialog.dw-search')).toHaveCount(0);
  await expect(rowById(page, peer.channelName, first.msg_id)).toBeAttached();
  await expect(search).toBeFocused();
  await page.getByRole('option', { name: /café lamp/ }).click();
  await expect(rowById(page, peer.channelName, first.msg_id)).toBeFocused();
  await peer.deleteMessage(first.msg_id, first.seq);
  const later = await peer.send('lamp after delete');
  await expect(rowById(page, peer.channelName, later.msg_id)).toContainText('lamp after delete', { timeout: WAIT });
  await page.keyboard.press('ControlOrMeta+K');
  await page.getByRole('combobox', { name: copy('search.placeholder') }).fill('lamp');
  await expect(page.getByRole('option', { name: /lamp after delete/ })).toBeVisible({ timeout: WAIT });
  await expect(page.getByRole('option', { name: /café lamp/ })).toHaveCount(0);
  await expectAccessible(page, 'search after delete');
});

test('Privacy, Keyboard and Diagnostics open on a phone with the nav as chips and close with Escape', async ({ page, peer, instanceName }) => {
  await page.setViewportSize({ width: 360, height: 740 });
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const tabs = page.getByRole('navigation', { name: copy('nav.tabs.label'), exact: true });
  for (const section of ['privacy', 'keyboard', 'diagnostics'] as const) {
    const dialog = await openSettings(page, section);
    await expect(dialog).toBeVisible({ timeout: WAIT });
    await expect(page).toHaveURL(new RegExp(`/settings/${section}$`), { timeout: WAIT });
    // Task 10 (README §5.10): a phone section shows the nav as a row of chips above it, and no Back button.
    await expect(dialog.getByRole('button', { name: copy('workspace.back'), exact: true })).toHaveCount(0);
    await expect(dialog.getByRole('navigation', { name: copy('settings.nav.label'), exact: true })).toBeVisible();
    await expect(dialog.locator(`.d-settings-nav__item[aria-current="page"]`)).toBeInViewport();
    await expect(page.locator('.d-app-shell').getByRole('navigation', { name: copy('nav.tabs.label'), exact: true })).toHaveCount(0);
    await expectAccessible(page, `${section} at 360px`);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth)).toBe(true);
    await page.keyboard.press('Escape');
    await expect(dialog).toBeHidden({ timeout: WAIT });
    expect(await page.evaluate(() => document.activeElement !== document.body)).toBe(true);
  }
});

// Signal task 10 fix round 1 (Jonas): settings search moved from the workspace to the ⌘K palette.
test('the search palette finds a setting and opens its section', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  await page.keyboard.press('ControlOrMeta+k');
  const query = page.getByRole('combobox', { name: copy('search.placeholder'), exact: true });
  await query.fill('microphone test');
  const result = page.getByRole('option', { name: new RegExp(`${copy('workspace.group.you')} / ${copy('workspace.voice')}`) }).first();
  await expect(result).toBeVisible({ timeout: WAIT });
  await result.click();
  await expect(page).toHaveURL('/settings/voice', { timeout: WAIT });
  const settings = page.getByRole('dialog', { name: copy('settings.title'), exact: true });
  await expect(settings.getByRole('heading', { name: copy('settings.voice.section'), exact: true })).toBeVisible();
  await closeSettings(page);
});

test('Appearance controls fit desktop rows and the phone section header keeps its title readable', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const settings = await openSettings(page, 'appearance');
  for (const width of [1440, 1024, 390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    const group = settings.getByRole('radiogroup', { name: copy('appearance.theme.label'), exact: true });
    // matchMedia updates the React phone layout after the viewport resize event.
    await expect.poll(async () => new Set(await group.getByRole('radio').evaluateAll(
      elements => elements.map(el => Math.round(el.getBoundingClientRect().y)),
    )).size, { message: `theme option rows at ${width}px` }).toBe(width >= 1024 ? 1 : 2);
    const overflow = await settings.locator('.d-settings-frame__body').evaluate(el => el.scrollWidth > el.clientWidth);
    expect(overflow, `settings overflow at ${width}px`).toBe(false);
    if (width < 768) {
      // Task 10 (README §5.10): the nav's chips name the section; the section's own title is in the content.
      await expect(settings.locator('.d-settings-nav__item[aria-current="page"]')).toHaveText(copy('workspace.appearance'));
      await expect(settings.getByRole('heading', { level: 2, name: copy('appearance.title'), exact: true })).toBeVisible();
      await expect(settings.locator('.d-settings-frame__activity')).toHaveAttribute('data-visible', 'false');
      expect((await settings.locator('.d-settings-frame__bar').boundingBox())!.height,
        'idle phone headers do not reserve a second activity row').toBeLessThanOrEqual(72);
    }
  }
  await closeSettings(page);
});

test('a dirty Profile draft survives Keep editing and is discarded only after confirmation', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const settings = await openSettings(page, 'profile');
  const display = settings.getByRole('textbox', { name: copy('profile.display.label'), exact: true });
  // Task 11 (Q7): Profile saves as you type, so only a change it cannot save (here a character dilla refuses) holds it.
  const DRAFT = `Unsaved${String.fromCodePoint(0x09)}draft`;
  await display.fill(DRAFT);
  await expect(settings.locator('.d-settings-frame__footer')).toContainText('Unsaved changes');
  const nav = settings.getByRole('navigation', { name: copy('settings.nav.label'), exact: true });
  await nav.getByRole('button', { name: copy('workspace.signin'), exact: true }).click();
  let prompt = page.getByRole('dialog', { name: 'Discard unsaved changes?', exact: true });
  await expect(prompt).toBeVisible({ timeout: WAIT });
  await prompt.getByRole('button', { name: 'Keep editing', exact: true }).click();
  await expect(page).toHaveURL('/settings/profile', { timeout: WAIT });
  await expect(display).toHaveValue(DRAFT);
  await nav.getByRole('button', { name: copy('workspace.signin'), exact: true }).click();
  prompt = page.getByRole('dialog', { name: 'Discard unsaved changes?', exact: true });
  await expect(prompt).toBeVisible({ timeout: WAIT });
  await prompt.getByRole('button', { name: 'Discard changes', exact: true }).click();
  await expect(page).toHaveURL('/settings/signin', { timeout: WAIT });
  await expect(settings.getByRole('heading', { name: copy('workspace.signin'), exact: true })).toBeFocused();
  await closeSettings(page);
});

test('scopes and keyboard selection stay on the textbox until a target is chosen', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  const sent = await peer.send('café compass');
  await expect(rowById(page, peer.channelName, sent.msg_id)).toContainText('café compass', { timeout: WAIT });
  // Escape closes the overlay and returns focus to where search was opened.
  await rowById(page, peer.channelName, sent.msg_id).focus();
  await page.keyboard.press('ControlOrMeta+/');
  await expect(page.getByRole('combobox', { name: copy('search.placeholder') })).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('combobox', { name: copy('search.placeholder') })).toHaveCount(0);
  await expect(rowById(page, peer.channelName, sent.msg_id)).toBeFocused();
  await page.keyboard.press('ControlOrMeta+/');
  const textbox = page.getByRole('combobox', { name: copy('search.placeholder') });
  await textbox.fill('cafe');
  // Task 9 fix round 1 (Jonas): the palette has no scope control; messages are searched everywhere, this conversation's first.
  await expect(page.getByRole('button', { name: 'this conversation' })).toHaveCount(0);
  await expect(page.getByRole('option', { name: /café compass/ })).toBeVisible({ timeout: WAIT });
  await page.keyboard.press('ArrowDown');
  await expect(textbox).toBeFocused();
  await expect(page.getByRole('option', { name: /café compass/ })).toHaveAttribute('aria-selected', 'true');
  await page.keyboard.press('Enter');
  await expect(rowById(page, peer.channelName, sent.msg_id)).toBeFocused();
});

test('typing is audience-scoped, expires by server time and is not replayed', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  await peer.typing(setup.channel_id, true);
  await expect(page.getByRole('status').filter({ hasText: /is typing/ })).toBeVisible({ timeout: WAIT });
  const outsider = await Peer.start();
  try {
    await outsider.setup({});
    await outsider.register();
    await expect(outsider.typing(setup.channel_id, true)).rejects.toThrow(/E_NOT_FOUND/);
    await expect(page.getByRole('status').filter({ hasText: /is typing/ })).toHaveCount(1);
  } finally { await outsider.close(); }
  await advanceClock(11);
  await expect(page.getByRole('status').filter({ hasText: /is typing/ })).toHaveCount(0, { timeout: WAIT });
  await page.reload();
  await expect(page.getByRole('status').filter({ hasText: /is typing/ })).toHaveCount(0);
});

// WB product walk I1: leaving a channel with an unsent draft is normal; no error banner, no refused typing signal.
test('switching channel with an unsent draft shows no error banner', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({ channels: 2 });
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  for (let i = 0; i < 3; i += 1) {
    await composerBox(page, peer.channelName).click();
    await page.keyboard.type('half a thought');
    await openChannel(page, peer.communityName, peer.channelNames[1]);
    await openChannel(page, peer.communityName, peer.channelName);
  }
  await page.waitForTimeout(500);
  await expect(page.getByText(/E_NOT_READY|That did not work/)).toHaveCount(0);
});

test('desktop, phone and reflow keep controls accessible in three themes', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await peer.waitForDevices(2, setup.channel_id);
  for (const viewport of [{ width: 1280, height: 800 }, { width: 360, height: 740 }, { width: 320, height: 740 }]) {
    await page.setViewportSize(viewport);
    for (const theme of ['signal', 'light', 'high-contrast']) {
      await page.evaluate(value => { document.documentElement.dataset.theme = value; }, theme);
      await expectAccessible(page, `${theme} ${viewport.width}px`);
      const layout = await page.evaluate(() => ({
        overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        small: [...document.querySelectorAll('button, input, [role="button"]')].filter(el => {
          const r = el.getBoundingClientRect();
          return r.width > 0 && r.height > 0 && (r.width < 24 || r.height < 24);
        }).length,
      }));
      expect(layout).toEqual({ overflow: false, small: 0 });
    }
  }
});

// I1, I2: labels and controls share aligned columns; a long danger label grows its button, it never overprints.
test('settings rows align their controls, and the sign-out button holds its text', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup({});
  await peer.register();
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, peer.channelName);
  await page.setViewportSize({ width: 1440, height: 900 });
  const columns = async (dialog: Locator) => dialog.locator('.d-settings-row__control').evaluateAll(els => els.map(el => {
    const box = el.getBoundingClientRect();
    return { x: Math.round(box.x), width: Math.round(box.width), right: Math.round(box.right) };
  }));
  let dialog = await openSettings(page, 'devices');
  await dialog.getByRole('navigation', { name: copy('settings.nav.label'), exact: true }).getByRole('button', { name: copy('factorSettings.nav'), exact: true }).click();
  await expect(dialog.locator('.d-settings-row').first()).toBeVisible({ timeout: WAIT });
  const signin = await columns(dialog);
  expect(signin.length).toBeGreaterThanOrEqual(4);
  // Task 10 (README §5.9): each row's control sits at the row's right edge, so the controls share one right x.
  expect(new Set(signin.map(c => c.right)).size, `one control right edge in Sign-in: ${JSON.stringify(signin)}`).toBe(1);
  await closeSettings(page);
  dialog = await openSettings(page, 'devices');
  // Signal task 10 fix round 1: Sign out is on this browser's own device row (README §5.9).
  const out = dialog.locator('.d-device-row__actions').getByRole('button', { name: copy('workspace.signOut'), exact: true }).first();
  await expect(out).toBeVisible({ timeout: WAIT });
  const fits = await out.evaluate(el => el.scrollHeight <= el.clientHeight + 1 && el.getBoundingClientRect().right <= (el.closest('.d-device-row')?.getBoundingClientRect().right ?? 0) + 1);
  expect(fits, 'the sign-out label is inside its button and its column').toBe(true);
  await closeSettings(page);
  // M-3: every pair of rows is parted by exactly one hairline (Signal task 10: the Voice page is groups, not rows, so
  // Notifications' rows are measured). Fix round 2: the hairline is the lower row's top edge (the prototype's 56px rows,
  // 56n + 2 per group), and the group keeps its outer edge.
  dialog = await openSettings(page, 'notifications');
  await expect(dialog.locator('.d-settings-row').first()).toBeVisible({ timeout: WAIT });
  const edges = await dialog.locator('.d-settings-row').evaluateAll(els => els.map(el => {
    const cs = getComputedStyle(el);
    const box = el.getBoundingClientRect();
    return { top: parseFloat(cs.borderTopWidth), bottom: parseFloat(cs.borderBottomWidth), y: box.top, end: box.bottom };
  }));
  expect(edges.length).toBeGreaterThan(1);
  for (let i = 1; i < edges.length; i += 1) {
    expect(edges[i - 1].bottom + edges[i].top, `one hairline between rows ${i} and ${i + 1}`).toBe(1);
    // Fix round 3: the rows touch (no section spacing between them), so the group is one closed box.
    expect(Math.abs(edges[i].y - edges[i - 1].end), `row ${i + 1} starts where row ${i} ends`).toBeLessThanOrEqual(0.5);
  }
  expect(edges[0].top, 'the group\'s top edge').toBe(1);
  expect(edges.at(-1)!.bottom, 'the group\'s bottom edge').toBe(1);
  await closeSettings(page);
});
