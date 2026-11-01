import { afterEach, describe, it, expect, vi } from 'vitest';
import { StrictMode } from 'react';
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { DmSummary, Notice, SliceName } from '@dilla/client-core';
import { CoreProvider } from './core/context.tsx';
import { FakeClient, refusal } from './test/fake-client.ts';
import { account } from './test/fixtures.ts';
import { browser } from './browser.ts';
import { readJoinError, replaceHistoryState } from './router.ts';
import { App, screenFor } from './App.tsx';
import { requestHandover } from './handover.ts';
import { spotifySession } from './screens/admin/spotify-link.ts';

vi.mock('./handover.ts', () => ({ requestHandover: vi.fn(async () => {}) }));

afterEach(() => { cleanup(); replaceHistoryState(null, '', '/'); });

describe('screenFor', () => {
  it('routes every phase', () => {
    expect(screenFor(undefined)).toBe('boot');
    for (const p of ['loading', 'unsupported', 'other-tab', 'store-lost', 'revoked', 'error'] as const) expect(screenFor(p)).toBe('boot');
    for (const p of ['needs-signup', 'signup-keys', 'registering'] as const) expect(screenFor(p)).toBe('onboarding');
    expect(screenFor('ready')).toBe('shell');
    for (const p of ['signin-login', 'signin-totp', 'signin-key', 'enrolling', 'signin-passkey'] as const) expect(screenFor(p)).toBe('signin');
    expect(screenFor('cleared')).toBe('boot');
  });
  it('routes the factor-setup phase by its origin', () => {
    expect(screenFor('factor-setup', 'signup')).toBe('onboarding');
    expect(screenFor('factor-setup', 'signin')).toBe('signin');
    expect(screenFor('factor-setup', 'required')).toBe('factor');
    // No origin yet is the forced screen: an account found owing is never let past it.
    expect(screenFor('factor-setup')).toBe('factor');
    expect(screenFor('factor-setup', null)).toBe('factor');
    // The origin only matters in that phase.
    expect(screenFor('ready', 'signup')).toBe('shell');
    expect(screenFor('signin-login', 'required')).toBe('signin');
  });
});

describe('App', () => {
  it('renders the Spotify callback screen on its canonical deep-link path', async () => {
    const fake = new FakeClient();
    fake.set('account', account());
    const state = 'csrf-app-test';
    spotifySession.setItem('dilla.spotifyAttempt', JSON.stringify({ communityId: '11'.repeat(16),
      attemptId: '22'.repeat(16), state, expires: `${Math.floor(Date.now() / 1000) + 600}`,
      instanceId: account().instance!.id, userId: account().user!.id, deviceId: account().deviceId }));
    replaceHistoryState(null, '', `/settings/bot/music/spotify/callback?code=oauth-code&state=${state}`);
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    await waitFor(() => expect(window.location.pathname).toBe(`/c/${'11'.repeat(16)}/settings/music`));
    expect(screen.queryByText('Linked')).not.toBeInTheDocument();
    expect(fake.callsOf('callbackBotSpotifyAttempt')).toHaveLength(1);
  });

  it('scrubs the callback code before sign-in, retains it in memory, and delivers after sign-in finishes', async () => {
    const fake = new FakeClient();
    const signedIn = account();
    fake.set('communities', [{ id: '11'.repeat(16), name: 'Midgard', owner: 'ee'.repeat(16) }]);
    const communityId = '11'.repeat(16);
    fake.set(`channels:${communityId}` as SliceName, []);
    fake.set(`permissions:${communityId}` as SliceName, { top: 0, owner: false, manageCommunity: true, manageChannels: false,
      kickMembers: false, banMembers: false, createInvite: false, manageRoles: false, loaded: true, overwritesLoaded: true,
      everyoneId: 'ab'.repeat(16) });
    fake.set('account', account({ phase: 'signin-login', signIn: { username: null, factor: null } }));
    fake.handler = command => {
      if (command.m === 'signInLogin') {
        fake.set('account', { ...fake.get('account')!, phase: 'signin-key', signIn: { username: command.username, factor: null } });
        return Promise.resolve({ needs: 0 });
      }
      if (command.m === 'signInKey') {
        fake.set('account', { ...signedIn, phase: 'ready' });
        return Promise.resolve(null);
      }
      if (command.m === 'callbackBotSpotifyAttempt') return Promise.resolve(null);
      return Promise.resolve(null);
    };
    const state = 'csrf-signin-test';
    spotifySession.setItem('dilla.spotifyAttempt', JSON.stringify({ communityId: '11'.repeat(16),
      attemptId: '22'.repeat(16), state, expires: `${Math.floor(Date.now() / 1000) + 600}`,
      instanceId: signedIn.instance!.id, userId: signedIn.user!.id, deviceId: signedIn.deviceId }));
    replaceHistoryState(null, '', `/settings/bot/music/spotify/callback?code=oauth-code&state=${state}`);
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(window.location.search).toBe('');
    expect(screen.getByText('Sign in to dilla.test')).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.click(screen.getByRole('textbox', { name: 'Recovery key' }));
    await user.paste('7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA');
    await user.click(screen.getByRole('button', { name: 'Add this browser' }));
    await user.click(screen.getByRole('button', { name: 'Open dilla' }));
    await waitFor(() => expect(window.location.pathname).toBe(`/c/${'11'.repeat(16)}/settings/music`));
    expect(screen.queryByText('Linked')).not.toBeInTheDocument();
    expect(window.location.pathname).toBe(`/c/${'11'.repeat(16)}/settings/music`);
    expect(fake.callsOf('callbackBotSpotifyAttempt')).toHaveLength(1);
  });

  it('keeps waiting for release, then offers retry when hand-over is refused', async () => {
    let fail!: (error: Error) => void;
    vi.mocked(requestHandover).mockImplementationOnce(() => new Promise((_resolve, reject) => { fail = reject; }));
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'other-tab' }));
    fake.set('diagnostics', { connection: 'offline', generation: null, instance: 'ab'.repeat(16), devices: 0, errors: [] });
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    await user.click(screen.getByRole('button', { name: 'Use this tab' }));
    expect(screen.getByRole('status')).toHaveTextContent('Moving to this tab…');
    expect(screen.queryByRole('button', { name: 'Use this tab' })).toBeNull();
    await act(async () => { fail(new Error('resign refused')); await Promise.resolve(); });
    expect(screen.getByRole('status')).toHaveTextContent('Close the other tab, then try again.');
    await user.click(screen.getByRole('button', { name: 'Use this tab' }));
    expect(requestHandover).toHaveBeenCalledTimes(2);
  });
  it('wires the other-tab action to the exact instance and a follower start', async () => {
    vi.mocked(requestHandover).mockClear();
    const user = userEvent.setup();
    const fake = new FakeClient();
    const instance = 'ab'.repeat(16);
    fake.set('account', account({ phase: 'other-tab' }));
    fake.set('diagnostics', { connection: 'offline', generation: null, instance, devices: 0, errors: [] });
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    await user.click(screen.getByRole('button', { name: 'Use this tab' }));
    expect(requestHandover).toHaveBeenCalledOnce();
    expect(vi.mocked(requestHandover).mock.calls[0][0]).toBe(instance);
    expect(vi.mocked(requestHandover).mock.calls[0][1]).toEqual(expect.any(Function));
    expect(vi.mocked(requestHandover).mock.calls[0][2]).toEqual(expect.any(Function));
    expect(screen.queryByRole('log')).toBeNull();
  });
  it('focuses the paused view before the shell leaves, resigns once and forwards the exact release', async () => {
    const channels: FakeChannel[] = [];
    class FakeChannel {
      readonly listeners = new Set<(event: { data: unknown }) => void>();
      readonly posted: unknown[] = [];
      closed = false;
      constructor(readonly name: string) { channels.push(this); }
      postMessage(value: unknown): void { this.posted.push(value); }
      addEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.add(cb); }
      removeEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.delete(cb); }
      close(): void { this.closed = true; }
    }
    vi.stubGlobal('BroadcastChannel', FakeChannel);
    try {
      const instance = 'ab'.repeat(16);
      const nonce = '01'.repeat(16);
      let finish!: (value: unknown) => void;
      const fake = new FakeClient();
      fake.handler = c => c.m === 'resign' ? new Promise(resolve => { finish = resolve; }) : Promise.resolve(null);
      fake.set('account', account({ phase: 'ready' }));
      fake.set('communities', []);
      fake.set('diagnostics', { connection: 'online', generation: '7', instance, devices: 1, errors: [] });
      render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
      const leader = channels.find(c => c.name === `dilla-core:${instance}` && c.listeners.size > 0)!;
      const takeover = (n: string) => { for (const cb of [...leader.listeners]) cb({ data: { t: 'takeover', instance, nonce: n } }); };
      act(() => takeover(nonce));
      // M-08: focus goes to the paused heading Flow 05 names, not the full-screen wrapper.
      const paused = screen.getByRole('heading', { name: 'This tab is paused' });
      expect(document.activeElement).toBe(paused);
      expect(screen.getByRole('heading', { name: 'This tab is paused' })).toBeInTheDocument();
      expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument();
      act(() => takeover('02'.repeat(16)));
      expect(fake.callsOf('resign')).toEqual([{ m: 'resign', nonce }]);
      await act(async () => { fake.set('account', account({ phase: 'other-tab' })); finish({ released: true }); await Promise.resolve(); });
      expect(screen.queryByRole('navigation', { name: 'servers' })).toBeNull();
      expect(screen.getByRole('heading', { name: 'This tab is paused' })).toBe(paused);
      expect(document.activeElement).toBe(paused);
      // P6 / M-07: the paused tab says what happened and can take dilla back.
      expect(screen.getByText('dilla moved to another tab. This tab takes over when that one closes, or you can use it here now.')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: 'Use this tab' })).toBeInTheDocument();
      // The voice-presence channel (product walk I7) is its own; the hand-over's channel forwards only the release.
      const forwarded = channels.filter(c => c.name.startsWith('dilla-core:') && c.posted.length > 0);
      expect(forwarded.map(c => [c.name, c.posted, c.closed])).toEqual([[`dilla-core:${instance}`, [{ t: 'released', instance, nonce }], true]]);
    } finally { vi.unstubAllGlobals(); }
  });
  // C1 hand-back: a released tab whose worker leads again (the other tab handed back or closed) shows the shell again.
  it('clears the paused view when the released tab is ready again, and hands back from it', async () => {
    const channels: { name: string; listeners: Set<(event: { data: unknown }) => void> }[] = [];
    class FakeChannel {
      readonly listeners = new Set<(event: { data: unknown }) => void>();
      constructor(readonly name: string) { channels.push(this); }
      postMessage(): void {}
      addEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.add(cb); }
      removeEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.delete(cb); }
      close(): void {}
    }
    vi.stubGlobal('BroadcastChannel', FakeChannel);
    vi.mocked(requestHandover).mockClear();
    try {
      const instance = 'ab'.repeat(16);
      const fake = new FakeClient();
      fake.handler = c => c.m === 'resign' ? Promise.resolve({ released: true }) : Promise.resolve(null);
      fake.set('account', account({ phase: 'ready' }));
      fake.set('communities', []);
      fake.set('diagnostics', { connection: 'online', generation: '7', instance, devices: 1, errors: [] });
      const user = userEvent.setup();
      render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
      const leader = channels.find(c => c.name === `dilla-core:${instance}` && c.listeners.size > 0)!;
      await act(async () => {
        for (const cb of [...leader.listeners]) cb({ data: { t: 'takeover', instance, nonce: '01'.repeat(16) } });
        fake.set('account', account({ phase: 'other-tab' }));
        await Promise.resolve();
      });
      expect(screen.getByRole('heading', { name: 'This tab is paused' })).toBeInTheDocument();
      await user.click(screen.getByRole('button', { name: 'Use this tab' }));
      expect(requestHandover).toHaveBeenCalledOnce();
      expect(vi.mocked(requestHandover).mock.calls[0][0]).toBe(instance);
      expect(screen.getByRole('status')).toHaveTextContent('Moving to this tab…');
      act(() => fake.set('account', account({ phase: 'loading' })));
      act(() => fake.set('account', account({ phase: 'ready' })));
      expect(screen.queryByRole('heading', { name: 'This tab is paused' })).toBeNull();
      expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument();
      // A second takeover pauses it again (the resign guard was reset).
      const again = channels.filter(c => c.name === `dilla-core:${instance}` && c.listeners.size > 0).at(-1)!;
      await act(async () => {
        for (const cb of [...again.listeners]) cb({ data: { t: 'takeover', instance, nonce: '02'.repeat(16) } });
        await Promise.resolve();
      });
      expect(fake.callsOf('resign')).toHaveLength(2);
    } finally { vi.unstubAllGlobals(); }
  });
  // Walk M9: "Use this tab" in a second tab failed while this one was on the forced screen: nothing here listened.
  it('hands over from the forced screen, and shows it again once this tab leads again', async () => {
    const channels: { name: string; posted: unknown[]; listeners: Set<(event: { data: unknown }) => void> }[] = [];
    class FakeChannel {
      readonly listeners = new Set<(event: { data: unknown }) => void>();
      readonly posted: unknown[] = [];
      constructor(readonly name: string) { channels.push(this); }
      postMessage(value: unknown): void { this.posted.push(value); }
      addEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.add(cb); }
      removeEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.delete(cb); }
      close(): void {}
    }
    vi.stubGlobal('BroadcastChannel', FakeChannel);
    try {
      const instance = 'ab'.repeat(16);
      const nonce = '01'.repeat(16);
      const fake = new FakeClient();
      fake.handler = c => c.m === 'resign' ? Promise.resolve({ released: true }) : Promise.resolve(null);
      fake.set('account', account({ phase: 'factor-setup', factorSetup: { origin: 'required' } }));
      fake.set('factors', { owed: true, totp: null, passkeys: [], backupCodesLeft: 0, hasPassword: true });
      fake.set('diagnostics', { connection: 'offline', generation: null, instance, devices: 1, errors: [] });
      render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
      expect(screen.getByRole('heading', { level: 1, name: 'Add a second factor' })).toBeInTheDocument();
      const leader = channels.find(c => c.name === `dilla-core:${instance}` && c.listeners.size > 0);
      expect(leader).toBeDefined();
      await act(async () => {
        for (const cb of [...leader!.listeners]) cb({ data: { t: 'takeover', instance, nonce } });
        fake.set('account', account({ phase: 'other-tab' }));
        await Promise.resolve();
      });
      expect(fake.callsOf('resign')).toEqual([{ m: 'resign', nonce }]);
      expect(screen.getByRole('heading', { name: 'This tab is paused' })).toBeInTheDocument();
      const forwarded = channels.filter(c => c.name.startsWith('dilla-core:') && c.posted.length > 0);
      expect(forwarded.map(c => c.posted)).toEqual([[{ t: 'released', instance, nonce }]]);
      act(() => fake.set('account', account({ phase: 'loading' })));
      act(() => fake.set('account', account({ phase: 'factor-setup', factorSetup: { origin: 'required' } })));
      expect(screen.queryByRole('heading', { name: 'This tab is paused' })).toBeNull();
      expect(screen.getByRole('heading', { level: 1, name: 'Add a second factor' })).toBeInTheDocument();
    } finally { vi.unstubAllGlobals(); }
  });
  // Product walk I7: "Use this tab" ended the other tab's call without a word. The tab that holds dilla says, content-free,
  // whether it is in a call; the waiting tab warns before taking over, and keeping the call takes nothing over.
  describe('a call in the other tab (product walk I7)', () => {
    const channels: VoiceChannel[] = [];
    class VoiceChannel {
      readonly listeners = new Set<(event: { data: unknown }) => void>();
      readonly posted: unknown[] = [];
      constructor(readonly name: string) { channels.push(this); }
      postMessage(value: unknown): void {
        this.posted.push(value);
        for (const other of channels) if (other !== this && other.name === this.name) for (const cb of [...other.listeners]) cb({ data: value });
      }
      addEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.add(cb); }
      removeEventListener(_type: string, cb: (event: { data: unknown }) => void): void { this.listeners.delete(cb); }
      close(): void { this.listeners.clear(); }
    }
    const instance = 'ab'.repeat(16);

    it('the waiting tab warns before ending the call, and keeping it takes nothing over', async () => {
      channels.length = 0;
      vi.stubGlobal('BroadcastChannel', VoiceChannel);
      vi.mocked(requestHandover).mockClear();
      try {
        const fake = new FakeClient();
        fake.set('account', account({ phase: 'other-tab' }));
        fake.set('diagnostics', { connection: 'offline', generation: null, instance, devices: 0, errors: [] });
        const user = userEvent.setup();
        render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
        const asked = channels.find(c => c.name === `dilla-voice:${instance}`)!;
        expect(asked.posted).toEqual([{ t: 'voice?', instance }]);
        // The tab that holds dilla answers: it is in a call.
        act(() => { for (const cb of [...asked.listeners]) cb({ data: { t: 'voice', instance, on: true } }); });
        await user.click(screen.getByRole('button', { name: 'Use this tab' }));
        expect(requestHandover).not.toHaveBeenCalled();
        expect(screen.getByRole('status')).toHaveTextContent('You are in a call in the other tab. Using this tab ends that call.');
        expect(screen.getByRole('button', { name: 'Keep the call' })).toHaveFocus();
        await user.click(screen.getByRole('button', { name: 'Keep the call' }));
        expect(requestHandover).not.toHaveBeenCalled();
        // Fix round 3 (m3): focus returns to Use this tab, never to the page.
        expect(screen.getByRole('button', { name: 'Use this tab' })).toHaveFocus();
        await user.click(screen.getByRole('button', { name: 'Use this tab' }));
        await user.click(screen.getByRole('button', { name: 'Leave the call and use this tab' }));
        expect(requestHandover).toHaveBeenCalledOnce();
      } finally { vi.unstubAllGlobals(); }
    });

    // Fix round 3 (m6): with no call in the other tab, Use this tab takes over at once, with no question.
    it('with no call in the other tab, Use this tab takes over at once', async () => {
      channels.length = 0;
      vi.stubGlobal('BroadcastChannel', VoiceChannel);
      vi.mocked(requestHandover).mockClear();
      try {
        const fake = new FakeClient();
        fake.set('account', account({ phase: 'other-tab' }));
        fake.set('diagnostics', { connection: 'offline', generation: null, instance, devices: 0, errors: [] });
        const user = userEvent.setup();
        render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
        const asked = channels.find(c => c.name === `dilla-voice:${instance}`)!;
        act(() => { for (const cb of [...asked.listeners]) cb({ data: { t: 'voice', instance, on: false } }); });
        await user.click(screen.getByRole('button', { name: 'Use this tab' }));
        expect(requestHandover).toHaveBeenCalledOnce();
        expect(screen.queryByText('You are in a call in the other tab. Using this tab ends that call.')).toBeNull();
      } finally { vi.unstubAllGlobals(); }
    });

    it('the tab that holds dilla answers whether it is in a call, and says so when that changes', () => {
      channels.length = 0;
      vi.stubGlobal('BroadcastChannel', VoiceChannel);
      try {
        const fake = new FakeClient();
        fake.set('account', account({ phase: 'ready' }));
        fake.set('communities', []);
        fake.set('diagnostics', { connection: 'online', generation: '7', instance, devices: 1, errors: [] });
        fake.set('call', { phase: 'in', channelId: 'cd'.repeat(16), callId: 'cd'.repeat(16), epoch: '1', code: null, error: null, roster: [],
          outgoing: null, changedUser: null, rejoining: false });
        render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
        const waiting = new VoiceChannel(`dilla-voice:${instance}`);
        const heard: unknown[] = [];
        waiting.addEventListener('message', e => { heard.push(e.data); });
        act(() => waiting.postMessage({ t: 'voice?', instance }));
        expect(heard).toEqual([{ t: 'voice', instance, on: true }]);
        act(() => fake.set('call', { phase: 'idle', channelId: null, callId: null, epoch: null, code: null, error: null, roster: [],
          outgoing: null, changedUser: null, rejoining: false }));
        expect(heard.at(-1)).toEqual({ t: 'voice', instance, on: false });
      } finally { vi.unstubAllGlobals(); }
    });
  });
  it('starts the core once and shows the loading splash', () => {
    const fake = new FakeClient();
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(fake.callsOf('start')).toEqual([{ m: 'start' }]);
    expect(screen.getByRole('status')).toHaveTextContent('Starting dilla');
  });
  it('follows the account phase', () => {
    const fake = new FakeClient();
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    act(() => fake.set('account', account({ phase: 'other-tab' })));
    // Web-2c requirement 5 and the other-tab ruling: TabHandover's status region explains the state with web-2b's
    // boot.otherTab.status, shown as its heading; the tabs.use action stays the button.
    expect(screen.getByRole('status')).toHaveTextContent('dilla is open in another tab');
    expect(screen.getByRole('button', { name: 'Use this tab' })).toBeInTheDocument();
  });
  it('shows a failed start', async () => {
    const fake = new FakeClient();
    fake.handler = () => Promise.reject(refusal({ code: 'E_NETWORK' }));
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(await screen.findByText(/Error E_NETWORK\./)).toBeInTheDocument();
  });
  it('shows a fatal error whatever the phase', () => {
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'ready' }));
    render(<CoreProvider client={fake}><App fatal={{ code: 'E_WORKER', detail: '', status: 0, retryAfterMs: null }} /></CoreProvider>);
    expect(screen.getByRole('status')).toHaveTextContent('Error E_WORKER. Reload the page to try again.');
  });
});

describe('App durable storage', () => {
  it('asks the browser to keep its storage once, when the account first becomes ready', () => {
    const persist = vi.spyOn(browser, 'persistStorage').mockResolvedValue(true);
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'loading', user: null }));
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(persist).not.toHaveBeenCalled();
    act(() => fake.set('account', account({ phase: 'signup-keys', user: null })));
    expect(persist).not.toHaveBeenCalled();
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(persist).toHaveBeenCalledTimes(1);
    act(() => fake.set('account', account({ phase: 'ready', deviceId: 'dd'.repeat(16) })));
    expect(persist).toHaveBeenCalledTimes(1);
    persist.mockRestore();
  });
  it('changes nothing on screen when the browser refuses or the call rejects', async () => {
    const persist = vi.spyOn(browser, 'persistStorage').mockRejectedValue(new Error('not allowed'));
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'ready' }));
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    await act(async () => { await Promise.resolve(); });
    expect(persist).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(/^Error /)).toBeNull();
    persist.mockRestore();
  });
});

describe('App onboarding', () => {
  // Signs up through the whole ceremony with a FakeClient whose signupSubmit resolves `result`,
  // turns the account ready, presses Open dilla and returns the location the App navigated to.
  // No communities slice is set, so the shell of task 24 (rendered from then on) never redirects.
  async function signUpWith(result: { communityId: string | null; joinError: unknown }): Promise<string> {
    const user = userEvent.setup();
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'needs-signup', user: null }));
    fake.handler = c => {
      if (c.m === 'signupBegin') fake.set('account', account({ phase: 'signup-keys', user: null,
        recoveryKey: ['7K2M', 'QX9D', 'H4TB', 'R8NW', 'C3VF', 'J6PZ', 'A1GE', 'Y5KS', 'M0QT', 'B7XH', 'W2DN', 'F9RC', 'P4ZA'] }));
      return Promise.resolve(c.m === 'signupSubmit' ? result : null);
    };
    replaceHistoryState(null, '', '/welcome?invite=ABCD-EFGH');
    const view = render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Join dilla.test');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.click(screen.getByRole('checkbox', { name: 'I have written down or printed my recovery key' }));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await user.click(screen.getByRole('button', { name: 'Create account' }));
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('You’re in');
    const depth = window.history.length;
    await user.click(screen.getByRole('button', { name: 'Open dilla' }));
    expect(screen.queryByRole('heading', { name: 'You’re in' })).toBeNull();
    expect(window.history.length).toBe(depth);
    const at = window.location.pathname + window.location.search;
    view.unmount();
    return at;
  }
  it('lands at / when the invite named no server', async () => {
    expect(await signUpWith({ communityId: null, joinError: null })).toBe('/');
  });
  it('lands in the server the invite named', async () => {
    const C = 'c4'.repeat(16);
    expect(await signUpWith({ communityId: C, joinError: null })).toBe(`/c/${C}`);
  });
  it('brings a refused join back to the invite, for the join dialog', async () => {
    const joinError = { code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null };
    expect(await signUpWith({ communityId: null, joinError })).toBe('/welcome?invite=ABCD-EFGH');
  });
  // Pre-flight ruling (e): the refused join travels in the history entry's state, so the join dialog
  // of task 24 opens with it shown (initialError). The server's detail text is not carried.
  it('carries the refused join in the history state, without its detail', async () => {
    const joinError = { code: 'E_INVITE_INVALID', detail: 'spent', status: 410, retryAfterMs: null };
    expect(await signUpWith({ communityId: null, joinError })).toBe('/welcome?invite=ABCD-EFGH');
    expect(window.history.state).toMatchObject({ joinError: { code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null } });
    expect(readJoinError(window.history.state)).toEqual({ code: 'E_INVITE_INVALID', detail: '', status: 410, retryAfterMs: null });
  });
  it('leaves no join error in the history state after a join or without one', async () => {
    expect(await signUpWith({ communityId: 'c4'.repeat(16), joinError: null })).toBe(`/c/${'c4'.repeat(16)}`);
    expect(readJoinError(window.history.state)).toBeNull();
    expect(readJoinError(undefined)).toBeNull();
    expect(readJoinError({ joinError: 'E_X' })).toBeNull();
  });
  it('never shows onboarding to an account that is already ready', () => {
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'ready' }));
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(screen.queryByRole('heading', { name: /Join/ })).toBeNull();
  });
});

describe('App shell', () => {
  it('shows the shell to a ready account', () => {
    const fake = new FakeClient();
    fake.set('account', account({ phase: 'ready' }));
    fake.set('communities', []);
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'You are not in a server yet' })).toBeInTheDocument();
  });
});

describe('App sign-in', () => {
  function signInWorker(fake: FakeClient) {
    const move = (over: Partial<ReturnType<typeof account>>) => fake.set('account', { ...fake.get('account')!, ...over });
    fake.handler = c => {
      if (c.m === 'signInBegin') move({ phase: 'signin-login', signIn: { username: null, factor: null } });
      // web3.8 (R6): the login holds the assertion and waits for the key; the key adds this browser.
      if (c.m === 'signInLogin') move({ phase: 'signin-key', signIn: { username: c.username, factor: null } });
      if (c.m === 'signInKey') move({ phase: 'ready', user: { id: 'bb'.repeat(16), username: 'ada' }, signIn: null });
      if (c.m === 'signInCancel') move({ phase: 'needs-signup', signIn: null });
      return Promise.resolve(c.m === 'signInLogin' ? { needs: 0 } : null);
    };
  }
  const startAt = (fake: FakeClient) => {
    fake.set('account', account({ phase: 'needs-signup', user: null, instance: { ...account().instance!, passwordSignup: true } }));
    signInWorker(fake);
    replaceHistoryState(null, '', '/');
    return render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
  };
  it('enters the ceremony from onboarding and lands at / after done', async () => {
    const user = userEvent.setup();
    const fake = new FakeClient();
    startAt(fake);
    await user.click(screen.getByRole('button', { name: 'Use an existing account' }));
    // web3.8 (R6, R7): the login first, then the recovery key, which adds this browser.
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(fake.callsOf('signInLogin')).toEqual([{ m: 'signInLogin', username: 'ada', password: 'correct horse' }]);
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Your recovery key');
    await user.click(screen.getByRole('textbox', { name: 'Recovery key' }));
    await user.paste('7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA');
    await user.click(screen.getByRole('button', { name: 'Add this browser' }));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('You’re in');
    replaceHistoryState(null, '', '/welcome');
    const depth = window.history.length;
    await user.click(screen.getByRole('button', { name: 'Open dilla' }));
    expect(screen.queryByRole('heading', { name: 'You’re in' })).toBeNull();
    expect(window.location.pathname).toBe('/');
    expect(window.history.length).toBe(depth);
    expect(screen.getByRole('navigation', { name: 'servers' })).toBeInTheDocument();
  });
  it('returns to onboarding on cancel, and a later signup is not followed by the sign-in done step', async () => {
    const user = userEvent.setup();
    const fake = new FakeClient();
    startAt(fake);
    await user.click(screen.getByRole('button', { name: 'Use an existing account' }));
    await user.click(screen.getByRole('button', { name: 'Have an invite? Create an account' }));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Join dilla.test');
    act(() => fake.set('account', account({ phase: 'ready' })));
    expect(screen.getByText('New account · step 6 of 6')).toBeInTheDocument();
    expect(screen.queryByText(/This browser is now a device of/)).toBeNull();
    expect(screen.queryByText('Sign in · step 4 of 4')).toBeNull();
  });
  // web3.8 (C1): the held assertion lives only in the worker, so a reload at the key step before the registration boots
  // to needs-signup. The page starts over at onboarding step 1, and the entry leads to the login, not to the key.
  it('a reload at the key step before the registration starts over at onboarding, then the login', async () => {
    const user = userEvent.setup();
    const fake = new FakeClient();
    startAt(fake);
    await user.click(screen.getByRole('button', { name: 'Use an existing account' }));
    await user.type(screen.getByRole('textbox', { name: 'Username' }), 'ada');
    await user.type(screen.getByLabelText('Password'), 'correct horse');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Your recovery key');
    // The reload: the boot runs again and finds no enrolment.
    act(() => fake.set('account', account({ phase: 'loading', user: null, instance: null })));
    act(() => fake.set('account', account({ phase: 'needs-signup', user: null, instance: { ...account().instance!, passwordSignup: true } })));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Join dilla.test');
    expect(screen.queryByRole('textbox', { name: 'Recovery key' })).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Use an existing account' }));
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Sign in to dilla.test');
    expect(screen.getByText('Sign in · step 1 of 4')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Username' })).toHaveValue('');
  });
});

describe('App cleared', () => {
  const RACE = { code: 'E_LIST_RACE', detail: '', status: 0, retryAfterMs: null };
  it('reloads to the race message after a list race, once', () => {
    const fake = new FakeClient();
    const reload = vi.fn();
    fake.set('account', account({ phase: 'cleared', user: null, deviceId: null, signIn: null, error: RACE }));
    const view = render(<CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider>);
    expect(screen.getByRole('status')).toHaveTextContent('Starting dilla');
    act(() => fake.set('account', { ...fake.get('account')! }));
    view.rerender(<CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider>);
    expect(reload.mock.calls).toEqual([['/welcome?signin=race']]);
  });
  // main.tsx renders under StrictMode, whose double effect on mount is what the once-per-mount guard is for.
  it('reloads once under StrictMode', () => {
    const fake = new FakeClient();
    const reload = vi.fn();
    fake.set('account', account({ phase: 'cleared', user: null, deviceId: null, signIn: null, error: RACE }));
    render(<StrictMode><CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider></StrictMode>);
    expect(reload.mock.calls).toEqual([['/welcome?signin=race']]);
  });
  // REGISTRATION-DEVICES-02: the row was replaced after the enrolment was written; the wipe reloads with the notice.
  it('reloads to the eviction message after an evicted sign-in, never through the revoked splash', () => {
    const fake = new FakeClient();
    const reload = vi.fn();
    fake.set('account', account({ phase: 'enrolling', user: null, deviceId: null, signIn: { username: 'ada', factor: null } }));
    render(<CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider>);
    act(() => fake.set('account', account({
      phase: 'cleared', user: null, deviceId: null, signIn: null, error: { code: 'E_SIGNIN_EVICTED', detail: '', status: 0, retryAfterMs: null },
    })));
    expect(screen.queryByText('This browser was signed out')).toBeNull();
    expect(reload.mock.calls).toEqual([['/welcome?signin=evicted']]);
  });
  it('reloads to / after a sign-out or a forget', () => {
    const fake = new FakeClient();
    const reload = vi.fn();
    fake.set('account', account({ phase: 'ready' }));
    render(<CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider>);
    expect(reload).not.toHaveBeenCalled();
    act(() => fake.set('account', account({ phase: 'cleared', user: null, deviceId: null, signIn: null, error: null })));
    expect(screen.queryByRole('navigation', { name: 'servers' })).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('Starting dilla');
    expect(reload.mock.calls).toEqual([['/']]);
  });
  it('a list race during the recovery key replaces the ceremony with the splash and reloads', async () => {
    const user = userEvent.setup();
    const fake = new FakeClient();
    const reload = vi.fn();
    fake.set('account', account({ phase: 'signin-key', user: null, deviceId: null, signIn: { username: 'ada', factor: null } }));
    fake.handler = c => {
      if (c.m !== 'signInKey') return Promise.resolve(null);
      fake.set('account', account({ phase: 'cleared', user: null, deviceId: null, signIn: null, error: RACE }));
      return Promise.reject(refusal({ code: 'E_LIST_RACE' }));
    };
    replaceHistoryState(null, '', '/');
    render(<CoreProvider client={fake}><App fatal={null} reload={reload} /></CoreProvider>);
    await user.click(screen.getByRole('textbox', { name: 'Recovery key' }));
    await user.paste('7K2M-QX9D-H4TB-R8NW-C3VF-J6PZ-A1GE-Y5KS-M0QT-B7XH-W2DN-F9RC-P4ZA');
    await user.click(screen.getByRole('button', { name: 'Add this browser' }));
    expect(screen.queryByRole('heading', { name: 'Your recovery key' })).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('Starting dilla');
    expect(reload.mock.calls).toEqual([['/welcome?signin=race']]);
  });
});

describe('App settings and notifications', () => {
  const C = 'c4'.repeat(16);
  const DM = '9a'.repeat(16);
  afterEach(() => { vi.unstubAllGlobals(); });
  function ready(fake: FakeClient) {
    fake.set('account', account({ phase: 'ready' }));
    fake.set('communities', [{ id: C, name: 'Midgard', owner: 'ee'.repeat(16) }]);
    fake.set(`channels:${C}`, []);
    fake.set('dms', [{ id: DM, kind: 3, members: [], name: 'bob', group: 'active' }] satisfies DmSummary[]);
    fake.set('settings', {});
  }
  it('opens settings over the shell and returns focus to the top bar’s gear on Escape (Signal: the user panel has no gear)', async () => {
    const user = userEvent.setup();
    const fake = new FakeClient();
    ready(fake);
    replaceHistoryState(null, '', `/c/${C}`);
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    const rail = screen.getByRole('navigation', { name: 'servers' });
    expect(within(rail).queryByRole('button', { name: 'settings' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'user settings' })).toBeNull();
    const gear = () => within(document.querySelector<HTMLElement>('.d-topbar')!).getByRole('button', { name: 'Open settings' });
    await user.click(gear());
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'servers' })).toBe(rail);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'Settings' })).toBeNull();
    expect(window.location.pathname).toBe(`/c/${C}`);
    expect(gear()).toHaveFocus();
  });
  it('shows a desktop notification for a new notice and opens its DM on click', () => {
    class Note {
      static permission: NotificationPermission = 'granted';
      static made: Note[] = [];
      onclick: ((ev: Event) => unknown) | null = null;
      closed = false;
      constructor(readonly title: string, readonly options: NotificationOptions) { Note.made.push(this); }
      close(): void { this.closed = true; }
    }
    vi.stubGlobal('Notification', Note);
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});
    const fake = new FakeClient();
    ready(fake);
    fake.set('notices', { nextId: 0, items: [] });
    replaceHistoryState(null, '', `/c/${C}`);
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    const n: Notice = { id: 0, channelId: DM, communityId: null, kind: 'dm', senderUser: '28'.repeat(16), senderName: 'bob', body: 'yo', ts: 1_790_000_000, threadRoot: null };
    act(() => fake.set('notices', { nextId: 1, items: [n] }));
    expect(Note.made.map(x => [x.title, x.options.body, x.options.tag])).toEqual([['bob', 'yo', `dilla:${DM}`]]);
    act(() => { Note.made[0].onclick?.(new Event('click')); });
    expect(focus).toHaveBeenCalledTimes(1);
    expect(window.location.pathname).toBe(`/dm/${DM}`);
    expect(Note.made[0].closed).toBe(true);
    focus.mockRestore();
  });
  // FX-core (final review I1): a reply that shows in a thread opens that thread, not only its channel.
  it('a notification click on a thread reply opens its channel and the thread', async () => {
    class Note {
      static permission: NotificationPermission = 'granted';
      static made: Note[] = [];
      onclick: ((ev: Event) => unknown) | null = null;
      constructor(readonly title: string, readonly options: NotificationOptions) { Note.made.push(this); }
      close(): void {}
    }
    vi.stubGlobal('Notification', Note);
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});
    const GEN = 'c3'.repeat(16);
    const ROOT = '71'.repeat(16);
    const fake = new FakeClient();
    ready(fake);
    fake.set(`channels:${C}`, [{ id: GEN, communityId: C, kind: 0, mode: 0, name: 'general', topic: '', parentId: null, position: 0, slowmode: 0, announcement: false, group: 'active' }]);
    fake.set(`timeline:${GEN}`, { channelId: GEN, group: 'active', hasEarlier: false, items: [] });
    fake.set(`timeline:${DM}`, { channelId: DM, group: 'active', hasEarlier: false, items: [] });
    fake.set('notices', { nextId: 0, items: [] });
    // In the DM, so the channel is not on screen and the notice notifies.
    replaceHistoryState(null, '', `/dm/${DM}`);
    render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
    const n: Notice = { id: 0, channelId: GEN, communityId: C, kind: 'mention', senderUser: '28'.repeat(16), senderName: 'bob', body: 'look', ts: 1_790_000_000, threadRoot: ROOT };
    act(() => fake.set('notices', { nextId: 1, items: [n] }));
    expect(Note.made).toHaveLength(1);
    act(() => { Note.made[0].onclick?.(new Event('click')); });
    expect(window.location.pathname).toBe(`/c/${C}/${GEN}`);
    await vi.waitFor(() => expect(fake.callsOf('openThread')).toEqual([{ m: 'openThread', channelId: GEN, rootMsgId: ROOT }]));
    focus.mockRestore();
  });
});

it('checks for builds before login without interrupting the form', async () => {
  const fake = new FakeClient();
  fake.set('account', account({ phase: 'signin-login', signIn: { username: null, factor: null } }));
  const reload = vi.fn();
  let changed!: (entry: string) => void;
  const watch = vi.fn((options: import('./update.ts').UpdateWatcher) => { changed = entry => options.changed(entry); return () => {}; });
  render(<CoreProvider client={fake}><App fatal={null} update={{ currentEntry: '/assets/old.js', watch,
    store: { load: () => Promise.resolve(null), save: async () => {}, remove: async () => {} }, reload }} /></CoreProvider>);
  await waitFor(() => expect(watch).toHaveBeenCalled());
  act(() => changed('/assets/new.js'));
  expect(await screen.findByText('An update is ready. Finish or copy your current work before reloading.')).toBeInTheDocument();
  expect(reload).not.toHaveBeenCalled();
});

it('sends coalesced input activity, with no activity on mount or foreground alone', async () => {
  const fake = new FakeClient(); fake.set('account', account());
  let now = Date.now(); const clock = vi.spyOn(Date, 'now').mockImplementation(() => now);
  const view = render(<CoreProvider client={fake}><App fatal={null} /></CoreProvider>);
  await waitFor(() => expect(fake.callsOf('start')).toHaveLength(1));
  document.dispatchEvent(new Event('visibilitychange'));
  expect(fake.callsOf('presenceActivity')).toHaveLength(0);
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 'a' }));
  window.dispatchEvent(new Event('pointermove'));
  expect(fake.callsOf('presenceActivity')).toHaveLength(1);
  now += 5000;
  window.dispatchEvent(new Event('pointerdown'));
  expect(fake.callsOf('presenceActivity')).toHaveLength(2);
  view.unmount(); clock.mockRestore();
});
