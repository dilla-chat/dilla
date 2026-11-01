import { expect } from '@playwright/test';
import { nextCode } from './support/totp';
import {
  CLASS, COPY, TEST_TIMEOUT, WAIT, channelRow, channelsNav, composerBox, copy, expectComposerReady, expectShell,
  closeSettings, heading, instanceInvite, logRegion, messageRow, nextButton, openSettings, railItem, railNav, submitButton, tabTo, tag, test, newAccount, escapeRegExp,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

test('the messaging flow runs on the keyboard alone, in the fixed focus order', async ({ page, peer, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  const press = (key: string) => page.keyboard.press(key);

  // Onboarding: every step is a form, submitted with Enter on its focused forward button (L-COPY-01).
  await page.goto(`/welcome?invite=${encodeURIComponent(instanceInvite())}`);
  await expect(heading(page, COPY.connectTitle, { instance: instanceName })).toBeVisible({ timeout: WAIT });
  await tabTo(page, nextButton(page));
  await press('Enter');
  await expect(heading(page, COPY.identityTitle)).toBeFocused({ timeout: WAIT });
  await tabTo(page, page.getByLabel(copy(COPY.usernameLabel), { exact: true }));
  await page.keyboard.type(`k${setup.user_id.slice(0, 8)}`);
  await tabTo(page, page.getByLabel(copy(COPY.displayLabel), { exact: true }));
  await page.keyboard.type('keyboard tester');
  await tabTo(page, page.getByLabel(copy(COPY.passwordLabel), { exact: true }));
  await page.keyboard.type('e2e-password-1234');
  await tabTo(page, nextButton(page));
  await press('Enter');
  await expect(heading(page, COPY.keysTitle)).toBeFocused({ timeout: WAIT });
  const acknowledge = page.getByRole('checkbox', { name: copy(COPY.keysAcknowledge), exact: true });
  await tabTo(page, acknowledge);
  await press('Space');
  await expect(acknowledge).toBeChecked();
  await tabTo(page, nextButton(page));
  await press('Enter');
  await expect(heading(page, COPY.deviceTitle)).toBeFocused({ timeout: WAIT });
  await tabTo(page, submitButton(page));
  await press('Enter');
  // web3.6 step 5, by keyboard: the authenticator app is the default choice.
  const account = newAccount();
  await expect(heading(page, 'factor.choose.title')).toBeFocused({ timeout: WAIT });
  await tabTo(page, page.getByRole('radio', { name: new RegExp(escapeRegExp(copy('factor.choose.app'))) }));
  await tabTo(page, page.getByRole('button', { name: copy('factor.choose.next'), exact: true }));
  await press('Enter');
  await expect(heading(page, 'factor.totp.title')).toBeFocused({ timeout: WAIT });
  const setupKey = page.getByLabel(copy('factor.totp.key'), { exact: true });
  await expect(setupKey).toHaveValue(/^[A-Z2-7]{16,}$/, { timeout: WAIT });
  account.totpSecret = await setupKey.inputValue();
  await tabTo(page, page.getByLabel(copy('factor.totp.code'), { exact: true }));
  await page.keyboard.type(await nextCode(account.control, account));
  await tabTo(page, page.getByRole('button', { name: copy('factor.totp.submit'), exact: true }));
  await press('Enter');
  await expect(heading(page, 'factor.codes.title')).toBeFocused({ timeout: WAIT });
  const ackCodes = page.getByRole('checkbox', { name: copy('factor.codes.ack'), exact: true });
  await tabTo(page, ackCodes);
  await press('Space');
  await expect(ackCodes).toBeChecked();
  await tabTo(page, page.getByRole('button', { name: copy('factor.codes.next'), exact: true }));
  await press('Enter');
  await expect(heading(page, COPY.doneTitle)).toBeFocused({ timeout: WAIT });
  await tabTo(page, page.getByRole('button', { name: copy(COPY.finish), exact: true }));
  await press('Enter');
  await expectShell(page);

  // Join the peer's server through the rail's "+" tile: the rail's one tab stop while it has no server is the @ tile, and
  // ArrowDown reaches the "+" tile; its dialog opens on the first choice, "join a server", then the join dialog.
  const rail = railNav(page);
  const addTile = rail.getByRole('button', { name: copy('layout.rail.add'), exact: true });
  await tabTo(page, rail.getByRole('button', { name: /^direct messages/ }));
  await press('ArrowDown');
  await expect(addTile).toBeFocused();
  await press('Enter');
  const addDialog = page.getByRole('dialog', { name: copy('layout.add.title'), exact: true });
  await expect(addDialog.getByRole('button', { name: copy('layout.add.join'), exact: true })).toBeFocused({ timeout: WAIT });
  await press('Enter');
  const dialog = page.getByRole('dialog', { name: copy(COPY.joinTitle), exact: true });
  await expect(dialog).toBeVisible({ timeout: WAIT });
  await tabTo(page, dialog.getByLabel(copy(COPY.joinInvite), { exact: true }));
  await page.keyboard.type(setup.invite_code);
  await tabTo(page, dialog.getByRole('button', { name: copy(COPY.joinSubmit), exact: true }));
  await press('Enter');
  await expect(dialog).toBeHidden({ timeout: WAIT });

  // Rail and channel list by their single tab stops (the active item, else the first).
  // The rail remembers the last tile focused: the "+" tile, one above the server's.
  await tabTo(page, addTile);
  await press('ArrowUp');
  await expect(railItem(page, peer.communityName)).toBeFocused();
  await press('Enter');
  await tabTo(page, channelsNav(page).getByRole('button', { name: channel }));
  await press('Enter');
  await expectComposerReady(page, channel);

  // Shift+Enter breaks the line, Enter sends.
  const first = `line one ${tag()}`;
  await tabTo(page, composerBox(page, channel));
  await page.keyboard.type(first);
  await press('Shift+Enter');
  await page.keyboard.type('line two');
  await press('Enter');
  await expect(messageRow(page, channel, first)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const received = await peer.waitFor(`${first}\nline two`);
  expect(received.tier).toBe(1);

  // Task 7 (Signal §5.5): the composer's tools sit in the footer under the textarea, so Shift+Tab goes straight into the
  // newest row. Tab enters its toolbar; Escape returns to the row, then to the composer.
  await expect(composerBox(page, channel)).toBeFocused();
  await press('Shift+Tab');
  const newest = logRegion(page, channel).locator(CLASS.messageRow).last();
  await expect(newest).toBeFocused();
  // MP7: the author name is the row's first inner stop (this is the first message of the run, so it is never grouped), then the toolbar.
  await press('Tab');
  await expect(newest.locator('.d-message-row__author-open')).toBeFocused();
  await press('Tab');
  const toolbar = newest.getByRole('toolbar', { name: copy('shell.message.toolbar'), exact: true });
  await expect(toolbar).toBeVisible();
  await expect(toolbar.locator(':focus')).toHaveCount(1);
  await press('Escape');
  await expect(newest).toBeFocused();
  await press('Escape');
  await expect(composerBox(page, channel)).toBeFocused();

  // Signal §5.5 (Ruling X, Jonas 2026-10-27): the header has exactly the prototype's Threads and Members; no "more" menu (pins and
  // edit channel are in the channel row's context menu, search is the top bar's).
  const bar = page.locator(CLASS.channelHeader);
  await expect(bar.getByRole('button', { name: copy('layout.header.threads'), exact: true })).toBeVisible();
  await expect(bar.getByRole('button', { name: /^Members/ })).toBeVisible();
  await expect(bar.getByRole('button')).toHaveCount(2);
  for (const key of ['layout.header.more', 'shell.pins.open', 'search.open']) await expect(bar.getByRole('button', { name: copy(key), exact: true }), key).toHaveCount(0);
  // Ruling X: Shift+F10 on a focused channel row opens its context menu (pins); Escape gives focus back to the row.
  const row = channelRow(page, channel);
  await row.focus();
  await press('Shift+F10');
  const rowMenu = page.getByRole('menu', { name: copy('nav.channel.menuOf', { name: channel }), exact: true });
  await expect(rowMenu).toBeVisible();
  await expect(rowMenu.getByRole('menuitem', { name: copy('shell.pins.open'), exact: true })).toBeFocused();
  await press('Escape');
  await expect(rowMenu).toHaveCount(0);
  await expect(row).toBeFocused();
  // One Tab lap from the skip link, in the order the Global Constraints fix.
  const regions = [
    page.getByRole('link', { name: copy(COPY.skipLink), exact: true }),   // 0 skip link
    rail,                                                                   // 1 community rail
    channelsNav(page),                                                      // 2 channel list
    page.locator('.d-user-panel'),                                          // 3 user panel (web3.8, item 13)
    page.locator(CLASS.channelHeader),                                      // 4 channel header
    logRegion(page, channel),                                               // 5 message log
    page.locator(CLASS.composer),                                           // 6 composer
  ];
  // web3.8 (R4): the desktop has no status bar, so the lap ends at the composer.
  await expect(page.getByRole('region', { name: copy(COPY.statusBarLabel), exact: true })).toHaveCount(0);
  const regionOfFocus = async (): Promise<number> => {
    for (let i = 0; i < regions.length; i++) {
      const inside = await regions[i].evaluateAll((els) =>
        els.some((el) => el === document.activeElement || el.contains(document.activeElement)));
      if (inside) return i;
    }
    return -1;
  };
  const skip = regions[0];
  for (let i = 0; i < 40 && !(await skip.evaluate(el => el === document.activeElement)); i++) {
    await press('Shift+Tab');
  }
  await expect(skip).toBeFocused();
  const seen: number[] = [0];
  // 20 Tabs (Signal: the top bar adds its search, bell and gear before the rail, outside every region): the channel header carries a search button (walk 1); the sidebar splitter is its own stop between the channel
  // list and the user panel (web3.8, item 10), and the panel is two stops (the name block, which opens the status menu; its
  // mute and deafen toolbar; Signal §5.4: no gear, the top bar holds it) before the header; the lap ends at the composer
  // (web3.8, R4: no status bar).
  const splitter = page.getByRole('separator', { name: copy('pane.resize.sidebar'), exact: true });
  let splitterAt = -1;
  for (let i = 0; i < 20; i++) {
    await press('Tab');
    if (await splitter.evaluate(el => el === document.activeElement)) splitterAt = i;
    seen.push(await regionOfFocus());
  }
  // seen[0] is the skip link, so the Tab that landed on the splitter is seen[splitterAt + 1].
  expect(splitterAt, 'the sidebar splitter is a tab stop').toBeGreaterThan(0);
  expect(seen[splitterAt + 1], 'the splitter sits outside every region').toBe(-1);
  expect(seen.slice(0, splitterAt + 1), 'the splitter comes after the channel list').toContain(2);
  expect(seen[splitterAt + 2], 'the stop after the splitter is the user panel\'s name block (the status menu)').toBe(3);
  expect(seen[splitterAt + 3], 'the toolbar is the panel\'s second stop').toBe(3);
  expect(seen[splitterAt + 4], 'the stop after the panel is in the channel header').toBe(4);
  const start = seen.indexOf(0);
  expect(start, `the skip link is a tab stop (regions seen: ${seen.join(' ')})`).toBeGreaterThanOrEqual(0);
  const lap: number[] = [];
  const stops = new Map<number, number>();
  let previous = -2;
  for (const region of seen.slice(start)) {
    if (region === 0 && lap.length > 1) break;
    if (region === -1) { previous = -1; continue; }
    if (region !== previous) lap.push(region);
    stops.set(region, (stops.get(region) ?? 0) + 1);
    previous = region;
  }
  expect(lap, `focus order ${lap.join(' > ')}`).toEqual([...lap].sort((a, b) => a - b));
  expect(new Set(lap).size, `each region entered once: ${lap.join(' > ')}`).toBe(lap.length);
  for (const required of [0, 1, 2, 3, 5, 6]) expect(lap).toContain(required);
  // Web3.8 (WS8): the server's one action is the settings gear beside its name, a stop before the tabs ("leave server" lives
  // in settings, "invite link" under settings → Invites, and the "+" is the owner's). The splitter sits outside the channel
  // list, so it is not counted here.
  // web3.9: the sidebar tabs are gone (the @ tile holds direct messages), so the sidebar is the gear and the rows.
  expect(stops.get(2), 'the sidebar is two tab stops: the "server settings" gear and the rows').toBe(2);
  expect(stops.get(1), 'the rail is one tab stop, its "+" tile included').toBe(1);
  expect(stops.get(3), 'the user panel is two tab stops: the status menu button, and mute and deafen as one toolbar').toBe(2);

  // web3.8 (R4): node, link, generation and devices live in Settings → Diagnostics, each with a value.
  const settings = await openSettings(page, 'diagnostics');
  for (const [key, value] of [['diagnostics.connection', /^online$/], ['diagnostics.generation', /^\S+$/],
    ['diagnostics.instance', new RegExp(`^${escapeRegExp(instanceName)}$`)], ['diagnostics.devices', /^[0-9]+$/]] as const) {
    const row = settings.locator('.d-settings-row').filter({ has: page.locator('.d-settings-row__label', { hasText: new RegExp(`^${escapeRegExp(copy(key))}$`) }) });
    await expect(row).toBeVisible({ timeout: WAIT });
    await expect(row.locator('.d-settings-row__control')).toHaveText(value, { timeout: WAIT });
  }
  await closeSettings(page);
});
