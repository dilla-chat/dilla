import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { baseRoute, goBack, parseRoute, PHONE_BACK, pushHistoryState, replaceHistoryState, requestNavigation, routeOf, routePath, SETTINGS_FROM, setNavigationGuard, subscribeCommittedTraversal, tabOf, useRoute, type Route } from './router.ts';

const C = '0123456789abcdef'.repeat(2);
const CH = 'fedcba9876543210'.repeat(2);

const DM = '9a'.repeat(16);
describe('parseRoute', () => {
  const cases: [string, string, unknown, Route][] = [
    ['/', '', null, { name: 'root' }],
    ['/welcome', '', null, { name: 'welcome', invite: null, signin: null }],
    ['/welcome', '?invite=ABCD-EFGH', null, { name: 'welcome', invite: 'ABCD-EFGH', signin: null }],
    ['/welcome', '?invite=', null, { name: 'welcome', invite: null, signin: null }],
    ['/welcome', '?invite=a%20b', null, { name: 'welcome', invite: 'a b', signin: null }],
    ['/welcome', '?signin=race', null, { name: 'welcome', invite: null, signin: 'race' }],
    ['/welcome', '?invite=ABCD-EFGH&signin=race', null, { name: 'welcome', invite: 'ABCD-EFGH', signin: 'race' }],
    ['/welcome', '?signin=evicted', null, { name: 'welcome', invite: null, signin: 'evicted' }],
    ['/welcome', '?signin=other', null, { name: 'welcome', invite: null, signin: null }],
    [`/c/${C}`, '', null, { name: 'channel', communityId: C, channelId: null }],
    [`/c/${C}/${CH}`, '?x=1', null, { name: 'channel', communityId: C, channelId: CH }],
    [`/c/${C.toUpperCase()}`, '', null, { name: 'root' }],
    [`/c/${C}/`, '', null, { name: 'root' }],
    [`/c/${C}/${CH}/x`, '', null, { name: 'root' }],
    ['/c/abc', '', null, { name: 'root' }],
    ['/welcome/', '', null, { name: 'root' }],
    ['/v1/instance', '', null, { name: 'root' }],
    [`/dm/${DM}`, '', null, { name: 'dm', channelId: DM }],
    [`/dm/${DM.toUpperCase()}`, '', null, { name: 'root' }],
    ['/dm/', '', null, { name: 'root' }],
    [`/dm/${DM}/x`, '', null, { name: 'root' }],
    ['/settings', '', null, { name: 'settings', section: null, from: null }],
    ['/settings/devices', '', { settingsFrom: `/c/${C}/${CH}` }, { name: 'settings', section: 'devices', from: `/c/${C}/${CH}` }],
    ['/settings/notifications', '', { settingsFrom: `/dm/${DM}` }, { name: 'settings', section: 'notifications', from: `/dm/${DM}` }],
    ['/settings/appearance', '', { settingsFrom: '/welcome?invite=X' }, { name: 'settings', section: 'appearance', from: '/welcome?invite=X' }],
    ['/settings/devices', '', { settingsFrom: '/settings/appearance' }, { name: 'settings', section: 'devices', from: null }],
    ['/settings/devices', '', { settingsFrom: '//evil.example/x' }, { name: 'settings', section: 'devices', from: null }],
    ['/settings/devices', '', { settingsFrom: 'https://evil.example/' }, { name: 'settings', section: 'devices', from: null }],
    ['/settings/devices', '', { settingsFrom: 7 }, { name: 'settings', section: 'devices', from: null }],
    ['/settings/privacy', '', null, { name: 'settings', section: 'privacy', from: null }],
    ['/settings/keyboard', '', null, { name: 'settings', section: 'keyboard', from: null }],
    ['/settings/voice', '', null, { name: 'settings', section: 'voice', from: null }],
    ['/settings/privacy', '', { settingsFrom: null, compareUser: 'ab'.repeat(16) }, { name: 'settings', section: 'privacy', from: null, compare: 'ab'.repeat(16) }],
    ['/settings/privacy', '', { settingsFrom: null, compareUser: 'not-hex' }, { name: 'settings', section: 'privacy', from: null }],
    ['/settings/diagnostics', '', null, { name: 'settings', section: 'diagnostics', from: null }],
    ['/settings/bot/music/spotify/callback', '?code=one&state=two', null,
      { name: 'spotifyCallback', search: '?code=one&state=two' }],
    ['/settings/signin', '', null, { name: 'settings', section: 'signin', from: null }],
    ['/settings/unrecognized', '', null, { name: 'settings', section: null, from: null }],
    [`/c/${C}/settings`, '', { settingsFrom: `/c/${CH}/settings/roles` }, { name: 'serverSettings', communityId: C, section: null, from: null }],
    [`/c/${C}/settings/channels`, '', { settingsFrom: `/c/${CH}/channels` }, { name: 'serverSettings', communityId: C, section: 'channels', from: `/c/${CH}/channels` }],
    [`/c/${C}/settings/roles`, '', null, { name: 'serverSettings', communityId: C, section: 'roles', from: null }],
    [`/c/${C}/settings/future`, '', null, { name: 'serverSettings', communityId: C, section: null, from: null }],
    ['/settings/', '', null, { name: 'root' }],
  ];
  it.each(cases)('%s%s', (path, search, state, want) => {
    expect(parseRoute(path, search, state)).toEqual(want);
  });
  it('defaults the state to none', () => {
    expect(parseRoute('/settings/devices', '')).toEqual({ name: 'settings', section: 'devices', from: null });
  });
});

describe('routePath', () => {
  it('inverts parseRoute', () => {
    expect(routePath({ name: 'root' })).toBe('/');
    expect(routePath({ name: 'welcome', invite: null, signin: null })).toBe('/welcome');
    expect(routePath({ name: 'welcome', invite: 'a b&c', signin: null })).toBe('/welcome?invite=a%20b%26c');
    expect(routePath({ name: 'welcome', invite: 'a b&c', signin: 'race' })).toBe('/welcome?invite=a%20b%26c&signin=race');
    expect(routePath({ name: 'welcome', invite: null, signin: 'race' })).toBe('/welcome?signin=race');
    expect(routePath({ name: 'welcome', invite: null, signin: 'evicted' })).toBe('/welcome?signin=evicted');
    expect(routePath({ name: 'channel', communityId: C, channelId: null })).toBe(`/c/${C}`);
    expect(routePath({ name: 'channel', communityId: C, channelId: CH })).toBe(`/c/${C}/${CH}`);
    expect(routePath({ name: 'dm', channelId: DM })).toBe(`/dm/${DM}`);
    expect(routePath({ name: 'settings', section: 'notifications', from: '/x' })).toBe('/settings/notifications');
    expect(routePath({ name: 'settings', section: null, from: null })).toBe('/settings');
    expect(routePath({ name: 'serverSettings', communityId: C, section: null, from: null })).toBe(`/c/${C}/settings`);
    expect(routePath({ name: 'serverSettings', communityId: C, section: 'roles', from: null })).toBe(`/c/${C}/settings/roles`);
    expect(routePath({ name: 'spotifyCallback', search: '?code=x' })).toBe('/settings/bot/music/spotify/callback?code=x');
    expect(routeOf('/welcome?invite=a%20b%26c')).toEqual({ name: 'welcome', invite: 'a b&c', signin: null });
    expect(routeOf('/welcome?signin=race')).toEqual({ name: 'welcome', invite: null, signin: 'race' });
    expect(routeOf('/welcome?invite=a%20b%26c&signin=race')).toEqual({ name: 'welcome', invite: 'a b&c', signin: 'race' });
    expect(routeOf(`/c/${C}/${CH}`)).toEqual({ name: 'channel', communityId: C, channelId: CH });
    expect(routeOf('/settings/devices')).toEqual({ name: 'settings', section: 'devices', from: null });
    expect(routeOf(`/c/${C}/settings/roles`)).toEqual({ name: 'serverSettings', communityId: C, section: 'roles', from: null });
  });
});

function Probe() {
  const [route, go] = useRoute();
  return (
    <>
      <output>{JSON.stringify(route)}</output>
      <button type="button" onClick={() => { void go({ name: 'channel', communityId: C, channelId: null }); }}>push</button>
      <button type="button" onClick={() => { void go({ name: 'welcome', invite: 'X', signin: null }, true); }}>replace</button>
    </>
  );
}

describe('useRoute', () => {
  it('pushes, replaces and follows popstate', async () => {
    const user = userEvent.setup();
    render(<Probe />);
    expect(screen.getByRole('status')).toHaveTextContent('{"name":"root"}');
    const before = window.history.length;
    await user.click(screen.getByRole('button', { name: 'push' }));
    expect(window.location.pathname).toBe(`/c/${C}`);
    expect(window.history.length).toBe(before + 1);
    expect(screen.getByRole('status')).toHaveTextContent(`{"name":"channel","communityId":"${C}","channelId":null}`);
    await user.click(screen.getByRole('button', { name: 'push' }));
    expect(window.history.length).toBe(before + 1);
    await user.click(screen.getByRole('button', { name: 'replace' }));
    expect(window.location.pathname + window.location.search).toBe('/welcome?invite=X');
    expect(window.history.length).toBe(before + 1);
    act(() => { window.history.back(); });
    await waitFor(() => expect(window.location.pathname).toBe('/'));
    expect(screen.getByRole('status')).toHaveTextContent('{"name":"root"}');
  });

  it('publishes a replace when only history state changes', async () => {
    function StateProbe() {
      const [route, go] = useRoute();
      return <><output>{JSON.stringify(route)}</output><button type="button" onClick={() => { void go({ name: 'settings', section: 'privacy', from: '/you', compare: 'ab'.repeat(16) }, true); }}>compare</button></>;
    }
    const user = userEvent.setup();
    replaceHistoryState({ settingsFrom: '/you' }, '', '/settings/privacy');
    render(<StateProbe />);
    await user.click(screen.getByRole('button', { name: 'compare' }));
    expect(window.location.pathname + window.location.search).toBe('/settings/privacy');
    expect(screen.getByRole('status')).toHaveTextContent(`"compare":"${'ab'.repeat(16)}"`);
  });
});

describe('guarded browser traversal', () => {
  function CurrentRoute() {
    const [route] = useRoute();
    return <output>{route.name}</output>;
  }

  it('does not leave a pending traversal for an unindexed raw pop event', async () => {
    const dispose = setNavigationGuard(() => Promise.resolve(true));
    act(() => { window.dispatchEvent(new PopStateEvent('popstate', { state: null })); });
    const action = vi.fn();
    await expect(requestNavigation(action)).resolves.toBe(true);
    expect(action).toHaveBeenCalledTimes(1);
    dispose();
  });

  it('restores the visible entry before confirmation and replays back after discard', async () => {
    replaceHistoryState(null, '', '/you');
    const originPath = '/you';
    const originState: unknown = window.history.state as unknown;
    pushHistoryState(null, '', '/activity');
    const targetPath = '/activity';
    const targetState: unknown = window.history.state as unknown;
    const go = vi.spyOn(window.history, 'go').mockImplementation(delta => {
      const restore = (delta ?? 0) > 0;
      window.history.replaceState(restore ? targetState : originState, '', restore ? targetPath : originPath);
      window.dispatchEvent(new PopStateEvent('popstate', { state: restore ? targetState : originState }));
    });
    let resolveGuard!: (allow: boolean) => void;
    const dispose = setNavigationGuard(() => new Promise(resolve => { resolveGuard = resolve; }));
    render(<CurrentRoute />);

    act(() => {
      window.history.replaceState(originState, '', originPath);
      window.dispatchEvent(new PopStateEvent('popstate', { state: originState }));
    });
    await waitFor(() => expect(go).toHaveBeenCalledWith(1));
    expect(window.location.pathname).toBe(targetPath);
    expect(screen.getByRole('status')).toHaveTextContent('activity');

    await act(async () => { resolveGuard(true); await Promise.resolve(); });
    await waitFor(() => expect(go).toHaveBeenCalledWith(-1));
    expect(window.location.pathname).toBe(originPath);
    expect(screen.getByRole('status')).toHaveTextContent('you');
    dispose();
    go.mockRestore();
  });

  it('keeps the current entry after confirmation is declined', async () => {
    replaceHistoryState(null, '', '/you');
    const originPath = '/you';
    const originState: unknown = window.history.state as unknown;
    pushHistoryState(null, '', '/activity');
    const targetPath = '/activity';
    const targetState: unknown = window.history.state as unknown;
    const committed = vi.fn();
    const off = subscribeCommittedTraversal(committed);
    const go = vi.spyOn(window.history, 'go').mockImplementation(() => {
      window.history.replaceState(targetState, '', targetPath);
      window.dispatchEvent(new PopStateEvent('popstate', { state: targetState }));
    });
    const dispose = setNavigationGuard(() => Promise.resolve(false));
    render(<CurrentRoute />);
    act(() => {
      window.history.replaceState(originState, '', originPath);
      window.dispatchEvent(new PopStateEvent('popstate', { state: originState }));
    });
    await waitFor(() => expect(go).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(window.location.pathname).toBe(targetPath));
    expect(screen.getByRole('status')).toHaveTextContent('activity');
    expect(committed).not.toHaveBeenCalled();
    off();
    dispose();
    go.mockRestore();
  });

  it('assigns the next push the adjacent position after Back discards the forward branch', async () => {
    replaceHistoryState(null, '', '/start');
    pushHistoryState(null, '', '/a');
    const aIndex = (window.history.state as Record<string, number>).dillaHistoryIndex;
    pushHistoryState(null, '', '/b');
    window.history.back();
    await waitFor(() => expect(window.location.pathname).toBe('/a'));
    pushHistoryState(null, '', '/c');
    const cIndex = (window.history.state as Record<string, number>).dillaHistoryIndex;
    expect(cIndex).toBe(aIndex + 1);

    let resolveGuard!: (allow: boolean) => void;
    let guardStarted!: () => void;
    const started = new Promise<void>(resolve => { guardStarted = resolve; });
    const dispose = setNavigationGuard(() => { guardStarted(); return new Promise(resolve => { resolveGuard = resolve; }); });
    const committed = vi.fn();
    const off = subscribeCommittedTraversal(committed);
    window.history.back();
    await started;
    await waitFor(() => expect(window.location.pathname).toBe('/c'));
    expect(committed).not.toHaveBeenCalled();
    resolveGuard(true);
    await waitFor(() => expect(window.location.pathname).toBe('/a'));
    expect(committed).toHaveBeenCalledTimes(1);
    off();
    dispose();
  });
});

function SettingsProbe() {
  const [route, go] = useRoute();
  return (
    <>
      <output>{JSON.stringify(route)}</output>
      <button type="button" onClick={() => { void go({ name: 'settings', section: 'devices', from: null }); }}>open</button>
      <button type="button" onClick={() => { void go({ name: 'settings', section: 'appearance', from: 'ignored' }, true); }}>section</button>
    </>
  );
}

describe('the settings route', () => {
  it('remembers where it was opened from in one history entry', async () => {
    const user = userEvent.setup();
    replaceHistoryState(null, '', `/c/${C}/${CH}`);
    render(<SettingsProbe />);
    await user.click(screen.getByRole('button', { name: 'open' }));
    const afterOpen = window.history.length;
    expect(window.location.pathname).toBe('/settings/devices');
    expect(window.history.state).toMatchObject({ [SETTINGS_FROM]: `/c/${C}/${CH}` });
    expect(screen.getByRole('status')).toHaveTextContent(`{"name":"settings","section":"devices","from":"/c/${C}/${CH}"}`);
    await user.click(screen.getByRole('button', { name: 'section' }));
    expect(window.location.pathname).toBe('/settings/appearance');
    expect(window.history.state).toMatchObject({ [SETTINGS_FROM]: `/c/${C}/${CH}` });
    expect(window.history.length).toBe(afterOpen);
  });

  it('keeps the current non-settings origin when opening server settings', async () => {
    function ServerProbe() {
      const [route, go] = useRoute();
      return <><output>{JSON.stringify(route)}</output><button type="button" onClick={() => { void go({ name: 'serverSettings', communityId: C, section: 'roles', from: null }); }}>server settings</button></>;
    }
    const user = userEvent.setup();
    replaceHistoryState(null, '', `/c/${C}/${CH}`);
    render(<ServerProbe />);
    await user.click(screen.getByRole('button', { name: 'server settings' }));
    expect(window.location.pathname).toBe(`/c/${C}/settings/roles`);
    expect(window.history.state).toMatchObject({ [SETTINGS_FROM]: `/c/${C}/${CH}` });
    expect(screen.getByRole('status')).toHaveTextContent(`"name":"serverSettings"`);
  });
});

describe('baseRoute', () => {
  it('shows what settings was opened over', () => {
    expect(baseRoute({ name: 'settings', section: 'devices', from: `/c/${C}/${CH}` })).toEqual({ name: 'channel', communityId: C, channelId: CH });
    expect(baseRoute({ name: 'settings', section: 'notifications', from: null })).toEqual({ name: 'root' });
    expect(baseRoute({ name: 'dm', channelId: CH })).toEqual({ name: 'dm', channelId: CH });
  });
});

describe('the phone routes', () => {
  it('parses /dm, /activity and /you and writes them back', () => {
    expect(parseRoute('/dm', '')).toEqual({ name: 'dms' });
    expect(parseRoute('/activity', '')).toEqual({ name: 'activity' });
    expect(parseRoute('/you', '')).toEqual({ name: 'you' });
    expect(parseRoute('/dm/', '')).toEqual({ name: 'root' });
    expect(routePath({ name: 'dms' })).toBe('/dm');
    expect(routePath({ name: 'activity' })).toBe('/activity');
    expect(routePath({ name: 'you' })).toBe('/you');
    expect(routeOf('/you')).toEqual({ name: 'you' });
  });

  it('/dm is Home with the @ tile selected: it parses to dms, which belongs to the home tab', () => {
    expect(parseRoute('/dm', '')).toEqual({ name: 'dms' });
    expect(tabOf(parseRoute('/dm', ''))).toBe('home');
  });

  it('tabOf names the tab of every route', () => {
    expect(tabOf({ name: 'channel', communityId: C, channelId: null })).toBe('home');
    expect(tabOf({ name: 'root' })).toBe('home');
    expect(tabOf({ name: 'welcome', invite: null, signin: null })).toBe('home');
    expect(tabOf({ name: 'dm', channelId: DM })).toBe('home');
    expect(tabOf({ name: 'dms' })).toBe('home');
    expect(tabOf({ name: 'activity' })).toBe('activity');
    expect(tabOf({ name: 'you' })).toBe('you');
    expect(tabOf({ name: 'settings', section: 'devices', from: null })).toBe('you');
    expect(tabOf({ name: 'serverSettings', communityId: C, section: null, from: null })).toBe('you');
  });
});

function PhoneProbe() {
  const [route, go] = useRoute();
  return (
    <>
      <output>{JSON.stringify(route)}</output>
      <button type="button" onClick={() => { void go({ name: 'channel', communityId: C, channelId: CH }, false, { phoneBack: true }); }}>push back</button>
      <button type="button" onClick={() => { void go({ name: 'settings', section: 'devices', from: null }, false, { phoneBack: true }); }}>open settings</button>
      <button type="button" onClick={() => { void go({ name: 'you' }); }}>plain</button>
    </>
  );
}

describe('navigate with phoneBack', () => {
  it('writes phoneBack into the pushed state, and only when asked', async () => {
    const user = userEvent.setup();
    replaceHistoryState(null, '', '/you');
    render(<PhoneProbe />);
    await user.click(screen.getByRole('button', { name: 'push back' }));
    expect(window.history.state).toMatchObject({ [PHONE_BACK]: true, phoneFrom: '/you' });
    replaceHistoryState(null, '', '/activity');
    await user.click(screen.getByRole('button', { name: 'plain' }));
    expect(window.history.state).not.toHaveProperty(PHONE_BACK);
  });

  it('keeps settingsFrom beside phoneBack on a settings route', async () => {
    const user = userEvent.setup();
    replaceHistoryState(null, '', '/you');
    render(<PhoneProbe />);
    await user.click(screen.getByRole('button', { name: 'open settings' }));
    expect(window.history.state).toMatchObject({ [SETTINGS_FROM]: '/you', [PHONE_BACK]: true, phoneFrom: '/you' });
  });
});

describe('goBack', () => {
  it('goes back in history when the entry was pushed by the app', () => {
    replaceHistoryState({ [PHONE_BACK]: true }, '', `/c/${C}/${CH}`);
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    const navigate = vi.fn();
    goBack({ name: 'channel', communityId: C, channelId: null }, navigate);
    expect(back).toHaveBeenCalledTimes(1);
    expect(navigate).not.toHaveBeenCalled();
    back.mockRestore();
  });

  describe('with an expected route (N5: the system forward button)', () => {
    const expected: Route = { name: 'channel', communityId: C, channelId: null };
    const backTo = (path: string, state: unknown) => vi.spyOn(window.history, 'back').mockImplementation(() => {
      window.history.replaceState(state, '', path);
      window.dispatchEvent(new PopStateEvent('popstate', { state }));
    });

    const pushPhoneRoute = () => {
      replaceHistoryState(null, '', '/dm');
      const expectedState: unknown = window.history.state as unknown;
      pushHistoryState({ [PHONE_BACK]: true }, '', `/c/${C}/${CH}`);
      return expectedState;
    };

    it('goes to the expected route when back landed somewhere else (a forward entry)', () => {
      const state = pushPhoneRoute();
      const back = backTo('/dm', state);
      const navigate = vi.fn();
      goBack({ name: 'dms' }, navigate, expected);
      expect(navigate).toHaveBeenCalledTimes(1);
      expect(navigate).toHaveBeenCalledWith(expected, true);
      back.mockRestore();
    });

    it('does nothing more when back landed on the expected route (control)', () => {
      const state = pushPhoneRoute();
      const back = backTo(routePath(expected), state);
      const navigate = vi.fn();
      goBack({ name: 'dms' }, navigate, expected);
      expect(navigate).not.toHaveBeenCalled();
      back.mockRestore();
    });

    it('listens once: a later popstate is not corrected', () => {
      const state = pushPhoneRoute();
      const back = backTo(routePath(expected), state);
      const navigate = vi.fn();
      goBack({ name: 'dms' }, navigate, expected);
      replaceHistoryState(null, '', '/dm');
      window.dispatchEvent(new PopStateEvent('popstate', { state: null }));
      expect(navigate).not.toHaveBeenCalled();
      back.mockRestore();
    });

    it('adds no listener on the deep-link path', () => {
      replaceHistoryState(null, '', `/c/${C}/${CH}`);
      const navigate = vi.fn();
      goBack(expected, navigate, expected);
      expect(navigate).toHaveBeenCalledWith(expected, true);
    });

    it('does not correct an attempted Back until the traversal guard accepts it', async () => {
      replaceHistoryState(null, '', '/dm');
      pushHistoryState({ [PHONE_BACK]: true }, '', `/c/${C}/${CH}`);
      const navigate = vi.fn();
      const dispose = setNavigationGuard(() => Promise.resolve(false));
      const off = goBack({ name: 'dms' }, navigate, expected);
      await waitFor(() => expect(window.location.pathname).toBe(`/c/${C}/${CH}`));
      expect(navigate).not.toHaveBeenCalled();
      off();
      dispose();
    });
  });

  it('replaces with the fallback on a deep link', () => {
    replaceHistoryState(null, '', `/c/${C}/${CH}`);
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    const navigate = vi.fn();
    const fallback: Route = { name: 'channel', communityId: C, channelId: null };
    goBack(fallback, navigate);
    expect(back).not.toHaveBeenCalled();
    expect(navigate).toHaveBeenCalledWith(fallback, true);
    back.mockRestore();
  });
});
