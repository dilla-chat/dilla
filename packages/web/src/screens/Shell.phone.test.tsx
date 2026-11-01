import { afterEach, describe, expect, it, vi } from 'vitest';
import { openRowMenu } from '../test/row-menu.ts';
import * as ClientCore from '@dilla/client-core';
import type { BotUtilities, ReminderSummary } from '@dilla/client-core';
import { act, cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { PHONE_QUERY } from '../layout.ts';
import { t } from '../strings/index.ts';
import { expectNoAxeViolations } from '../test/setup.ts';
import { A, BOB, callState, CAROL, CHANNELS, DM, GENV, MUSIC, renderVoiceShell, TEXT, type VoiceHarnessOptions } from '../test/fake-client.ts';
import { ME } from '../test/fixtures.ts';
import { pushHistoryState, replaceHistoryState } from '../router.ts';

/** A window of the given kind: only the phone query matches, so the wide-window defaults (the members column) stay off. */
function media(phone: boolean) {
  window.matchMedia = vi.fn((query: string) => ({
    matches: phone && query === PHONE_QUERY, media: query,
    addEventListener: () => undefined, removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
}
afterEach(() => {
  cleanup();
  replaceHistoryState(null, '', '/');
  Reflect.deleteProperty(window, 'matchMedia');
  // eslint-disable-next-line no-restricted-properties -- clears the tab memory the shell writes
  window.sessionStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function phone(o: VoiceHarnessOptions = {}) {
  media(true);
  return renderVoiceShell(o);
}
const tabs = () => screen.queryByRole('navigation', { name: t('nav.tabs.label') });
const tab = (name: string) => within(tabs()!).getByRole('link', { name });
const homeTile = () => within(screen.getByRole('navigation', { name: t('shell.rail.label') })).getByRole('button', { name: t('home.tile') });
const goto = (path: string, state: unknown = null) => {
  act(() => pushHistoryState(state, '', path));
};
const here = () => window.location.pathname;
const manage = (fake: ReturnType<typeof renderVoiceShell>['client']) =>
  act(() => { fake.publish(`permissions:${A}`, { loaded: true, manageChannels: true } as never); });

describe('the phone shell: the servers list', () => {
  it('shows the tab bar, stays on the server (no first-channel redirect) and has no status bar or sidebar tabs, and exactly three tabs', async () => {
    phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    expect(within(tabs()!).getAllByRole('link').map(l => l.textContent)).toEqual(['Home', 'Activity', 'You']);
    expect(tab('Home')).toHaveAttribute('aria-current', 'page');
    expect(here()).toBe(`/c/${A}`);
    expect(screen.queryByRole('region', { name: t('shell.status.label') })).toBeNull();
    expect(screen.queryByRole('tablist')).toBeNull();
    expect(document.querySelector('.dw-voice-dock')).toBeNull();
  });

  // Ruling AB: the server head is the prototype's: the name and the ⌄ (server settings); the "+" only while there are no categories
  // and for a person who may. No search, no reminders, no menu in it.
  it('has only the ⌄ beside the server name; the list header has the "+" only for a person who may', async () => {
    const { user, client } = phone({ path: `/c/${A}` });
    const menu = await screen.findByRole('button', { name: t('admin.open') });
    const head = menu.closest('.d-channel-list__head') as HTMLElement;
    expect(within(head).getAllByRole('button')).toEqual([menu]);
    expect(menu.querySelector('svg[data-icon="chevron-down"]')).not.toBeNull();
    expect(screen.queryByRole('button', { name: t('search.open') })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Reminders' })).toBeNull();
    expect(screen.queryByRole('button', { name: t('nav.server.menu') })).toBeNull();
    expect(screen.queryByRole('button', { name: t('admin.leave.open') })).toBeNull();
    expect(screen.queryByRole('button', { name: t('create.channel.open') })).toBeNull();
    manage(client);
    expect(screen.getByRole('button', { name: t('create.channel.open') })).toBeInTheDocument();
    // Signal task 10 fix round 2: the ⌄ is the server's menu; its first item opens the server's settings.
    await user.click(menu);
    await user.click(await screen.findByRole('menuitem', { name: t('signalSettings.menu.serverSettings') }));
    const dialog = await screen.findByRole('dialog', { name: t('admin.title') });
    expect(within(dialog).getByRole('button', { name: t('workspace.danger') })).toBeInTheDocument();
  });

  it('has one "+" tile on the rail and no captions; it opens the add dialog', async () => {
    const { user } = phone({ path: `/c/${A}` });
    const rail = await screen.findByRole('navigation', { name: t('shell.rail.label') });
    expect(within(rail).queryByText(t('create.rail.join'))).toBeNull();
    expect(within(rail).queryByText(t('create.rail.new'))).toBeNull();
    await user.click(within(rail).getByRole('button', { name: t('layout.rail.add') }));
    const dialog = await screen.findByRole('dialog', { name: t('layout.add.title') });
    expect(within(dialog).getByRole('button', { name: t('layout.add.join') })).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: t('layout.add.create') })).toBeInTheDocument();
  });

  it('a tap on a voice row only opens it; the join icon joins', async () => {
    const app = phone({ path: `/c/${A}` });
    await app.user.click(await screen.findByRole('button', { name: 'voice general' }));
    await waitFor(() => expect(here()).toBe(`/c/${A}/${GENV}`));
    expect(app.log).not.toContain(`callJoin:${GENV}`);
    goto(`/c/${A}`);
    const row = (await screen.findByRole('button', { name: 'voice music' })).closest('li') as HTMLElement;
    await app.user.click(within(row).getByRole('button', { name: t('voice.join') }));
    await waitFor(() => expect(app.log).toContain(`callJoin:${MUSIC}`));
  });

  it('has no axe violations', async () => {
    const { view } = phone({ path: `/c/${A}` });
    await screen.findByRole('button', { name: t('admin.open') });
    await expectNoAxeViolations(view.container);
  });
});

describe('the phone shell: search (walk 1)', () => {
  // Ruling AB: the list has no search; a conversation's phone header has it.
  it('the servers list has no search of its own', async () => {
    phone({ path: `/c/${A}` });
    await screen.findByRole('button', { name: t('admin.open') });
    expect(screen.queryByRole('button', { name: t('search.open') })).toBeNull();
  });

  it('opens it from the channel header\'s search (Signal §5.10, Ruling U), scoped to the conversation', async () => {
    const { user, client } = phone({ path: `/c/${A}/${TEXT}` });
    const header = (await screen.findByRole('heading', { level: 2, name: 'lobby' })).closest('.d-phone-header') as HTMLElement;
    await user.click(within(header).getByRole('button', { name: t('search.open') }));
    await user.type(screen.getByRole('combobox', { name: t('search.placeholder') }), 'lamp');
    // Task 9 fix round 1: the scope is implicit (everywhere, the open conversation's hits first).
    await waitFor(() => expect(client.callsOf('search')).toContainEqual({ m: 'search', scope: 'all', id: null, query: 'lamp', cursor: null }));
  });
});

describe('the phone shell: pushed views', () => {
  it('pushes a channel with phoneBack, shows it alone, and back calls history.back()', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    expect(here()).toBe(`/c/${A}/${TEXT}`);
    expect(window.history.state).toMatchObject({ phoneBack: true });
    expect(tabs()).not.toBeNull();
    expect(screen.getByRole('main')).not.toHaveAttribute('hidden');
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    await user.click(screen.getByRole('button', { name: t('nav.back') }));
    expect(back).toHaveBeenCalledTimes(1);
  });

  it('opening a channel focuses no field: the composer is not active and the tab bar is shown', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    const box = await screen.findByRole('textbox', { name: 'message #lobby' });
    expect(box).not.toHaveFocus();
    expect(tabs()).toBeVisible();
    expect(document.querySelector('.d-app-shell')).not.toHaveAttribute('data-typing');
  });

  it('replaces a deep-linked channel with its server on back', async () => {
    const { user } = phone({ path: `/c/${A}/${TEXT}` });
    const before = window.history.length;
    await user.click(await screen.findByRole('button', { name: t('nav.back') }));
    expect(here()).toBe(`/c/${A}`);
    expect(window.history.length).toBe(before);
    expect(await screen.findByRole('navigation', { name: t('nav.tabs.label') })).toBeInTheDocument();
  });

  it('has exactly Threads, Members and search in the header, no menu (Ruling X)', async () => {
    phone({ path: `/c/${A}/${TEXT}` });
    const header = (await screen.findByRole('heading', { level: 2, name: 'lobby' })).closest('.d-phone-header') as HTMLElement;
    const actions = header.querySelector<HTMLElement>('.d-phone-header__actions')!;
    expect(within(actions).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual([t('layout.header.threads'), expect.stringMatching(/^Members/), t('search.open')]);
    expect(within(header).queryByRole('button', { name: t('nav.channel.menu') })).toBeNull();
  });

  it('a channel row\'s context menu on the list holds pins, and edit channel for a person who may (Ruling X)', async () => {
    const { user, client } = phone({ path: `/c/${A}` });
    let menu = await openRowMenu(user, 'lobby');
    // Signal task 10 fix round 1: Mute moved from Settings → Notifications into the row's menu.
    expect(within(menu).getAllByRole('menuitem').map(i => i.textContent)).toEqual([t('shell.pins.open')]);
    // Signal task 10 fix rounds 1–2: Mute and the conversation's notify mode are in the row menu too.
    expect(within(menu).getByRole('menuitemcheckbox', { name: t('notify.mute') })).toHaveAttribute('aria-checked', 'false');
    expect(within(menu).getByRole('menuitemradio', { name: t('signalSettings.menu.notifyDefault') })).toHaveAttribute('aria-checked', 'true');
    await user.keyboard('{Escape}');
    manage(client);
    menu = await openRowMenu(user, 'lobby');
    expect(within(menu).getAllByRole('menuitem').map(i => i.textContent)).toEqual([t('shell.pins.open'), t('admin.channel.open')]);
  });

  // Ruling Z: a DM's phone header is back, name, topic and Search; its pins (and call) are in the DM row's context menu.
  it('opens a DM as a pushed view whose header has search alone; pins are in its row menu', async () => {
    const { user } = phone({ path: '/dm' });
    let menu = await openRowMenu(user, 'bob', 'dms');
    expect(within(menu).getAllByRole('menuitem').map(i => i.textContent)).toEqual([t('call.start'), t('shell.pins.open')]);
    expect(within(menu).getByRole('menuitemcheckbox', { name: t('notify.mute') })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await user.click(await screen.findByRole('button', { name: 'bob' }));
    expect(here()).toBe(`/dm/${DM}`);
    expect(window.history.state).toMatchObject({ phoneBack: true });
    expect(tabs()).not.toBeNull();
    const actions = document.querySelector<HTMLElement>('.d-phone-header__actions')!;
    expect(within(actions).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual([t('search.open')]);
    expect(screen.queryByRole('button', { name: t('nav.channel.menu') })).toBeNull();
    menu = screen.queryByRole('menu') as HTMLElement;
    expect(menu).toBeNull();
  });

  // Ruling Z: a voice channel's header has no controls (the prototype's), its topic says it is a voice channel.
  it('the voice stage header has no controls', async () => {
    phone({ path: `/c/${A}/${GENV}` });
    const header = (await screen.findByRole('heading', { level: 2, name: 'general' })).closest('.d-phone-header') as HTMLElement;
    expect(header.querySelector('.d-phone-header__actions')?.querySelectorAll('button').length ?? 0).toBe(0);
    expect(header).toHaveTextContent(t('voice.header.idle'));
  });

  it('gives the voice stage a phone header with its name and a back, under the tab bar', async () => {
    phone({ path: `/c/${A}/${GENV}` });
    expect(await screen.findByRole('heading', { level: 2, name: 'general' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t('nav.back') })).toBeInTheDocument();
    expect(tabs()).not.toBeNull();
  });
});

describe('the phone shell: the members sheet', () => {
  it('opens as a history entry, closes on the system back and returns focus to the Members button', async () => {
    const { user } = phone({ path: `/c/${A}/${TEXT}` });
    const trigger = await screen.findByRole('button', { name: /^Members/ });
    await user.click(trigger);
    expect(await screen.findByRole('complementary', { name: 'members of Midgard' })).toBeInTheDocument();
    expect(window.history.state).toMatchObject({ phoneSheet: 'members' });
    act(() => { window.history.back(); });
    await waitFor(() => expect(screen.queryByRole('complementary')).toBeNull());
    expect(trigger).toHaveFocus();
  });

  it('closes through its own back control by popping its entry', async () => {
    const { user } = phone({ path: `/c/${A}/${TEXT}` });
    const trigger = await screen.findByRole('button', { name: /^Members/ });
    const index = () => (window.history.state as { dillaHistoryIndex?: number } | null)?.dillaHistoryIndex;
    const before = index();
    await user.click(trigger);
    const sheet = await screen.findByRole('complementary', { name: 'members of Midgard' });
    const length = window.history.length;
    await user.click(within(sheet).getByRole('button', { name: t('members.close') }));
    await waitFor(() => expect(screen.queryByRole('complementary')).toBeNull());
    expect(window.history.length).toBe(length);
    expect((window.history.state as { phoneSheet?: unknown } | null)?.phoneSheet).toBeUndefined();
    // Fix round 2: the list's own × (Q4) pops the sheet's entry rather than leaving it behind, so the next Back leaves the channel.
    await waitFor(() => expect(index()).toBe(before));
  });
});

describe('the phone shell: the link banner', () => {
  const line = () => document.querySelector('.d-link-banner');
  it('says nothing online, offline when the link is down, and where it connects while connecting', async () => {
    const { client } = phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    expect(line()).toHaveAttribute('role', 'status');
    expect(line()).toHaveTextContent('');
    act(() => client.set('connection', { status: 'offline', generation: '1' }));
    expect(screen.getByText(t('nav.link.offline'))).toBeInTheDocument();
    expect(screen.getByText(t('nav.link.offline')).closest('[role="status"]')).toBe(line());
    act(() => client.set('connection', { status: 'connecting', generation: '1' }));
    expect(line()).toHaveTextContent(t('nav.link.connecting', { node: 'dilla.test' }));
    act(() => client.set('connection', { status: 'online', generation: '1' }));
    expect(line()).toHaveTextContent('');
  });
});

describe('the phone shell: tabs', () => {
  it('@ shows the DMs on Home; activity and you show each screen without a rail', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    expect(screen.getByRole('navigation', { name: t('shell.rail.label') })).toBeInTheDocument();
    // Home shows the rail and the list pane; the @ tile shows the DMs in the list pane, still on Home.
    await user.click(homeTile());
    expect(here()).toBe('/dm');
    expect(await screen.findByRole('button', { name: 'bob' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: t('shell.rail.label') })).toBeInTheDocument();
    expect(homeTile()).toHaveAttribute('aria-current', 'page');
    expect(tab('Home')).toHaveAttribute('aria-current', 'page');
    await user.click(tab('Activity'));
    expect(here()).toBe('/activity');
    expect(await screen.findByText(t('activity.emptyTitle'))).toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: t('shell.rail.label') })).toBeNull();
    await user.click(tab('You'));
    expect(here()).toBe('/you');
    expect(await screen.findByRole('heading', { level: 2, name: t('you.title') })).toBeInTheDocument();
  });

  it('counts DMs unread on Home, and activity as channel mentions plus DM unread', async () => {
    const { client } = phone({ path: '/dm' });
    await screen.findByRole('button', { name: 'bob' });
    act(() => client.set('badges', { [TEXT]: { unread: 5, mentions: 2 }, [DM]: { unread: 3, mentions: 0 } }));
    expect(tab(t('nav.tab.unread', { name: 'Home', count: 3 }))).toBeInTheDocument();
    expect(tab(t('nav.tab.unread', { name: 'Activity', count: 5 }))).toBeInTheDocument();
  });

  it('opens a tab where it was left', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    expect(here()).toBe(`/c/${A}/${TEXT}`);
    goto('/activity', { phoneBack: true });
    await screen.findByText(t('activity.emptyTitle'));
    await user.click(tab('Home'));
    expect(here()).toBe(`/c/${A}/${TEXT}`);
  });

  it('keeps the tab bar in a channel; the current tab goes back to its list', async () => {
    const { user } = phone({ path: `/c/${A}/${TEXT}` });
    await screen.findByRole('main');
    expect(tabs()).not.toBeNull();
    expect(tab('Home')).toHaveAttribute('aria-current', 'page');
    await user.click(tab('Home'));
    expect(here()).toBe(`/c/${A}`);
    expect(await screen.findByRole('navigation', { name: t('shell.channels.label') })).toBeInTheDocument();
  });

  it('Home opens where it was left: a DM, then a channel', async () => {
    const { user } = phone({ path: `/dm/${DM}` });
    await screen.findByRole('main');
    goto('/activity', { phoneBack: true });
    await screen.findByText(t('activity.emptyTitle'));
    await user.click(tab('Home'));
    expect(here()).toBe(`/dm/${DM}`);
    goto(`/c/${A}/${TEXT}`, { phoneBack: true });
    goto('/activity', { phoneBack: true });
    await user.click(tab('Home'));
    expect(here()).toBe(`/c/${A}/${TEXT}`);
  });

  it('an open members sheet closes when a tab is tapped: its entry is popped once and the new tab shows', async () => {
    const { user } = phone({ path: `/c/${A}/${TEXT}` });
    await user.click(await screen.findByRole('button', { name: /^Members/ }));
    await screen.findByRole('complementary', { name: 'members of Midgard' });
    expect(tabs()).not.toBeNull();
    const length = window.history.length;
    const back = vi.spyOn(window.history, 'back');
    await user.click(tab('Activity'));
    await waitFor(() => expect(here()).toBe('/activity'));
    expect(back).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('complementary')).toBeNull();
    expect(await screen.findByText(t('activity.emptyTitle'))).toBeInTheDocument();
    expect(window.history.length).toBe(length);
    expect((window.history.state as { phoneSheet?: unknown } | null)?.phoneSheet).toBeUndefined();
  });

  it('hides the tab bar while server settings is open', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: t('admin.open') })); await user.click(await screen.findByRole('menuitem', { name: t('signalSettings.menu.serverSettings') })); // Signal task 10 fix round 2: the ⌄ is a menu
    expect(await screen.findByRole('dialog', { name: t('admin.title') })).toBeInTheDocument();
    expect(tabs()).toBeNull();
    expect(here()).toBe(`/c/${A}/settings`);
  });

  it('two quick taps on a tab with a sheet open pop the sheet entry once', async () => {
    phone({ path: `/c/${A}/${TEXT}` });
    const { user } = { user: (await import('@testing-library/user-event')).default.setup() };
    await user.click(await screen.findByRole('button', { name: /^Members/ }));
    await screen.findByRole('complementary', { name: 'members of Midgard' });
    const back = vi.spyOn(window.history, 'back');
    const link = tab('Activity');
    act(() => { link.click(); link.click(); });
    await waitFor(() => expect(here()).toBe('/activity'));
    expect(back).toHaveBeenCalledTimes(1);
  });

  it('the current tab in a view the app pushed goes back instead of replacing it', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    await user.click(tab('Home'));
    expect(back).toHaveBeenCalledTimes(1);
  });

  it('the current tab lands on its own list, never on the screen the view was pushed from', async () => {
    const { user } = phone({ path: '/activity' });
    await screen.findByRole('heading', { name: t('activity.title') });
    goto(`/c/${A}/${TEXT}`, { phoneBack: true, phoneFrom: '/activity' });
    await screen.findByRole('textbox', { name: 'message #lobby' });
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    await user.click(tab('Home'));
    expect(back).not.toHaveBeenCalled();
    expect(here()).toBe(`/c/${A}`);
  });

  describe('the system forward button (N5, R-c)', () => {
    it('lands the current tab on its own list when back would land on another view', async () => {
      const { user } = phone({ path: '/dm' });
      await screen.findByRole('navigation', { name: t('shell.dms.label') });
      goto(`/c/${A}/${TEXT}`, { phoneBack: true, phoneFrom: `/c/${A}` });
      await screen.findByRole('textbox', { name: 'message #lobby' });
      const originalBack = window.history.back.bind(window.history);
      const back = vi.spyOn(window.history, 'back').mockImplementation(() => originalBack());
      await user.click(tab('Home'));
      expect(back).toHaveBeenCalledTimes(1);
      await waitFor(() => expect(here()).toBe(`/c/${A}`));
    });

    it('control: back that lands on the tab list is left alone', async () => {
      const { user } = phone({ path: `/c/${A}` });
      await screen.findByRole('navigation', { name: t('shell.channels.label') });
      goto(`/c/${A}/${TEXT}`, { phoneBack: true, phoneFrom: `/c/${A}` });
      await screen.findByRole('textbox', { name: 'message #lobby' });
      const originalBack = window.history.back.bind(window.history);
      const back = vi.spyOn(window.history, 'back').mockImplementation(() => originalBack());
      await user.click(tab('Home'));
      expect(back).toHaveBeenCalledTimes(1);
      await waitFor(() => expect(here()).toBe(`/c/${A}`));
    });

    it('two quick Home taps from a forward entry land on the Home list', async () => {
      const { user } = phone({ path: '/dm' });
      await screen.findByRole('navigation', { name: t('shell.dms.label') });
      goto(`/c/${A}/${TEXT}`, { phoneBack: true, phoneFrom: `/c/${A}` });
      await screen.findByRole('textbox', { name: 'message #lobby' });
      await user.click(tab('Home'));
      await user.click(tab('Home'));
      await waitFor(() => expect(here()).toBe(`/c/${A}`));
    });
  });

  it('a DM opened from the server list: the current Home tab goes back to that list', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    goto(`/dm/${DM}`, { phoneBack: true, phoneFrom: `/c/${A}` });
    await screen.findByRole('textbox');
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    await user.click(tab('Home'));
    expect(back).toHaveBeenCalledTimes(1);
  });

  it('a deep-linked DM: the current Home tab lands on the DM list, never on a server', async () => {
    const { user } = phone({ path: `/dm/${DM}` });
    await screen.findByRole('textbox');
    await user.click(tab('Home'));
    expect(here()).toBe('/dm');
    expect(await screen.findByRole('button', { name: 'bob' })).toBeInTheDocument();
  });

  it('a DM opened from the @ list: the current Home tab goes back to /dm', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(homeTile());
    await user.click(await screen.findByRole('button', { name: 'bob' }));
    expect(here()).toBe(`/dm/${DM}`);
    await screen.findByRole('textbox');
    await user.click(tab('Home'));
    await waitFor(() => expect(here()).toBe('/dm'));
  });

  it('opens an unvisited tab at its root', async () => {
    const { user } = phone({ path: '/you' });
    await screen.findByRole('heading', { level: 2, name: t('you.title') });
    await user.click(tab('Home'));
    expect(here()).toBe(`/c/${A}`);
  });
});

describe('the phone shell: history', () => {
  it('replaces the entry on every tab tap, so the history never grows', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    const before = window.history.length;
    await user.click(tab('Activity'));
    await user.click(tab('You'));
    await user.click(tab('Home'));
    expect(window.history.length).toBe(before);
    const state = window.history.state as { dillaHistoryIndex?: unknown };
    expect(Number.isSafeInteger(state.dillaHistoryIndex)).toBe(true);
  });

  it('goes back to the list from a view a tab restored', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    goto('/activity', { phoneBack: true });
    await screen.findByText(t('activity.emptyTitle'));
    await user.click(tab('Home'));
    expect(here()).toBe(`/c/${A}/${TEXT}`);
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    await user.click(screen.getByRole('button', { name: t('nav.back') }));
    expect(back).not.toHaveBeenCalled();
    expect(here()).toBe(`/c/${A}`);
  });

  it('puts focus on the row that opened the view when back returns to the list', async () => {
    const { user } = phone({ path: `/c/${A}` });
    await user.click(await screen.findByRole('button', { name: 'lobby' }));
    await screen.findByRole('log');
    goto(`/c/${A}`, { phoneBack: true });
    await screen.findByRole('button', { name: 'lobby' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'lobby' })).toHaveFocus());
  });

  it('focuses the exact row it left when another name starts with it', async () => {
    const { user, client } = phone({ path: `/c/${A}` });
    const more = ['general', 'gen'].map((name, i) => ({ id: `${i}f`.repeat(16), communityId: A, kind: 0 as const, mode: 0 as const, name,
      topic: '', parentId: null, position: 5 + i, group: 'active' as const, slowmode: 0, announcement: false }));
    act(() => client.set(`channels:${A}`, [...CHANNELS, ...more]));
    await user.click(await screen.findByRole('button', { name: 'gen' }));
    await screen.findByRole('log');
    goto(`/c/${A}`, { phoneBack: true });
    await waitFor(() => expect(screen.getByRole('button', { name: 'gen' })).toHaveFocus());
  });

  it('closes a direct Settings link with Escape without adding history', async () => {
    const { user } = phone({ path: '/settings/devices', withSettings: true });
    await screen.findByRole('dialog', { name: t('settings.title') });
    const before = window.history.length;
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: t('settings.title') })).toBeNull());
    expect(window.history.length).toBe(before);
  });

  it('shows no link banner for a version refusal, only the newer-client banner', async () => {
    const { client } = phone({ path: `/c/${A}` });
    await screen.findByRole('navigation', { name: t('shell.channels.label') });
    act(() => client.set('connection', { status: 'offline', generation: '1', reason: 'version' }));
    expect(screen.getByText(t('shell.banner.clientTooOld'))).toBeInTheDocument();
    expect(document.querySelector('.d-link-banner')).toBeNull();
    expect(screen.queryByText(t('nav.link.offline'))).toBeNull();
  });
});

describe('the phone shell: the call bar', () => {
  /** Joins voice 'general' from its stage, as the shell does; the call bar follows the person off the stage. */
  async function joined() {
    const app = phone({ path: `/c/${A}/${GENV}` });
    await app.user.click(await screen.findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
    return app;
  }

  it('shows a call bar on the list, with no dock, and its open button pushes the call', async () => {
    const { user } = await joined();
    expect(screen.queryByRole('group', { name: t('voice.dock.region') })).toBeNull();
    goto(`/c/${A}`);
    const open = await screen.findByRole('button', { name: t('nav.call.open', { where: 'general' }) });
    expect(screen.getByRole('group', { name: t('voice.dock.region') })).toBeInTheDocument();
    expect(document.querySelector('.dw-voice-dock')).toBeNull();
    await user.click(open);
    expect(here()).toBe(`/c/${A}/${GENV}`);
    expect(window.history.state).toMatchObject({ phoneBack: true });
    await screen.findByRole('toolbar', { name: 'voice channel controls' });
    expect(screen.queryByRole('group', { name: t('voice.dock.region') })).toBeNull();
  });

  it('keeps the call bar on another view, and mutes and leaves from it', async () => {
    const app = await joined();
    goto(`/c/${A}/${TEXT}`);
    const bar = await screen.findByRole('group', { name: t('voice.dock.region') });
    await app.user.click(within(bar).getByRole('button', { name: t('voice.mute') }));
    expect(app.session.muted).toBe(true);
    await app.user.click(within(bar).getByRole('button', { name: t('voice.leave') }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('user'));
  });
});

describe('the phone shell: settings', () => {
  it('opens a modal index from You, hides tabs, and pushes individual sections', async () => {
    const { user } = phone({ path: '/you', withSettings: true });
    await user.click(await screen.findByRole('button', { name: t('you.settings') }));
    expect(here()).toBe('/settings');
    const dialog = screen.getByRole('dialog', { name: t('settings.title') });
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    expect(tabs()).toBeNull();
    const before = history.length;
    await user.click(within(dialog).getByRole('button', { name: 'Devices' }));
    expect(here()).toBe('/settings/devices');
    expect(history.length).toBe(before + 1);
    expect(history.state).toMatchObject({ settingsFrom: '/you' });
    // Task 10 (README §5.10): no Back button on a phone section; the browser's back returns to the index.
    expect(within(dialog).queryByRole('button', { name: t('workspace.back') })).toBeNull();
    history.back();
    await waitFor(() => expect(here()).toBe('/settings'));
    const settingsDialog = screen.getByRole('dialog', { name: t('settings.title') });
    await within(settingsDialog).findByRole('button', { name: t('settings.close') });
    await user.click(within(settingsDialog).getByRole('button', { name: t('settings.close') }));
    await waitFor(() => expect(here()).toBe('/you'));
  });
  // Task 10 (README §5.10): a direct section link shows the nav as chips above the section; Close leaves settings.
  it('direct section links show the nav as chips, and Close leaves', async () => {
    const { user } = phone({ path: '/settings/devices', withSettings: true });
    await screen.findByRole('dialog', { name: t('settings.title') });
    const dialog = screen.getByRole('dialog', { name: t('settings.title') });
    expect(dialog).toHaveAttribute('data-phone-page', 'section');
    expect(within(dialog).getByRole('navigation', { name: t('settings.nav.label') })).toBeVisible();
    expect(within(dialog).getByRole('button', { name: 'Devices' })).toHaveAttribute('aria-current', 'page');
    expect(tabs()).toBeNull();
    await user.click(within(dialog).getByRole('button', { name: t('settings.close') }));
    await waitFor(() => expect(here()).not.toMatch(/^\/settings/));
  });
  it('keeps the voice session running when settings opens and returns to the call', async () => {
    const app = phone({ path: `/c/${A}/${GENV}` });
    await app.user.click(await screen.findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    act(() => app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV })));
    goto('/settings', { settingsFrom: `/c/${A}/${GENV}` });
    const dialog = await screen.findByRole('dialog', { name: t('settings.title') });
    expect(app.session.leave).not.toHaveBeenCalled();
    await app.user.click(within(dialog).getByRole('button', { name: t('workspace.returnCall') }));
    await waitFor(() => expect(here()).toBe(`/c/${A}/${GENV}`));
    expect(app.sessions).toHaveLength(1);
    expect(app.session.leave).not.toHaveBeenCalled();
  });
});

describe('the phone shell: one header on every tab root (H1, H2, H3)', () => {
  it('Home on a server: the server name in the 48 head, with search, settings and, for a manager, the "+"', async () => {
    const { client } = phone({ path: `/c/${A}` });
    const gear = await screen.findByRole('button', { name: t('admin.open') });
    const head = gear.closest('.d-channel-list__head') as HTMLElement;
    expect(within(head).getByRole('heading', { level: 1 })).toHaveTextContent('Midgard');
    manage(client);
    expect(within(head).getByRole('button', { name: t('create.channel.open') })).toBeInTheDocument();
  });

  it('Home on @: "direct messages" in the head with the new-message action, and no footer', async () => {
    const { user } = phone({ path: '/dm' });
    const head = (await screen.findByRole('heading', { level: 1, name: t('shell.dms.title') })).closest('.d-channel-list__head') as HTMLElement;
    expect(head).not.toBeNull();
    const add = within(head).getByRole('button', { name: t('shell.dms.new') });
    expect(add.querySelector('svg[data-icon="square-pen"]')).not.toBeNull();
    expect(document.querySelector('.dw-phone-footer')).toBeNull();
    expect(screen.getAllByRole('button', { name: t('shell.dms.new') })).toHaveLength(1);
    await user.click(add);
    expect(await screen.findByRole('dialog', { name: t('shell.dms.newTitle') })).toBeInTheDocument();
  });

  it('Home on @ has no search in its head, only "message someone"', async () => {
    phone({ path: '/dm' });
    const head = (await screen.findByRole('heading', { level: 1, name: t('shell.dms.title') })).closest('.d-channel-list__head') as HTMLElement;
    // Signal task 10 fix round 2: beside "message someone", the DM list head's menu (the device-wide notify default).
    expect(within(head).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual([t('shell.dms.new'), t('signalSettings.menu.dms')]);
  });

  // Task 11 (Jonas, Q3): Activity keeps its titled head (the prototype's "Activity"); You has no title bar, only a heading for
  // the tab root (screen readers and the tab's focus).
  it('Activity opens with a phone header holding its title; You with a hidden heading', async () => {
    const { user } = phone({ path: '/activity' });
    const activity = await screen.findByRole('heading', { level: 2, name: t('activity.title') });
    expect(activity.closest('header')).toHaveClass('d-phone-header');
    await user.click(tab('You'));
    const you = await screen.findByRole('heading', { level: 2, name: t('you.title') });
    expect(you).toHaveClass('d-sr-only');
  });

  // Ruling Z: the call is the DM row's menu item and Control/⌘+Shift+Enter; the header and the strip have no call button.
  it('a DM view has no call button; Control+Shift+Enter starts the call', async () => {
    const { user, sessions } = phone({ path: `/dm/${DM}` });
    await screen.findByRole('heading', { level: 2, name: 'bob' });
    expect(screen.queryByRole('button', { name: t('call.start') })).toBeNull();
    expect(document.querySelector('.dw-call-strip')).toHaveAttribute('data-empty', 'true');
    await user.keyboard('{Control>}{Shift>}{Enter}{/Shift}{/Control}');
    await waitFor(() => expect(sessions).toHaveLength(1));
  });

  it('Control+Shift+Enter outside a DM keeps its default and starts nothing', async () => {
    const { sessions } = phone({ path: `/c/${A}/${TEXT}` });
    await screen.findByRole('textbox', { name: /message/ });
    const event = new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, shiftKey: true, bubbles: true, cancelable: true });
    act(() => { window.dispatchEvent(event); });
    expect(event.defaultPrevented).toBe(false);
    expect(sessions).toHaveLength(0);
  });

  it('a held Control+Shift+Enter in a DM starts one call, not one per repeat', async () => {
    const { sessions } = phone({ path: `/dm/${DM}` });
    await screen.findByRole('heading', { level: 2, name: 'bob' });
    const repeat = new KeyboardEvent('keydown', { key: 'Enter', ctrlKey: true, shiftKey: true, repeat: true, bubbles: true, cancelable: true });
    act(() => { window.dispatchEvent(repeat); });
    expect(repeat.defaultPrevented).toBe(true);
    expect(sessions).toHaveLength(0);
  });

  describe('while typing', () => {
    async function inCall() {
      const app = phone({ path: `/c/${A}/${GENV}` });
      await app.user.click(await screen.findByRole('button', { name: 'Join voice' }));
      await waitFor(() => expect(app.sessions).toHaveLength(1));
      app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
      goto(`/c/${A}/${TEXT}`);
      await screen.findByRole('group', { name: t('voice.dock.region') });
      return app;
    }
    it('drops the call bar and shows an accent dot after the title, named by the bar\'s own label', async () => {
      await inCall();
      const label = t('nav.call.open', { where: 'general' });
      const header = () => document.querySelector('.d-app-shell__main .d-phone-header') as HTMLElement;
      expect(within(header()).queryByRole('button', { name: label })).toBeNull();
      act(() => { screen.getByRole('textbox', { name: /message/ }).focus(); });
      await waitFor(() => expect(screen.queryByRole('group', { name: t('voice.dock.region') })).toBeNull());
      expect(within(header()).getByRole('button', { name: label })).toHaveClass('d-phone-header__dot');
      act(() => { (document.activeElement as HTMLElement).blur(); });
      await screen.findByRole('group', { name: t('voice.dock.region') });
      expect(within(header()).queryByRole('button', { name: label })).toBeNull();
    });
    it('the dot is a button that opens the call, as the bar does', async () => {
      const app = await inCall();
      act(() => { screen.getByRole('textbox', { name: /message/ }).focus(); });
      const header = document.querySelector('.d-app-shell__main .d-phone-header') as HTMLElement;
      await app.user.click(await within(header).findByRole('button', { name: t('nav.call.open', { where: 'general' }) }));
      expect(here()).toBe(`/c/${A}/${GENV}`);
    });
    it('shows no dot when no call is up', () => {
      phone({ path: `/c/${A}/${TEXT}` });
      act(() => { screen.getByRole('textbox', { name: /message/ }).focus(); });
      expect(document.querySelector('.d-phone-header__dot')).toBeNull();
    });
  });
});

describe('the desktop keeps its shell', () => {
  it('turns /you into the Settings root and /activity into the server list', async () => {
    media(false);
    renderVoiceShell({ path: '/you', withSettings: true });
    await waitFor(() => expect(here()).toBe('/settings'));
    expect(window.history.state).toMatchObject({ settingsFrom: null });
  });

  it('leaves /activity for the first server', async () => {
    media(false);
    renderVoiceShell({ path: '/activity' });
    await waitFor(() => expect(here()).toBe(`/c/${A}/${TEXT}`));
  });

  it('selects the @ tile on /dm with no DM open, and shows the DM list', async () => {
    media(false);
    renderVoiceShell({ path: '/dm' });
    expect(await screen.findByRole('button', { name: t('home.tile') })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('navigation', { name: t('shell.dms.label') })).toBeInTheDocument();
    expect(here()).toBe('/dm');
    expect(screen.queryByRole('navigation', { name: t('nav.tabs.label') })).toBeNull();
  });

  it('renders no phone chrome', async () => {
    media(false);
    renderVoiceShell({ path: `/c/${A}/${TEXT}` });
    await screen.findByRole('log');
    expect(document.querySelector('[data-layout="phone"]')).toBeNull();
    expect(screen.queryByRole('region', { name: t('shell.status.label') })).toBeNull();
    expect(document.querySelector('.d-link-banner')).toHaveAttribute('role', 'status');
    expect(document.querySelector('.d-link-banner')).toBeEmptyDOMElement();
  });
});

describe('the phone shell: one channel list (web3.8 item 12)', () => {
  it('lists the voice channels in the one list on the servers tab, with no active voice', async () => {
    phone({ path: `/c/${A}` });
    const nav = await screen.findByRole('navigation', { name: t('shell.channels.label') });
    expect(within(nav).getByRole('button', { name: 'voice general' })).toBeInTheDocument();
    expect(within(nav).getByRole('button', { name: /^lobby/ })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'active voice' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Voice' })).toBeNull();
    expect(document.querySelector('.d-active-voice, .dw-voice-section')).toBeNull();
    expect(nav.querySelectorAll('ul[role="list"]:not(.d-voice-row__people)').length).toBe(1);
  });
});

describe('web3.9 WEB-AFK: a phone is exempt from the AFK move (R5)', () => {
  it('the first activity report after a join carries exempt, the second does not', async () => {
    const app = phone({ path: `/c/${A}/${GENV}` });
    act(() => app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false }));
    await app.user.click(await within(screen.getByRole('main')).findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    act(() => app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV })));
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(1));
    expect(app.client.callsOf('afkActivity')[0]).toEqual({ m: 'afkActivity', communityId: A, channelId: GENV, exempt: true });
    const now = Date.now();
    const dateNow = vi.spyOn(Date, 'now').mockReturnValue(now + 6_000);
    act(() => { window.dispatchEvent(new Event('pointerdown')); });
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(2));
    expect(app.client.callsOf('afkActivity')[1]).toEqual({ m: 'afkActivity', communityId: A, channelId: GENV });
    dateNow.mockRestore();
  });

  it('final review M-1: a failed exempt report is retried after a backoff until it is accepted', async () => {
    const app = phone({ path: `/c/${A}/${GENV}` });
    let failures = 1;
    const base = app.client.handler;
    app.client.handler = command => command.m === 'afkActivity' && failures-- > 0
      ? Promise.reject(Object.assign(new Error('E_UNAVAILABLE'), { code: 'E_UNAVAILABLE' })) : base(command);
    act(() => app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false }));
    await app.user.click(await within(screen.getByRole('main')).findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    act(() => app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV })));
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(1));
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(2), { timeout: 3_000 });
    expect(app.client.callsOf('afkActivity')[1]).toEqual({ m: 'afkActivity', communityId: A, channelId: GENV, exempt: true });
    // Accepted: no further retry.
    await new Promise(r => setTimeout(r, 2_500));
    expect(app.client.callsOf('afkActivity')).toHaveLength(2);
  }, 10_000);
});

/** A desktop window whose one media fact is the pointer: coarse (a touch tablet) or fine (a mouse). */
function pointer(coarse: boolean) {
  window.matchMedia = vi.fn((query: string) => ({
    matches: coarse && query === '(pointer: coarse)', media: query,
    addEventListener: () => undefined, removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
}

describe('search on a touch tablet, focus return and a server that did not load (E2 review)', () => {
  it('a fine pointer keeps the desktop server head to the gear alone', async () => {
    pointer(false);
    renderVoiceShell({ path: `/c/${A}` });
    const gear = await screen.findByRole('button', { name: t('admin.open') });
    const head = gear.closest('.d-channel-list__head') as HTMLElement;
    expect(within(head).getAllByRole('button')).toEqual([gear]);
  });

  // Ruling AB: a coarse pointer adds nothing: the top bar holds search on a desktop.
  it('a coarse pointer adds no search beside the ⌄', async () => {
    pointer(true);
    renderVoiceShell({ path: `/c/${A}` });
    const menu = await screen.findByRole('button', { name: t('admin.open') });
    expect(within(menu.closest('.d-channel-list__head') as HTMLElement).getAllByRole('button')).toEqual([menu]);
  });
});

// Task 6b fix round 2: a due reminder is an Activity row; a tap opens the reminders sheet (the popover's sheet layout) with it.
it('opens the reminders sheet from a due reminder\'s Activity row', async () => {
  const due: ReminderSummary = { id: '01'.padStart(32, '0'), text: 'bring two ropes', dueUnixSeconds: 1_800_000_001n,
    timezone: 'UTC', sourceChannelId: TEXT, sourceThreadId: null, state: 'due', revision: 1n };
  vi.spyOn(ClientCore, 'createBotUtilities').mockReturnValue({
    capabilities: () => Promise.resolve({ version: 1, installationId: '44'.repeat(16), botUserId: '55'.repeat(16), supportedOperations: [3, 80, 81, 82, 83, 84] }),
    reminderList: () => Promise.resolve({ items: [due], nextCursor: null }),
  } as unknown as BotUtilities);
  const { user } = phone({ path: '/activity' });
  const row = await screen.findByRole('button', { name: /bring two ropes/ });
  expect(row).toHaveTextContent(t('activity.reminder', { where: '#lobby' }));
  await user.click(row);
  const sheet = await screen.findByRole('dialog', { name: 'Reminders' });
  expect(sheet).toHaveAttribute('data-layout', 'sheet');
  expect(within(sheet).getByRole('tab', { name: /^Due 1/ })).toHaveAttribute('aria-selected', 'true');
  expect(within(sheet).getByText('bring two ropes')).toBeInTheDocument();
  expect(within(sheet).getByRole('button', { name: 'Dismiss' })).toBeInTheDocument();
  await user.keyboard('{Escape}');
  expect(screen.queryByRole('dialog', { name: 'Reminders' })).toBeNull();
  expect(row).toHaveFocus();
});

// Ruling AE: the phone's home lists end in the prototype's user panel (above the tab bar), the desktop's panel and data.
it('shows the user panel under the phone\'s server and DM lists, not on Activity or You', async () => {
  const { user } = phone({ path: `/c/${A}` });
  await screen.findByRole('button', { name: t('admin.open') });
  const panel = document.querySelector('.dw-sidebar .d-user-panel');
  expect(panel).not.toBeNull();
  expect(within(panel as HTMLElement).getByRole('toolbar', { name: 'your account' })).toBeInTheDocument();
  await user.click(screen.getByRole('link', { name: new RegExp(`^${t('nav.tab.activity')}`) }));
  await screen.findByRole('heading', { level: 2, name: t('activity.title') });
  expect(document.querySelector('.d-user-panel')).toBeNull();
});

// Ruling X / Jonas 2026-10-27: no "Reminders" button in any phone header or list head; a due reminder is a row on the Activity
// tab (Activity.test.tsx) that opens the reminders sheet (the provider's RemindersPopover in its sheet layout).
for (const path of [`/dm/${DM}`, `/c/${A}/${GENV}`, `/c/${A}`, '/dm', `/c/${A}/${TEXT}`] as const) {
  it(`has no reminders button on the phone at ${path}`, async () => {
    phone({ path });
    await screen.findByRole('navigation', { name: t('nav.tabs.label') });
    expect(screen.queryByRole('button', { name: 'Reminders' })).toBeNull();
  });
}

// Task 9 fix round 2: the member menu's Mention on a phone pops the members sheet first, then focuses the composer, caret at the end.
describe('the phone shell: Mention', () => {
  it('closes the members sheet and focuses the channel composer with "@username " at the end', async () => {
    const { user, client } = phone({ path: `/c/${A}/${TEXT}` });
    act(() => { client.set(`presence:${A}`, { [ME.id]: 'online', [BOB]: 'online', [CAROL]: 'online' }); });
    await user.click(await screen.findByRole('button', { name: /^Members/ }));
    const sheet = await screen.findByRole('complementary', { name: 'members of Midgard' });
    fireEvent.contextMenu(within(sheet).getByRole('button', { name: /\bbob\b/ }), { clientX: 10, clientY: 10 });
    await user.click(within(screen.getByRole('menu', { name: 'actions for bob' })).getByRole('menuitem', { name: 'Mention' }));
    await waitFor(() => expect(screen.queryByRole('complementary')).toBeNull());
    const box = document.querySelector<HTMLTextAreaElement>('.d-app-shell__main .d-composer textarea')!;
    await waitFor(() => expect(box).toHaveFocus());
    expect(box.value).toBe('@bob ');
    expect(box.selectionStart).toBe(box.value.length);
  });
});
