import { afterEach, describe, expect, it, vi } from 'vitest';
import { dmMenuItems, startDmCall } from '../../test/row-menu.ts';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { t } from '../../strings/index.ts';
import {
  A, BOB, BOB_DEV, CAROL, CAROL_DEV, MEMBERS, OWN_DEV, bobChanged, callState, DM, GENV, MUSIC, renderVoiceShell, TEXT, type VoiceHarnessOptions,
} from '../../test/fake-client.ts';
import { ME } from '../../test/fixtures.ts';
import { REMOVED_HOLD_MS } from './useCall.tsx';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { PHONE_QUERY } from '../../layout.ts';
import { isApplePlatform, voiceShortcutText } from './shortcuts.ts';

afterEach(() => { vi.unstubAllGlobals(); });

const main = () => within(screen.getByRole('main'));
// The dock holds its controls only while the stage is not on screen (web3.7): these step off the stage and back on it.
const go = (path: string) => { window.history.pushState(null, '', path); window.dispatchEvent(new PopStateEvent('popstate')); };
async function offStage() {
  go(`/c/${A}/${TEXT}`);
  await screen.findByRole('toolbar', { name: 'voice controls' });
}
async function onStage() {
  go(`/c/${A}/${GENV}`);
  await main().findByRole('toolbar', { name: 'voice channel controls' });
}
const UMK = { code: 'E_UMK_CHANGED', detail: '', status: 0, retryAfterMs: null };

/** Joins voice 'general' from its voice view; the worker then reports the call in. */
async function inVoice(o: VoiceHarnessOptions = {}) {
  const app = renderVoiceShell(o);
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
  return app;
}

describe('voice in the shell', () => {
  it('keeps playback controls out of the voice view', async () => {
    const app = await inVoice();
    expect(screen.queryByRole('region', { name: 'Music' })).toBeNull();
    expect(app.client.callsOf('humanInteractionRequest')).toHaveLength(0);
  });
  it('shows a voice channel the shell does not know as AFK as an ordinary voice view with a join button', async () => {
    renderVoiceShell({ path: `/c/${A}/${MUSIC}`, voice: { [MUSIC]: { callId: MUSIC, participants: [
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 0, own: false, web: false, bot: false }] } } });
    const app = screen.getByRole('main');
    expect(await within(app).findByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'AFK channel' })).toBeNull();
  });

  it('final review I-2 control: in an ordinary call the stage\u2019s camera is live (Signal, Ruling U: the dock has leave alone)', async () => {
    await inVoice();
    const dock = within(screen.getByRole('toolbar', { name: t('voice.dock.region') }));
    expect(dock.queryByRole('button', { name: 'camera' })).toBeNull();
    expect(main().getByRole('button', { name: 'Camera' })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('an ordinary voice call keeps its controls live', async () => {
    await inVoice();
    for (const name of ['Mute', 'Camera']) expect(main().getByRole('button', { name })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('reports actual input while a voice call remains open on another page', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false });
    expect(app.client.callsOf('afkActivity')).toHaveLength(1);
    expect(app.client.callsOf('afkActivity')[0]).toEqual({ m: 'afkActivity', communityId: A, channelId: GENV }); // desktop: not exempt
    app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false });
    expect(app.client.callsOf('afkActivity')).toHaveLength(1);
    const now = Date.now();
    const dateNow = vi.spyOn(Date, 'now').mockReturnValue(now + 6_000);
    go(`/dm/${DM}`);
    fireEvent.pointerMove(window);
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(2));
    expect(app.client.callsOf('afkActivity')[1]).toEqual({ m: 'afkActivity', communityId: A, channelId: GENV });
    dateNow.mockRestore();
  });

  it('reports a short microphone utterance before the next polling interval', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false });
    expect(app.client.callsOf('afkActivity')).toHaveLength(1);
    const now = Date.now();
    const dateNow = vi.spyOn(Date, 'now').mockReturnValue(now + 6_000);
    act(() => app.session.update({ speaking: true }));
    await waitFor(() => expect(app.client.callsOf('afkActivity')).toHaveLength(2));
    act(() => app.session.update({ speaking: false }));
    dateNow.mockRestore();
  });

  it('retries a restored call after a transient start failure', async () => {
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    const app = renderVoiceShell({ update: { currentEntry: '/assets/new.js', watch: () => () => undefined,
      store: { save: () => Promise.resolve(), load: async () => { await gate; return { state: { call: { channelId: GENV, camera: true, sharing: false } }, files: [] }; },
        remove: () => Promise.resolve() }, reload: vi.fn() } });
    let attempts = 0;
    app.client.handler = command => {
      if (command.m === 'callJoin' && ++attempts === 1) return Promise.reject(Object.assign(new Error('temporary'), { code: 'E_DISCONNECTED' }));
      if (command.m === 'callJoin') app.client.set('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
      return Promise.resolve(null);
    };
    release();
    await waitFor(() => expect(attempts).toBe(2), { timeout: 4_000 });
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    expect(app.session.setCamera).toHaveBeenCalledWith(true);
  });
  it('rejoins after a lost media connection and keeps the call available', async () => {
    const app = await inVoice();
    act(() => {
      app.session.update({ camera: true, sharing: true });
      app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' });
      app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
        error: { code: 'E_DISCONNECTED', detail: '', status: 0, retryAfterMs: null } }));
    });
    await waitFor(() => expect(app.sessions).toHaveLength(2), { timeout: 4_000 });
    expect(app.client.callsOf('callJoin')).toHaveLength(2);
    expect(app.session.setCamera).toHaveBeenCalledWith(true);
    expect(screen.getByText(t('update.sharingStopped'))).toBeInTheDocument();
  });
  it('late rejoin: a device gone longer than the AFK timeout is not rejoined; the stage says so and its join button joins', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false, seq: 1, away: [] });
    // The lid was closed for 301 s: the wall clock moved on, the page's monotonic clock with it.
    const wall = Date.now.bind(Date);
    const perf = performance.now.bind(performance);
    const nowSpy = vi.spyOn(Date, 'now').mockImplementation(() => wall() + 301_000);
    const perfSpy = vi.spyOn(performance, 'now').mockImplementation(() => perf() + 301_000);
    try {
      act(() => app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' }));
      expect(await main().findByText(t('voice.leftWhileAway'), {}, { timeout: 4_000 })).toBeInTheDocument();
      expect(app.sessions).toHaveLength(1);
      expect(screen.queryByText(t('update.rejoiningVoice'))).toBeNull();
      await app.user.click(main().getByRole('button', { name: 'Join voice' }));
      await waitFor(() => expect(app.sessions).toHaveLength(2));
      expect(app.client.callsOf('callJoin')).toHaveLength(2);
    } finally {
      nowSpy.mockRestore();
      perfSpy.mockRestore();
    }
  });
  it('lets the person cancel a pending automatic voice rejoin', async () => {
    const app = await inVoice();
    act(() => app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' }));
    await app.user.click(screen.getByRole('button', { name: 'leave' }));
    await new Promise(resolve => setTimeout(resolve, 1_200));
    expect(app.sessions).toHaveLength(1);
  });
  it('stops automatic rejoin when the call refuses the device permanently', async () => {
    const app = await inVoice();
    app.client.handler = command => command.m === 'callJoin'
      ? Promise.reject(Object.assign(new Error('E_FORBIDDEN'), { code: 'E_FORBIDDEN' }))
      : Promise.resolve(null);
    act(() => app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' }));
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(2), { timeout: 4_000 });
    await waitFor(() => expect(screen.queryByText(t('update.rejoiningVoice'))).toBeNull());
    await new Promise(resolve => setTimeout(resolve, 2_100));
    expect(app.client.callsOf('callJoin')).toHaveLength(2);
  });
  it('keeps a just-ended room for the update refresh to rejoin', async () => {
    let changed!: (entry: string) => void;
    const save = vi.fn<(...args: [unknown, unknown, readonly File[]]) => Promise<void>>().mockResolvedValue();
    const reload = vi.fn();
    const app = await inVoice({ update: { currentEntry: '/assets/old.js',
      watch: options => { changed = entry => options.changed(entry); return () => undefined; },
      store: { save, load: () => Promise.resolve(null), remove: () => Promise.resolve() }, reload } });
    act(() => app.session.update({ phase: 'ended', error: 'E_CALL_ENDED' }));
    act(() => changed('/assets/new.js'));
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(save.mock.calls[0]?.[1]).toMatchObject({ call: { channelId: GENV } });
  });
  it('reopens a community voice room closed while an update is in progress', async () => {
    let changed!: (entry: string) => void;
    const app = await inVoice({ update: { currentEntry: '/assets/old.js',
      watch: options => { changed = entry => options.changed(entry); return () => undefined; },
      store: { save: () => new Promise<void>(() => undefined), load: () => Promise.resolve(null), remove: () => Promise.resolve() },
      reload: vi.fn() } });
    act(() => changed('/assets/new.js'));
    act(() => {
      app.session.update({ phase: 'ended', error: 'E_CALL_ENDED' });
      app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
        error: { code: 'E_CALL_ENDED', detail: '', status: 0, retryAfterMs: null } }));
    });
    await waitFor(() => expect(app.sessions).toHaveLength(2), { timeout: 4_000 });
    expect(app.client.callsOf('callJoin')).toHaveLength(2);
  });
  it('Control+Shift+M toggles mute only while in voice; a bare "m" does nothing', async () => {
    const app = await inVoice();
    await app.user.keyboard('m');
    expect(app.session.muted).toBe(false);
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    expect(app.session.muted).toBe(true);
    await app.user.keyboard('{Meta>}{Shift>}M{/Shift}{/Meta}');
    expect(app.session.muted).toBe(false);
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    expect(app.session.deafened).toBe(true);
    expect(screen.getByText(t('voice.announce.deafened'))).toBeInTheDocument();
  });

  it('binds no shortcut outside voice', async () => {
    const app = renderVoiceShell();
    await main().findByRole('button', { name: 'Join voice' });
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    expect(app.sessions).toHaveLength(0);
    expect(app.client.calls.filter(c => c.m === 'voiceSelf')).toHaveLength(0);
  });

  it('a blocked call shows voice.blockedChanged and does not pause the DM composer', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}`, call: callState({ phase: 'blocked', channelId: DM, error: UMK, changedUser: BOB }) });
    expect(await screen.findByText(t('voice.blockedChanged', { name: 'bob' }))).toBeInTheDocument();
    const composer = screen.getByRole('textbox', { name: 'message @bob' });
    expect(composer).not.toHaveAttribute('aria-disabled');
    expect(composer).not.toHaveAttribute('readonly');
    // Ruling I2: with changedUser null the notice is voice.blockedJoin.
    app.client.publish('call', callState({ phase: 'blocked', channelId: DM, error: UMK, changedUser: null }));
    expect(await screen.findByText(t('voice.blockedJoin'))).toBeInTheDocument();
    expect(screen.queryByText(t('voice.blockedChanged', { name: 'bob' }))).toBeNull();
    expect(screen.getByRole('textbox', { name: 'message @bob' })).not.toHaveAttribute('aria-disabled');
  });

  it('a paused call shows the voice.paused banner, focuses its action once, and announces voice.resumed when it ends', async () => {
    const app = await inVoice({ trust: [bobChanged()], withSettings: true });
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    const text = t('voice.paused', { name: 'bob' });
    expect(await screen.findByText(text)).toBeInTheDocument();
    const action = main().getByRole('button', { name: 'compare safety numbers' });
    await waitFor(() => expect(action).toHaveFocus());
    // A re-render does not move focus again.
    main().getByRole('button', { name: 'Leave' }).focus();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    expect(main().getByRole('button', { name: 'Leave' })).toHaveFocus();
    expect(screen.getByText('paused in general')).toBeInTheDocument();
    expect(app.session.setPaused).toHaveBeenCalledWith(true);
    expect(app.session.setPaused).toHaveBeenCalledTimes(1);
    // The action opens Privacy's comparison for bob.
    await app.user.click(main().getByRole('button', { name: 'compare safety numbers' }));
    await waitFor(() => expect(window.location.pathname).toBe('/settings/privacy'));
    await waitFor(() => expect(app.client.callsOf('safetyNumber')).toEqual([{ m: 'safetyNumber', userId: BOB, umk: 'cd'.repeat(32) }]));
    // The pause ends.
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
    await waitFor(() => expect(screen.queryByText(text)).toBeNull());
    expect(screen.getByText(t('voice.resumed'))).toBeInTheDocument();
    expect(screen.getByText(t('voice.resumed')).closest('[role="status"]')).not.toBeNull();
    expect(app.session.setPaused).toHaveBeenLastCalledWith(false);
    expect(app.sounds.played).toEqual([]);
  });

  it('a block or a removal leaves the page call and never resumes it', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' } }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('blocked'));
    expect(app.session.setPaused).not.toHaveBeenCalledWith(false);
    expect(await screen.findByText(t('voice.blocked'))).toBeInTheDocument();
    expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull();
  });

  it('an unsupported browser shows the dialog instead of joining, and focus returns to join voice', async () => {
    const app = renderVoiceShell({ supported: false });
    const join = await main().findByRole('button', { name: 'Join voice' });
    await app.user.click(join);
    expect(await screen.findByRole('dialog', { name: 'Voice does not work in this browser' })).toBeInTheDocument();
    expect(app.client.callsOf('callJoin')).toEqual([]);
    expect(app.module.startPageCall).not.toHaveBeenCalled();
    await app.user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(main().getByRole('button', { name: 'Join voice' })).toHaveFocus();
  });

  it('switching voice channels leaves the first call before joining the second', async () => {
    const app = await inVoice();
    let release: () => void = () => undefined;
    app.session.leaveGate = new Promise<void>(resolve => { release = resolve; });
    await app.user.click(screen.getByRole('button', { name: 'voice music' }));
    await waitFor(() => expect(window.location.pathname).toContain(MUSIC));
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.log).toContain('leave:switch'));
    expect(app.log).not.toContain(`callJoin:${MUSIC}`);
    release();
    await waitFor(() => expect(app.log).toContain(`callJoin:${MUSIC}`));
    expect(app.log.indexOf('left:switch')).toBeLessThan(app.log.indexOf(`callJoin:${MUSIC}`));
    expect(app.client.callsOf('callJoin').map(c => c.channelId)).toEqual([GENV, MUSIC]);
  });

  it('joins with the stored options and moves focus to the first stage control, then leaves back to the row', async () => {
    const app = await inVoice({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'F13', 'voice.noise': 'off' } });
    const opts = vi.mocked(app.module.startPageCall).mock.calls[0][2];
    expect(opts).toMatchObject({ mode: 'ptt', noise: 'off', input: '', output: '' });
    expect(app.module.bindPushToTalk).toHaveBeenCalledWith(expect.objectContaining({ code: 'F13' }));
    await waitFor(() => expect(main().getByRole('toolbar', { name: 'voice channel controls' }).querySelector('button')).toHaveFocus());
    expect(screen.getByRole('toolbar', { name: 'voice controls' })).toBeInTheDocument();
    expect(document.querySelector('.dw-voice-dock .d-voice-dock__text')).toHaveTextContent(/^general$/);
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('user'));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull());
    expect(screen.getByRole('button', { name: 'voice general' })).toHaveFocus();
  });

  it('a stored push-to-talk key that is not one key on its own binds nothing', async () => {
    const app = await inVoice({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'KeyV' } });
    expect(app.module.bindPushToTalk).not.toHaveBeenCalled();
  });

  it('maps the microphone outcome to its notice without hiding video', async () => {
    const app = await inVoice();
    app.session.update({ mic: 'failed', error: 'E_MIC_PUBLISH' });
    expect(await screen.findByText(t('voice.micFailed'))).toBeInTheDocument();
    expect(document.querySelector('.d-call-stage')).not.toHaveAttribute('data-media-hidden');
    app.session.update({ mic: 'denied', error: null });
    expect(await screen.findByText(t('voice.micDenied'))).toBeInTheDocument();
  });

  it('lists who is in voice under their voice row in the one list, with no voice headings', async () => {
    renderVoiceShell({ voice: { [GENV]: { callId: GENV, participants: [
      { userId: BOB, deviceId: '29'.repeat(16), name: 'bob', flags: 1 | 2, own: false, web: true, bot: false },
    ] } } });
    expect(await screen.findByRole('button', { name: 'voice general' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'active voice' })).toBeNull();
    expect(screen.queryByRole('heading', { name: 'voice' })).toBeNull();
    expect(screen.getAllByText('bob').length).toBeGreaterThan(0);
    expect(screen.getAllByRole('img', { name: 'muted' }).length).toBeGreaterThan(0);
  });
});

// Security-scanner lead (fail-open state drift): the session follows the call slice on every change, whenever it arrives.
describe('the session follows the call slice', () => {
  const start = async (o: VoiceHarnessOptions = {}) => {
    const app = renderVoiceShell(o);
    const release = app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.log).toContain(`callJoin:${GENV}`));
    return { app, release };
  };
  it('a paused slice that arrived while the start was pending pauses the session once it attaches', async () => {
    const { app, release } = await start();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.setPaused).toHaveBeenCalledWith(true));
  });
  it('a blocked slice that arrived while the start was pending leaves the session once it attaches', async () => {
    const { app, release } = await start();
    app.client.publish('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('blocked'));
    expect(app.session.setPaused).not.toHaveBeenCalledWith(false);
  });
  // WB client I1: the start is cancelled at once (the worker told with callLeave); no session is made for nobody.
  it('an unmount while the start is pending cancels the start and tells the worker', async () => {
    const { app, release } = await start();
    app.view.unmount();
    release();
    await waitFor(() => expect(app.log).toContain(`aborted:${GENV}`));
    // The abort's own callLeave, and the follow-up leave a next join would wait for.
    expect(app.client.callsOf('callLeave').length).toBeGreaterThan(0);
    expect(app.client.callsOf('callLeave').every(c => c.reason === 'user')).toBe(true);
    expect(app.sessions).toHaveLength(0);
  });
  it('an unmount in voice leaves the session and never unpauses it', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.setPaused).toHaveBeenCalledWith(true));
    app.view.unmount();
    expect(app.session.leave).toHaveBeenCalledWith('user');
    expect(app.session.setPaused).not.toHaveBeenCalledWith(false);
  });
  it('a slice that drops to idle, or names another channel, after the call was live leaves the session', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'idle' }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('removed'));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull());
    app.view.unmount();
    const second = await inVoice();
    second.client.publish('call', callState({ phase: 'in', channelId: MUSIC, callId: MUSIC }));
    await waitFor(() => expect(second.session.leave).toHaveBeenCalledWith('removed'));
  });
  it('a pause or block for another channel is not applied to this session', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'paused', channelId: MUSIC, callId: MUSIC, error: UMK, changedUser: BOB }));
    expect(app.session.setPaused).not.toHaveBeenCalled();
    expect(screen.queryByText(t('voice.paused', { name: 'bob' }))).toBeNull();
  });
  it('a start that resolves after the slice already left the call ends the session at once', async () => {
    const { app, release } = await start();
    app.client.publish('call', callState({ phase: 'idle' }));
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('removed'));
    expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull();
  });
  it('the latest slice wins when several arrive between two renders', async () => {
    const app = await inVoice();
    act(() => {
      app.client.set('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
      app.client.set('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    });
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('blocked'));
    expect(app.session.setPaused).not.toHaveBeenCalledWith(false);
  });
  it('a setPaused that fails tears the session down rather than leave media running', async () => {
    const app = await inVoice();
    app.session.setPaused.mockImplementation(() => Promise.reject(new Error('E_INTERNAL')));
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('removed'));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull());
  });
  it('a setPaused that throws, or a leave that throws, never escapes and never keeps the session attached', async () => {
    const app = await inVoice();
    app.session.setPaused.mockImplementation(() => { throw new Error('E_INTERNAL'); });
    app.session.leave.mockImplementation(() => { throw new Error('E_INTERNAL'); });
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('removed'));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull());
  });
});

// Task-9 review, fix round 1.
describe('review round 1', () => {
  const lasting = () => document.querySelector('.dw-voice-lasting');
  const toLobby = async (app: ReturnType<typeof renderVoiceShell>) => {
    window.history.pushState(null, '', `/c/${A}/${TEXT}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull());
    void app;
  };

  it('I1: a block after a pause that already rendered leaves and never unpauses', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.setPaused).toHaveBeenCalledWith(true));
    app.client.publish('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('blocked'));
    expect(app.session.setPaused).not.toHaveBeenCalledWith(false);
    expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull();
    expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull();
  });

  it('I1: a removal leaves with "removed", shows voice.removed in the voice view and ends the dock', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV, callId: GENV }));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('removed'));
    expect(app.session.setPaused).not.toHaveBeenCalled();
    // AW5 fix round 3: the notice waits (at most REMOVED_HOLD_MS) for the read that may give the cut a pending reason.
    expect(await main().findByText(t('voice.removed'), undefined, { timeout: REMOVED_HOLD_MS + 1_500 })).toBeInTheDocument();
    expect(screen.queryByRole('toolbar', { name: 'voice controls' })).toBeNull();
  });

  it('I2: switching the input mode in a call follows in the session; push to talk binds only once the session is in it', async () => {
    const app = await inVoice({ settings: { 'voice.pttKey': 'F13' } });
    expect(app.module.bindPushToTalk).not.toHaveBeenCalled();
    app.client.publish('settings', { 'voice.pttKey': 'F13', 'voice.mode': 'ptt' });
    expect(app.session.setMode).toHaveBeenCalledWith('ptt');
    await waitFor(() => expect(app.module.bindPushToTalk).toHaveBeenCalledWith(expect.objectContaining({ code: 'F13' })));
    expect(app.log.indexOf('mode:ptt')).toBeGreaterThanOrEqual(0);
    expect(document.querySelector('.d-voice-dock')).toHaveTextContent(t('settings.voice.ptt'));
    app.client.publish('settings', { 'voice.pttKey': 'F13' });
    expect(app.session.setMode).toHaveBeenLastCalledWith('vad');
    expect(document.querySelector('.d-voice-dock')).toHaveTextContent(t('settings.voice.vad'));
  });

  it('I2: a push-to-talk key is never bound while the session is not in push to talk', async () => {
    const app = await inVoice({ settings: { 'voice.pttKey': 'F13' } });
    // The session does not take the mode (an ended session ignores it): the key must not bind, or it would suggest a
    // closed microphone that is open.
    app.session.setMode.mockImplementation(() => undefined);
    app.client.publish('settings', { 'voice.pttKey': 'F13', 'voice.mode': 'ptt' });
    expect(app.session.setMode).toHaveBeenCalledWith('ptt');
    expect(app.module.bindPushToTalk).not.toHaveBeenCalled();
  });

  it('f10app m2: a rejoin that waits for its earlier Remove is announced in the dock status', async () => {
    const app = renderVoiceShell();
    const release = app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    const status = () => document.querySelector('.d-voice-dock [role="status"]');
    await waitFor(() => expect(status()).not.toBeNull());
    app.client.publish('call', callState({ phase: 'joining', channelId: GENV }));
    expect(status()).not.toHaveTextContent(t('voice.announce.rejoining', { where: 'general' }));
    app.client.publish('call', callState({ phase: 'joining', channelId: GENV, rejoining: true }));
    await waitFor(() => expect(status()).toHaveTextContent('Rejoining general. This can take up to 30 seconds.'));
    release();
  });

  it('I3: the outgoing ring is announced in the dock status, once per change', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await startDmCall(app.user, 'bob');
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    app.client.publish('call', callState({ phase: 'in', channelId: DM, callId: DM, outgoing: { ringing: ['bob'], declined: [], notRung: [] } }));
    const status = () => document.querySelector('.d-voice-dock [role="status"]');
    await waitFor(() => expect(status()).toHaveTextContent('ringing bob'));
    app.client.publish('call', callState({ phase: 'in', channelId: DM, callId: DM, outgoing: { ringing: [], declined: ['bob'], notRung: [] } }));
    await waitFor(() => expect(status()).toHaveTextContent('bob declined.'));
  });

  it('I4 and m4: off the stage, the dock pause banner is a status with a described, full-size action that takes focus once', async () => {
    const app = await inVoice({ trust: [bobChanged()] });
    await toLobby(app);
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    const banner = await waitFor(() => { const b = document.querySelector('.dw-voice-dock__paused'); expect(b).not.toBeNull(); return b!; });
    expect(banner).toHaveAttribute('role', 'status');
    const action = within(banner as HTMLElement).getByRole('button', { name: 'compare safety numbers' });
    expect(action).toHaveAccessibleDescription(t('voice.paused', { name: 'bob' }));
    expect(action).not.toHaveAttribute('data-size', 'sm');
    await waitFor(() => expect(action).toHaveFocus());
    // Opening the voice view later does not take focus again for the same pause.
    await app.user.click(screen.getByRole('button', { name: 'voice general' }));
    await main().findByRole('button', { name: 'compare safety numbers' });
    expect(screen.getByRole('button', { name: 'voice general' })).toHaveFocus();
  });

  it('I6: a block off the stage is announced where it outlives the dock, and focus in the dock goes to the voice row', async () => {
    const app = await inVoice();
    await toLobby(app);
    screen.getByRole('toolbar', { name: 'voice controls' }).querySelector('button')!.focus();
    app.client.publish('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' } }));
    await waitFor(() => expect(lasting()).toHaveTextContent(t('voice.blocked')));
    expect(screen.getByRole('button', { name: 'voice general' })).toHaveFocus();
  });

  it('I6: a removal off the stage is announced too', async () => {
    const app = await inVoice();
    await toLobby(app);
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV, callId: GENV }));
    await waitFor(() => expect(lasting()).toHaveTextContent(t('voice.removed')), { timeout: REMOVED_HOLD_MS + 1_500 });
  });

  it('"You left {where}." is announced in a status region that outlives the dock', async () => {
    const app = await inVoice();
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    await waitFor(() => expect(lasting()).toHaveTextContent('You left general.'));
    expect(lasting()).toHaveAttribute('role', 'status');
  });

  it('m9: no join button while the sendBlocks slice is not known yet (a block counts as held)', () => {
    renderVoiceShell({ call: callState({ phase: 'blocked', channelId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' } }) });
    expect(main().getByText(t('voice.blockedJoin'))).toBeInTheDocument();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
  });

  // Integration ruling (2026-10-13): a block that came only from the call group (the channel's own sendBlocks entry is
  // clear, no changedUser) offers the join again. The call group was left and forgotten, so nothing can show the block
  // cleared; a new join is safe because call_join_check refuses a staged join before anything is posted, and RV-2 closes a
  // poisoned idle group. Attacker: a member (or the instance) who poisons one call group must not lock this person out of
  // the channel's voice for good.
  it('a call-group-only block keeps its notice and offers the join again', async () => {
    const app = renderVoiceShell({ sendBlocks: {},
      call: callState({ phase: 'blocked', channelId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' }, changedUser: null }) });
    expect(main().getByText(t('voice.blockedRetry'))).toBeInTheDocument();
    await app.user.click(main().getByRole('button', { name: 'Join voice' }));
    expect(app.client.calls.filter(c => c.m === 'callJoin')).toHaveLength(1);
  });

  it('a block of the DM text group keeps the call button hidden until the sendBlocks entry clears', async () => {
    const block = { code: 'E_DEVICE_UNLISTED' as const, own: false };
    const app = renderVoiceShell({ path: `/dm/${DM}`, sendBlocks: { [DM]: block },
      call: callState({ phase: 'blocked', channelId: DM, error: { ...UMK, code: 'E_DEVICE_UNLISTED' }, changedUser: null }) });
    expect(await main().findByText(t('voice.blockedJoin'))).toBeInTheDocument();
    expect(await dmMenuItems(app.user, 'bob')).not.toContain('call');
    act(() => app.client.publish('sendBlocks', {}));
    await waitFor(async () => expect(await dmMenuItems(app.user, 'bob')).toContain('call'));
  });

  it('a changed identity keeps the join hidden even with the sendBlocks entry clear', () => {
    renderVoiceShell({ sendBlocks: {}, trust: [bobChanged()], call: callState({ phase: 'blocked', channelId: GENV, error: UMK, changedUser: null }) });
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
  });

  // Web-3 task 10, concern 3: the block's cause verified clears it without a reload, named or not.
  it('a block by a named changed contact names them and offers the join again once they are verified', async () => {
    const app = renderVoiceShell({ sendBlocks: {}, trust: [bobChanged()],
      call: callState({ phase: 'blocked', channelId: GENV, error: UMK, changedUser: BOB }) });
    expect(main().getByText(t('voice.blockedChanged', { name: 'bob' }))).toBeInTheDocument();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    act(() => app.client.publish('trust', [{ ...bobChanged(), umk: 'cd'.repeat(32), verified: true, changedUmk: null }]));
    expect(await main().findByRole('button', { name: 'Join voice' })).toBeInTheDocument();
  });

  it('an unnamed changed-identity block offers the join again once no contact is changed any more', async () => {
    const app = renderVoiceShell({ sendBlocks: {}, trust: [bobChanged()],
      call: callState({ phase: 'blocked', channelId: GENV, error: UMK, changedUser: null }) });
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    act(() => app.client.publish('trust', [{ ...bobChanged(), umk: 'cd'.repeat(32), verified: true, changedUmk: null }]));
    expect(await main().findByRole('button', { name: 'Join voice' })).toBeInTheDocument();
  });

  it('a block by a changed contact keeps the join hidden while that contact is changed', () => {
    renderVoiceShell({ sendBlocks: {}, trust: [bobChanged()],
      call: callState({ phase: 'blocked', channelId: GENV, error: UMK, changedUser: BOB }) });
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
  });

  it('the voice row’s join is reachable from the keyboard: Tab from the row reaches it', async () => {
    const app = renderVoiceShell({ path: `/c/${A}/${TEXT}` });
    screen.getByRole('button', { name: 'voice general' }).focus();
    await app.user.tab();
    expect(document.activeElement).toHaveAccessibleName('join voice');
    expect(document.activeElement?.closest('.d-voice-item')).toContainElement(screen.getByRole('button', { name: 'voice general' }));
  });

  // Product walk M4: the SFU's own speaking (the session's state.speaking), no longer the meter level.
  it('the own tile shows speaking when the SFU hears this device', async () => {
    const app = await inVoice();
    const own = () => document.querySelector('.d-call-tile[data-own="true"]');
    expect(own()).toHaveAttribute('data-speaking', 'false');
    act(() => app.session.update({ level: 8 }));
    expect(own()).toHaveAttribute('data-speaking', 'false');
    act(() => app.session.update({ speaking: true }));
    expect(own()).toHaveAttribute('data-speaking', 'true');
  });

  it('I7: in a DM call a "messages" toggle swaps the stage for the log and composer', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await startDmCall(app.user, 'bob');
    await main().findByRole('toolbar', { name: 'call controls' });
    const box = document.querySelector('.dw-dm-call')!;
    expect(box).toHaveAttribute('data-view', 'stage');
    const toggle = main().getByRole('button', { name: 'messages' });
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    await app.user.click(toggle);
    expect(box).toHaveAttribute('data-view', 'messages');
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
  });
});

describe('AW5: the AFK channel in the shell never hosts a call', () => {
  const afkSlice = (over: Record<string, unknown> = {}) => ({ channelId: MUSIC, timeoutSeconds: 300, pending: false, seq: 1, away: [], ...over });
  const order = (app: ReturnType<typeof renderVoiceShell>) => app.client.calls.map(c => c.m).filter(m => m === 'callLeave' || m === 'afkAway' || m === 'callJoin');
  it('the AFK row is a voice row with no join icon; its desktop click starts no call and marks the person away', async () => {
    const app = renderVoiceShell();
    app.client.publish(`afk:${A}`, afkSlice());
    const rowButton = await screen.findByRole('button', { name: 'voice music' });
    const row = rowButton.closest('li') as HTMLElement;
    // Signal §5.4: the AFK row's glyph is the moon.
    expect(row.querySelector('svg[data-icon="moon"]')).not.toBeNull();
    expect(row.querySelector('[aria-disabled="true"]')).toBeNull();
    // Final review M4: no "join voice" icon on the AFK row; the page offers go AFK, and a desktop click goes AFK.
    expect(within(row).queryByRole('button', { name: /join/i })).toBeNull();
    await app.user.click(rowButton);
    await waitFor(() => expect(app.client.callsOf('afkAway')).toEqual([{ m: 'afkAway', communityId: A }]));
    expect(app.module.startPageCall).not.toHaveBeenCalled();
    expect(order(app)).toEqual(['afkAway']);
  });

  it('in an ordinary call, the AFK page’s go AFK leaves the call first, then marks the person away, and the dock goes', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, afkSlice());
    go(`/c/${A}/${MUSIC}`);
    expect(await screen.findByRole('toolbar', { name: t('voice.dock.region') })).toBeInTheDocument();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    await app.user.click(await main().findByRole('button', { name: t('afk.go') }));
    await waitFor(() => expect(app.client.callsOf('afkAway')).toHaveLength(1));
    expect(app.session.leave).toHaveBeenCalledWith('user');
    expect(order(app).slice(-2)).toEqual(['callLeave', 'afkAway']);
    expect(app.module.startPageCall).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('toolbar', { name: t('voice.dock.region') })).toBeNull();
    expect(screen.queryByText(/move to afk|leave afk/i)).toBeNull();
  });

  it('a read that says pending joins nothing', async () => {
    const app = renderVoiceShell();
    app.client.publish(`afk:${A}`, afkSlice({ pending: true, seq: 3 }));
    await new Promise(r => setTimeout(r, 200));
    expect(app.module.startPageCall).not.toHaveBeenCalled();
    expect(app.client.callsOf('callJoin')).toEqual([]);
  });

  it('the sweep’s cut says afk.moved on the stage and starts no call', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, afkSlice({ pending: true, seq: 2 }));
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV, callId: GENV }));
    expect(await main().findByText(t('afk.moved', { channel: 'music' }))).toBeInTheDocument();
    expect(main().queryByText(t('voice.removed'))).toBeNull();
    await new Promise(r => setTimeout(r, 200));
    expect(app.module.startPageCall).toHaveBeenCalledTimes(1);
    expect(app.client.callsOf('callJoin').filter(c => c.channelId === MUSIC)).toEqual([]);
    expect(document.querySelector('.d-banner')).toBeNull();
  });
});

describe('AW6: the AFK row lists the away roster', () => {
  const since = 1_900_000_000;
  const afkSlice = (away: { userId: string; since: number }[]) => ({ channelId: MUSIC, timeoutSeconds: 300, pending: false, seq: 1, away });
  const afkRow = async () => (await screen.findByRole('button', { name: 'voice music' })).closest('li') as HTMLElement;
  const people = (row: HTMLElement) => [...row.querySelectorAll<HTMLElement>('.d-voice-row__person')]
    .map(p => `${p.querySelector('.d-voice-row__person-name')?.textContent ?? ''}:${p.querySelector('[data-state="away"]')?.textContent ?? ''}`);
  it('lists an away person who is in no call, counted, and never the voice book’s people', async () => {
    // The voice book still names someone in the AFK channel (a web3.9 client): the row ignores it.
    const app = renderVoiceShell({ voice: { [MUSIC]: { callId: MUSIC, participants: [
      { userId: CAROL, deviceId: CAROL_DEV, name: 'carol', flags: 0, own: false, web: false, bot: false }] } } });
    app.client.publish(`afk:${A}`, afkSlice([{ userId: BOB, since }]));
    const row = await afkRow();
    await waitFor(() => expect(people(row)).toEqual([`bob:${t('afk.away')}`]));
    // Fix round 1 (M4): nobody there is in voice, so the count says where they are.
    expect(within(row).getByRole('button', { name: 'voice music' })).toHaveAccessibleDescription(t('voice.afk.count', { n: 1 }));
    expect(row.querySelector('[data-state="muted"]')).toBeNull();
  });

  it('updates on op 55: a mark adds a person, a clear (since 0) removes them', async () => {
    const app = renderVoiceShell();
    app.client.publish(`afk:${A}`, afkSlice([{ userId: BOB, since }]));
    const row = await afkRow();
    await waitFor(() => expect(people(row)).toEqual([`bob:${t('afk.away')}`]));
    // Op 55: carol is marked away (the worker patches the slice).
    app.client.publish(`afk:${A}`, afkSlice([{ userId: BOB, since }, { userId: CAROL, since: since + 1 }]));
    await waitFor(() => expect(people(row)).toEqual([`bob:${t('afk.away')}`, `carol:${t('afk.away')}`]));
    // An op 55 with since 0 clears bob (the server sends it after his message, typing or join: Q1, R-AW4).
    app.client.publish(`afk:${A}`, afkSlice([{ userId: CAROL, since: since + 1 }]));
    await waitFor(() => expect(people(row)).toEqual([`carol:${t('afk.away')}`]));
    app.client.publish(`afk:${A}`, afkSlice([]));
    await waitFor(() => expect(row.querySelector('.d-voice-row__people')).toBeNull());
  });
});

describe('AW6 fix round 1: the AFK channel’s page (Jonas, concern 2)', () => {
  const since = 1_900_000_000;
  const afkSlice = (away: { userId: string; since: number }[]) => ({ channelId: MUSIC, timeoutSeconds: 300, pending: false, seq: 1, away });
  const rosterOf = () => [...document.querySelectorAll<HTMLElement>('.dw-voice-stage .dw-afk-roster .d-voice-row__person')]
    .map(p => `${p.querySelector('.d-voice-row__person-name')?.textContent ?? ''}:${p.querySelector('[data-state="away"]')?.textContent ?? ''}`);
  it('lists the away roster, as the row does, with go AFK in place of join voice', async () => {
    const app = renderVoiceShell({ path: `/c/${A}/${MUSIC}` });
    app.client.publish(`afk:${A}`, afkSlice([{ userId: BOB, since }, { userId: CAROL, since }]));
    await waitFor(() => expect(rosterOf()).toEqual([`bob:${t('afk.away')}`, `carol:${t('afk.away')}`]));
    expect(main().getByRole('button', { name: t('afk.go') })).toBeInTheDocument();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    expect(main().queryByText(t('afk.empty'))).toBeNull();
    await expectNoAxeViolations(screen.getByRole('main'));
  });

  it('an empty roster says nobody is away', async () => {
    const app = renderVoiceShell({ path: `/c/${A}/${MUSIC}` });
    app.client.publish(`afk:${A}`, afkSlice([]));
    expect(await main().findByText(t('afk.empty'))).toBeInTheDocument();
    expect(rosterOf()).toEqual([]);
  });

  it('a person already away sees the away line and no button', async () => {
    const app = renderVoiceShell({ path: `/c/${A}/${MUSIC}` });
    app.client.publish(`afk:${A}`, afkSlice([{ userId: ME.id, since }]));
    expect(await main().findByText(t('afk.youAway'))).toBeInTheDocument();
    expect(main().queryByRole('button', { name: t('afk.go') })).toBeNull();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    expect(rosterOf()).toEqual([`ada:${t('afk.away')}`]);
  });

  it('M2: a call cut because AFK was chosen on another device says so as a calm notice on its stage', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, afkSlice([]));
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV, callId: GENV }));
    // Round 3: the instance's read gives this device's pending reason 2 (AFK chosen on another device or tab).
    app.client.publish(`afk:${A}`, { ...afkSlice([{ userId: ME.id, since }]), pending: true, pendingReason: 2, seq: 2 });
    const line = await main().findByText(t('afk.elsewhere'));
    expect(line.closest('[data-kind]')).toHaveAttribute('data-kind', 'info');
    expect(main().queryByText(t('voice.removed'))).toBeNull();
  });

  it('the away line that replaces go AFK takes its focus (fix round 1, M6)', async () => {
    const app = renderVoiceShell({ path: `/c/${A}/${MUSIC}` });
    app.client.publish(`afk:${A}`, afkSlice([]));
    await app.user.click(await main().findByRole('button', { name: t('afk.go') }));
    await waitFor(() => expect(app.client.callsOf('afkAway')).toHaveLength(1));
    app.client.publish(`afk:${A}`, afkSlice([{ userId: ME.id, since }]));
    await waitFor(() => expect(main().getByText(t('afk.youAway'))).toHaveFocus());
  });

  it('go AFK in a call leaves the call before it sends the away mark', async () => {
    const app = await inVoice();
    app.client.publish(`afk:${A}`, afkSlice([]));
    go(`/c/${A}/${MUSIC}`);
    await app.user.click(await main().findByRole('button', { name: t('afk.go') }));
    await waitFor(() => expect(app.client.callsOf('afkAway')).toEqual([{ m: 'afkAway', communityId: A }]));
    expect(app.client.calls.map(c => c.m).filter(m => m === 'callLeave' || m === 'afkAway')).toEqual(['callLeave', 'afkAway']);
    expect(app.module.startPageCall).toHaveBeenCalledTimes(1);
  });
});

describe('push to talk in the user panel (web3.9)', () => {
  const panelMic = () => within(screen.getByRole('toolbar', { name: t('panel.region') })).getByRole('button', { name: 'mute' });
  it('shows the PTT tag and the push to talk description in push-to-talk mode, the ring while heard', async () => {
    const app = await inVoice({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'F13' } });
    expect(screen.getByText(t('layout.ptt.tag'))).toHaveAttribute('aria-hidden', 'true');
    expect(panelMic()).toHaveAccessibleDescription(new RegExp(t('settings.voice.ptt')));
    const ring = () => document.querySelector('.d-user-panel__avatar');
    expect(ring()).not.toHaveAttribute('data-speaking');
    act(() => app.session.update({ speaking: true }));
    expect(ring()).toHaveAttribute('data-speaking', 'true');
  });
  it('shows none of it in voice-activity mode (control)', async () => {
    await inVoice();
    expect(screen.queryByText(t('layout.ptt.tag'))).toBeNull();
  });
});

describe('WEB-DOCK review: a DM call stage carries the hearing line', () => {
  it('shows "hearing {attached} of {expected}" under a DM call\'s controls', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await startDmCall(app.user, 'bob');
    await main().findByRole('toolbar', { name: 'call controls' });
    expect(document.querySelector('.dw-dm-call [data-audio-expected]')).toBeNull();
    act(() => app.session.update({ audio: { attached: 1, expected: 2 } }));
    const line = await waitFor(() => { const l = document.querySelector('.dw-dm-call [data-audio-expected]'); expect(l).not.toBeNull(); return l!; });
    expect(line).toHaveTextContent(t('voice.diag.tracks', { attached: 1, expected: 2 }));
  });
});

describe('web3.3: the browser holds call audio back', () => {
  it('the dock offers "turn on audio" with its hint; a key press starts audio; the line goes once audio plays', async () => {
    const app = await inVoice();
    const turnOn = () => screen.queryByRole('button', { name: t('voice.audioStart') });
    expect(turnOn()).toBeNull();
    expect(screen.queryByText(t('voice.audioBlocked'))).toBeNull();
    act(() => app.session.update({ audioBlocked: true }));
    const button = await screen.findByRole('button', { name: t('voice.audioStart') });
    expect(button).toHaveAccessibleDescription(t('voice.audioBlocked'));
    // Beside the call controls: in the dock, which shows wherever the person is while in a call.
    expect(button.closest('.dw-voice-dock')).not.toBeNull();
    // Review M1: the line is no status region of its own; the dock’s one status region says it, once per change.
    const line = document.querySelector('.dw-voice-dock__audio')!;
    expect(line.closest('[role="status"]')).toBeNull();
    const said = () => [...document.querySelectorAll('[role="status"]')].filter(r => r.textContent === t('voice.audioBlocked'));
    await waitFor(() => expect(said()).toHaveLength(1));
    expect(said()[0].closest('.dw-voice-dock')).not.toBeNull();
    // Said on the false→true change only: a later announcement is not overwritten by an update that keeps it blocked.
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await waitFor(() => expect(said()).toHaveLength(0));
    act(() => app.session.update({ level: 3 }));
    expect(said()).toHaveLength(0);
    button.focus();
    await app.user.keyboard('{Enter}');
    expect(app.session.startAudio).toHaveBeenCalledTimes(1);
    act(() => app.session.update({ audioBlocked: false }));
    await waitFor(() => expect(turnOn()).toBeNull());
    expect(screen.queryByText(t('voice.audioBlocked'))).toBeNull();
  });
});

describe('web3.4 V2: the stage says how many it hears (web3.9: it left the dock)', () => {
  it('shows "hearing {attached} of {expected}" with its counts while any remote audio is expected, and nothing otherwise', async () => {
    const app = await inVoice();
    const line = () => document.querySelector('.dw-voice-stage [data-audio-expected]'); // web3.9 R26: on the stage, no longer in the dock
    expect(line()).toBeNull();
    // Deafened: a newcomer's track is expected but held back.
    act(() => app.session.update({ audio: { attached: 1, expected: 2 } }));
    const shown = await waitFor(() => { const l = line(); expect(l).not.toBeNull(); return l!; });
    expect(shown).toHaveTextContent(t('voice.diag.tracks', { attached: 1, expected: 2 }));
    expect(shown).toHaveAttribute('data-audio-attached', '1');
    expect(shown).toHaveAttribute('data-audio-expected', '2');
    // A diagnostic, not news: no status region of its own (the dock's one region says what changes).
    expect(shown.closest('[role="status"]')).toBeNull();
    await expectNoAxeViolations(document.querySelector('.d-call-stage__stats')!);
    act(() => app.session.update({ audio: { attached: 2, expected: 2 } }));
    await waitFor(() => expect(line()).toHaveAttribute('data-audio-attached', '2'));
    expect(line()).toHaveTextContent(t('voice.diag.tracks', { attached: 2, expected: 2 }));
    act(() => app.session.update({ audio: { attached: 0, expected: 0 } }));
    await waitFor(() => expect(line()).toBeNull());
  });

  it('fix 1: no hearing line while deafened or paused (nothing plays then); it returns after', async () => {
    const app = await inVoice();
    const shown = () => screen.queryByText(t('voice.diag.tracks', { attached: 2, expected: 2 }));
    act(() => app.session.update({ audio: { attached: 2, expected: 2 } }));
    await waitFor(() => expect(shown()).not.toBeNull());
    act(() => app.session.update({ deafened: true, muted: true }));
    await waitFor(() => expect(shown()).toBeNull());
    act(() => app.session.update({ deafened: false }));
    await waitFor(() => expect(shown()).not.toBeNull());
    act(() => app.session.update({ paused: true }));
    await waitFor(() => expect(shown()).toBeNull());
    act(() => app.session.update({ paused: false }));
    await waitFor(() => expect(shown()).not.toBeNull());
  });
});

describe('web3.3 fix round 2: the call context is made in the press (Minor 1); the hold joins the join announcement (Nit 3)', () => {
  it('makes the call’s AudioContext synchronously in the join press and hands that one to the session', async () => {
    const app = renderVoiceShell();
    const button = await main().findByRole('button', { name: 'Join voice' });
    fireEvent.click(button);
    // Nothing of the join was awaited yet: the leave in flight, the module load and a switch’s leave all come after.
    expect(app.callContexts).toHaveLength(1);
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    expect(app.startedWith).toEqual([app.callContexts[0]]);
    expect(app.callContexts[0].closed).toBe(false);
  });

  it('closes the context when the browser cannot do voice, and never starts a call', async () => {
    const app = renderVoiceShell({ supported: false });
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    expect(await screen.findByRole('dialog', { name: 'Voice does not work in this browser' })).toBeInTheDocument();
    expect(app.callContexts).toHaveLength(1);
    expect(app.callContexts[0].closed).toBe(true);
    expect(app.startedWith).toEqual([]);
  });

  it('closes the context when the call module fails to load', async () => {
    const app = renderVoiceShell({ loadFails: true });
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.callContexts[0]?.closed).toBe(true));
    expect(app.startedWith).toEqual([]);
  });

  it('closes the context when building the start’s arguments throws before the session gets it', async () => {
    const app = renderVoiceShell();
    app.module.browserDeps = () => { throw new Error('E_INTERNAL'); };
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.callContexts[0]?.closed).toBe(true));
    expect(app.startedWith).toEqual([]);
  });

  it('closes the context of a join cancelled by another before it started; the next join brings its own', async () => {
    const app = await inVoice();
    let release: () => void = () => undefined;
    app.session.leaveGate = new Promise<void>(resolve => { release = resolve; });
    // A row click joins on a desktop (web3.9), so the stage is opened by the route here and joined from its button.
    const go = (id: string) => act(() => { window.history.pushState(null, '', `/c/${A}/${id}`); window.dispatchEvent(new PopStateEvent('popstate')); });
    go(MUSIC);
    await waitFor(() => expect(window.location.pathname).toContain(MUSIC));
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.log).toContain('leave:switch'));
    // While the switch’s leave runs, the person picks general again: the music join is superseded before it started.
    go(GENV);
    await waitFor(() => expect(window.location.pathname).toContain(GENV));
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    expect(app.callContexts).toHaveLength(3);
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(2));
    expect(app.callContexts[1].closed).toBe(true);
    expect(app.startedWith).toEqual([app.callContexts[0], app.callContexts[2]]);
    expect(app.callContexts[2].closed).toBe(false);
  });

  for (const when of ['at-start', 'same-tick'] as const) {
    it(`a hold ${when === 'at-start' ? 'the call starts in' : 'that arrives in the same tick as the join'} is said with "You joined", not instead of it`, async () => {
      const app = renderVoiceShell({ audioBlocked: when });
      await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
      await waitFor(() => expect(app.sessions).toHaveLength(1));
      const said = () => [...document.querySelectorAll('.dw-voice-dock [role="status"]')].map(r => r.textContent ?? '');
      await waitFor(() => expect(said().some(s => /^You joined /.test(s) && s.endsWith(` ${t('voice.audioBlocked')}`))).toBe(true));
    });
  }
});

// web3.7: the dock holds the controls only while the stage is off screen, and the stage holds them while it is on, so each
// test reads one, steps across (offStage, onStage), and reads the other. The call stays up across the step.
describe('VI-2: camera and screen share in the voice dock (plan 2026-10-15)', () => {
  const dock = () => within(screen.getByRole('toolbar', { name: 'voice controls' }));
  const userPanel = () => within(screen.getByRole('toolbar', { name: t('panel.region') }));
  const stage = () => within(main().getByRole('toolbar', { name: 'voice channel controls' }));
  // jsdom has no getDisplayMedia; the stage and the dock both read it, so give them one.
  const withCapture = () => {
    Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: { getDisplayMedia: vi.fn() } });
  };
  afterEach(() => { Reflect.deleteProperty(navigator, 'mediaDevices'); });

  // Signal §5.4 (Ruling U): the dock is the prototype's, leave alone; camera and share stay the stage's controls under its rules.
  it('the dock holds leave alone; camera and share are the stage’s toggles', async () => {
    withCapture();
    const app = await inVoice();
    await app.user.click(stage().getByRole('button', { name: t('callStage.bar.camera') }));
    await waitFor(() => expect(app.session.state.camera).toBe(true));
    await offStage();
    expect(dock().getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual([t('voice.leave')]);
    await onStage();
    await waitFor(() => expect(stage().getByRole('button', { name: t('callStage.bar.camera') })).toHaveAttribute('aria-pressed', 'true'));
    await app.user.click(stage().getByRole('button', { name: t('callStage.bar.share') }));
    // Task 8c: Share opens the picker; Go live starts the share.
    await app.user.click(main().getByRole('button', { name: t('callStage.share.go') }));
    await waitFor(() => expect(app.session.state.sharing).toBe(true));
  });

  // Task 8c (Signal §5.6): the picker's choices reach the session; the estimate is the encoding's own, under the instance's cap.
  it('the share picker starts the share with its options, and Apply hands new ones to the running share', async () => {
    withCapture();
    const app = await inVoice();
    await app.user.click(stage().getByRole('button', { name: t('callStage.bar.share') }));
    const picker = within(main().getByRole('dialog', { name: t('callStage.share.title') }));
    // jsdom is no Chromium: no source row and no sound row (the browser would ignore both).
    expect(picker.queryByRole('radiogroup', { name: t('callStage.share.source') })).toBeNull();
    expect(picker.queryByRole('switch')).toBeNull();
    expect(picker.getByText(t('callStage.share.estimate', { mbps: '2.5' }))).toBeInTheDocument();
    await app.user.click(picker.getByRole('radio', { name: '720p' }));
    await app.user.click(picker.getByRole('radio', { name: '15' }));
    expect(picker.getByText(t('callStage.share.estimate', { mbps: '1.5' }))).toBeInTheDocument();
    await app.user.click(picker.getByRole('button', { name: t('callStage.share.go') }));
    await waitFor(() => expect(app.session.state.sharing).toBe(true));
    expect(app.session.shareOptions).toEqual({ surface: 'monitor', resolution: '720p', fps: 15, systemAudio: false });
    expect(main().queryByRole('dialog')).toBeNull();
    await app.user.click(stage().getByRole('button', { name: t('callStage.share.quality') }));
    const quality = within(main().getByRole('dialog', { name: t('callStage.share.titleSharing') }));
    expect(quality.getByRole('radio', { name: '720p' })).toHaveAttribute('aria-checked', 'true');
    await app.user.click(quality.getByRole('radio', { name: '1080p' }));
    await app.user.click(quality.getByRole('button', { name: t('callStage.share.apply') }));
    expect(app.session.setShareOptions).toHaveBeenCalledWith({ surface: 'monitor', resolution: '1080p', fps: 15, systemAudio: false });
    await app.user.click(stage().getByRole('button', { name: t('callStage.bar.stopShare') }));
    await waitFor(() => expect(app.session.state.sharing).toBe(false));
  });

  it('without screen capture, share is aria-disabled on the stage, and the dock has none', async () => {
    await inVoice();
    expect(stage().getByRole('button', { name: t('callStage.bar.share') })).toHaveAttribute('aria-disabled', 'true');
    await offStage();
    expect(dock().queryByRole('button', { name: t('voice.shareOn') })).toBeNull();
  });

  it('while paused, the stage’s camera and share are aria-disabled; the dock’s leave and the panel’s mute and deafen stay live', async () => {
    withCapture();
    const app = await inVoice({ trust: [bobChanged()], withSettings: true });
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await screen.findByText('paused in general');
    for (const name of [t('callStage.bar.camera'), t('callStage.bar.share')]) expect(stage().getByRole('button', { name })).toHaveAttribute('aria-disabled', 'true');
    await offStage();
    expect(dock().getByRole('button', { name: t('voice.leave') })).not.toHaveAttribute('aria-disabled');
    for (const name of [t('voice.mute'), t('voice.deafen')]) expect(userPanel().getByRole('button', { name })).not.toHaveAttribute('aria-disabled');
    expect(app.session.state.camera).toBe(false);
  });

  it('while joining, the user panel’s mute and deafen and the dock’s leave stay live', async () => {
    withCapture();
    const app = renderVoiceShell();
    const release = app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    await offStage();
    expect(dock().getByRole('button', { name: t('voice.leave') })).not.toHaveAttribute('aria-disabled');
    for (const name of [t('voice.mute'), t('voice.deafen')]) expect(userPanel().getByRole('button', { name })).not.toHaveAttribute('aria-disabled');
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
  });

  it('only the user panel’s mute and deafen carry aria-keyshortcuts and are described by their shortcut; the stage’s tip shows it and they carry none', async () => {
    await inVoice();
    expect(stage().getByRole('button', { name: t('callStage.bar.mute') })).not.toHaveAttribute('aria-keyshortcuts');
    expect(stage().getByRole('button', { name: t('callStage.bar.deafen') })).not.toHaveAttribute('aria-keyshortcuts');
    expect(userPanel().getByRole('button', { name: t('voice.mute') })).toHaveAccessibleDescription('Ctrl Shift M');
    expect(userPanel().getByRole('button', { name: t('voice.deafen') })).toHaveAccessibleDescription('Ctrl Shift D');
    await offStage();
    expect(userPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+M Meta+Shift+M');
    expect(dock().queryByRole('button', { name: t('voice.mute') })).toBeNull();
    expect([...document.querySelectorAll('[aria-keyshortcuts]')].filter(e => e.getAttribute('aria-keyshortcuts')!.includes('Shift+M')))
      .toHaveLength(1);
  });

  // Jonas, 2026-10-15: a disabled camera or share says why (voice.why.*) on the stage; the name stays. (The dock has none, Ruling U.)
  it('a disabled camera or share says why: not available, paused', async () => {
    const app = await inVoice();
    const share = stage().getByRole('button', { name: t('callStage.bar.share') });
    expect(share).toHaveAccessibleName(t('callStage.bar.share'));
    expect(share).toHaveAccessibleDescription(t('voice.why.noShare'));
    expect(stage().getByRole('button', { name: t('callStage.bar.camera') })).not.toHaveAttribute('aria-describedby');
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await screen.findByText('paused in general');
    for (const name of [t('callStage.bar.camera'), t('callStage.bar.share')]) {
      expect(stage().getByRole('button', { name })).toHaveAccessibleDescription(t('voice.why.paused'));
    }
  });
});

describe('voiceShortcutText', () => {
  it('writes ⌘⇧M and ⌘⇧D on Apple platforms and Ctrl+Shift+M and Ctrl+Shift+D elsewhere', () => {
    expect(voiceShortcutText(true)).toEqual({ mute: '⌘⇧M', deafen: '⌘⇧D' });
    expect(voiceShortcutText(false)).toEqual({ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' });
  });

  it('reads the platform from userAgentData first, then navigator.platform', () => {
    expect(isApplePlatform({ userAgentData: { platform: 'macOS' }, platform: 'Linux x86_64' })).toBe(true);
    expect(isApplePlatform({ platform: 'MacIntel' })).toBe(true);
    expect(isApplePlatform({ platform: 'iPhone' })).toBe(true);
    expect(isApplePlatform({ userAgentData: { platform: 'Windows' }, platform: 'MacIntel' })).toBe(false);
    expect(isApplePlatform({ platform: 'Linux x86_64' })).toBe(false);
    expect(isApplePlatform({ platform: '' })).toBe(false);
    // Review round 1 (#7): Chromium reports an empty UA-CH platform when it does not know; navigator.platform decides then.
    expect(isApplePlatform({ userAgentData: { platform: '' }, platform: 'MacIntel' })).toBe(true);
  });
});

describe('web3.7: one set of call controls', () => {
  it('the dock keeps its toolbar while the stage is shown', async () => {
    await inVoice();
    expect(document.querySelector('.dw-voice-dock .d-voice-dock__controls')).not.toBeNull();
    expect(main().getByRole('toolbar', { name: 'voice channel controls' })).toBeInTheDocument();
    go(`/c/${A}/${TEXT}`);
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull());
    expect(document.querySelector('.dw-voice-dock .d-voice-dock__controls')).not.toBeNull();
  });

  it('repeats no element id while both toolbars are on screen', async () => {
    await inVoice();
    const ids = [...document.querySelectorAll('[id]')].map(e => e.id);
    expect(ids.filter((id, i) => ids.indexOf(id) !== i)).toEqual([]);
  });

  it('join focuses the stage’s first control, and ArrowRight in the dock toolbar leaves the stage’s focus alone', async () => {
    const app = await inVoice();
    const first = () => main().getByRole('toolbar', { name: 'voice channel controls' }).querySelector('button')!;
    await waitFor(() => expect(first()).toHaveFocus());
    const dockFirst = screen.getByRole('toolbar', { name: 'voice controls' }).querySelector('button')!;
    dockFirst.focus();
    await app.user.keyboard('{ArrowRight}');
    expect(screen.getByRole('toolbar', { name: 'voice controls' }).contains(document.activeElement)).toBe(true);
    // Ruling U: leave is the dock's one control, so the arrow keeps focus on it.
    expect(document.activeElement).toBe(dockFirst);
    expect(main().getByRole('toolbar', { name: 'voice channel controls' }).contains(document.activeElement)).toBe(false);
  });
});

// A2 (web3.7.2): a phone that cannot capture a screen shows no share control; desktop keeps it, switched off with its reason.
describe('screen share without getDisplayMedia', () => {
  const asPhone = (phone: boolean) => {
    window.matchMedia = vi.fn((query: string) => ({
      matches: query === PHONE_QUERY ? phone : false, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
    })) as unknown as typeof window.matchMedia;
  };
  afterEach(() => { Reflect.deleteProperty(window, 'matchMedia'); });

  it('on a phone there is no share button on the stage', async () => {
    asPhone(true);
    await inVoice();
    const stage = within(main().getByRole('toolbar', { name: 'voice channel controls' }));
    expect(stage.queryByRole('button', { name: t('callStage.bar.share') })).toBeNull();
    expect(stage.getByRole('button', { name: t('callStage.bar.camera') })).toBeInTheDocument();
  });

  it('on a phone the stage’s mute and deafen carry aria-keyshortcuts', async () => {
    asPhone(true);
    await inVoice();
    const bar = () => within(main().getByRole('toolbar', { name: 'voice channel controls' }));
    expect(bar().getByRole('button', { name: t('callStage.bar.mute') })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+M Meta+Shift+M');
    expect(bar().getByRole('button', { name: t('callStage.bar.deafen') })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+D Meta+Shift+D');
  });

  it('on desktop the share button stays, aria-disabled', async () => {
    asPhone(false);
    await inVoice();
    const share = within(main().getByRole('toolbar', { name: 'voice channel controls' })).getByRole('button', { name: t('callStage.bar.share') });
    expect(share).toHaveAttribute('aria-disabled', 'true');
  });
});

// web3.8 WS7: one tile per source (camera, screen, avatar), and R26's one auto-focus rule.
describe('the stage has one tile per source (web3.8 WS7)', () => {
  if (typeof globalThis.MediaStream === 'undefined') {
    (globalThis as { MediaStream?: unknown }).MediaStream = class MediaStream {};
  }
  const room = (flags: number) => ({ [GENV]: { callId: GENV, participants: [
    { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags, own: false, web: false, bot: false },
    { userId: 'carl', deviceId: 'c1', name: 'carl', flags: 0, own: false, web: false, bot: false },
    { userId: ME.id, deviceId: OWN_DEV, name: 'me', flags: 0, own: true, web: true, bot: false },
  ] } });
  const FLAG_MUTE = 1 << 1;
  const FLAG_SCREEN = 1 << 4;
  const ids = () => [...document.querySelectorAll<HTMLElement>('li.d-call-stage__tile')].map(e => e.dataset.tileId);
  const li = (id: string) => document.querySelector<HTMLElement>(`li.d-call-stage__tile[data-tile-id="${id}"]`)!;
  const focusedIds = () => [...document.querySelectorAll<HTMLElement>('li.d-call-stage__tile[data-focused="true"]')].map(e => e.dataset.tileId);
  const tile = (deviceId: string, userId: string, video: MediaStream | null, screen: MediaStream | null) =>
    ({ deviceId, userId, state: 'ok' as const, speaking: false, video, screen, screenAudio: false });

  it('a participant with a camera and a screen gives two tiles, "bob" and "bob’s screen", with no inset', async () => {
    const app = await inVoice({ voice: room(0) });
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, new MediaStream(), new MediaStream())] }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
    expect(ids()).toContain(`${BOB_DEV}:person`);
    expect(within(li(`${BOB_DEV}:person`)).getByRole('button', { name: 'bob' })).toBeInTheDocument();
    expect(within(li(`${BOB_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'bob' }) })).toBeInTheDocument();
    expect(document.querySelector('.d-call-tile__inset')).toBeNull();
  });

  // Task 8c fix round 1: another person's ping is the RTT their device publishes; without one their tile has no stat.
  it('a person tile shows the ping its device publishes ("31 ms"), and none without one; their screen tile never shows it', async () => {
    const app = await inVoice({ voice: room(FLAG_SCREEN) });
    act(() => app.session.update({ tiles: [{ ...tile(BOB_DEV, BOB, null, new MediaStream()), rttMs: 31 }] }));
    await waitFor(() => expect(li(`${BOB_DEV}:person`).querySelector('.d-call-tile__stat')).toHaveTextContent('31 ms'));
    act(() => app.session.update({ tiles: [{ ...tile(BOB_DEV, BOB, null, new MediaStream()), rttMs: null }] }));
    await waitFor(() => expect(li(`${BOB_DEV}:person`).querySelector('.d-call-tile__stat')).toBeNull());
  });

  it('a participant who shares only a screen keeps an avatar tile with the muted glyph; neither tile has a range of its own', async () => {
    const app = await inVoice({ voice: room(FLAG_MUTE | FLAG_SCREEN) });
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
    const person = li(`${BOB_DEV}:person`);
    expect(person).toHaveAttribute('data-source', 'avatar');
    expect(within(person).getByRole('img', { name: 'muted' })).toBeInTheDocument();
    expect(within(person).queryByRole('slider')).toBeNull();
    expect(within(li(`${BOB_DEV}:screen`)).queryByRole('slider')).toBeNull();
    expect(within(li(`${BOB_DEV}:screen`)).queryByRole('img', { name: 'muted' })).toBeNull();
  });

  it('#37: a remote screen without sound shows the mark; with sound it does not; the own screen never does', async () => {
    const app = await inVoice({ voice: room(FLAG_SCREEN) });
    const screenTile = (over: Partial<ReturnType<typeof tile>>) => ({ ...tile(BOB_DEV, BOB, null, new MediaStream()), ...over });
    act(() => app.session.update({ tiles: [screenTile({ screenAudio: false })], sharing: true, localScreen: new MediaStream() }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
    expect(within(li(`${BOB_DEV}:screen`)).getByRole('img', { name: t('voice.state.noSound') })).toBeInTheDocument();
    expect(li(`${OWN_DEV}:screen`).querySelector('[data-state="no-sound"]')).toBeNull();
    expect(within(li(`${BOB_DEV}:screen`)).queryByRole('slider')).toBeNull();
    act(() => app.session.update({ tiles: [screenTile({ screenAudio: true })] }));
    await waitFor(() => expect(li(`${BOB_DEV}:screen`).querySelector('[data-state="no-sound"]')).toBeNull());
    // The mark is about the stream, not the person: bob's person tile never says it.
    expect(li(`${BOB_DEV}:person`).querySelector('[data-state="no-sound"]')).toBeNull();
  });

  it('#37 T5, fix round 1: a screen with sound has its own volume and Mute for me in its person menu; muting sets the stream volume to 0', async () => {
    const app = await inVoice({ voice: room(FLAG_SCREEN), settings: { [`voice.streamVolume.${BOB}`]: '70' } });
    act(() => app.session.update({ tiles: [{ ...tile(BOB_DEV, BOB, null, new MediaStream()), screenAudio: true }] }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
    expect(within(li(`${BOB_DEV}:screen`)).queryByRole('img', { name: t('voice.state.noSound') })).toBeNull();
    fireEvent.contextMenu(li(`${BOB_DEV}:screen`), { clientX: 20, clientY: 20 });
    const menu = await screen.findByRole('menu', { name: t('callStage.menu.label', { name: 'bob' }) });
    expect(screen.getByRole('slider', { name: t('callStage.menu.streamVolume') })).toHaveValue('70');
    await app.user.click(within(menu).getByRole('menuitemcheckbox', { name: t('callStage.menu.muteForMe') }));
    expect(app.session.setVolume).toHaveBeenCalledWith(BOB, 0, 'stream');
    expect(app.client.callsOf('setSetting')).toContainEqual({ m: 'setSetting', key: `voice.streamVolume.${BOB}`, value: '0' });
    expect(app.client.callsOf('setSetting').some(c => c.key === `voice.volume.${BOB}`)).toBe(false);
    await app.user.keyboard('{Escape}');
    // The person's own voice: User volume in the person tile's menu, a different key.
    fireEvent.contextMenu(li(`${BOB_DEV}:person`), { clientX: 20, clientY: 20 });
    expect(await screen.findByRole('slider', { name: t('callStage.menu.volume') })).toHaveValue('100');
  });

  it('fix round 1: a roster entry without an SFU tile renders data-voice-state="connecting"', async () => {
    const app = await inVoice({ voice: room(0) });
    act(() => app.session.update({ tiles: [{ ...tile(BOB_DEV, BOB, null, null), state: 'connecting' }] }));
    await waitFor(() => expect(li(`${BOB_DEV}:person`).querySelector('.d-call-tile')).toHaveAttribute('data-voice-state', 'connecting'));
  });

  it('an audio-only participant gives one avatar tile', async () => {
    const app = await inVoice({ voice: room(0) });
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:person`));
    expect(ids().filter(i => i?.startsWith(BOB_DEV))).toEqual([`${BOB_DEV}:person`]);
  });

  it('Signal §5.6: the people of a call this device has not joined are the preview\'s faces, not tiles', async () => {
    renderVoiceShell({ voice: room(0) });
    await waitFor(() => expect(document.querySelectorAll('.d-call-stage__face').length).toBeGreaterThan(0));
    expect(ids()).toEqual([]);
    expect(main().getByRole('button', { name: t('callStage.preview.join') })).toBeInTheDocument();
  });

  it('the own camera tile is mirrored and the own screen tile is not', async () => {
    const app = await inVoice({ voice: room(0) });
    const cam = new MediaStream();
    const scr = new MediaStream();
    act(() => app.session.update({ camera: true, localCamera: cam, sharing: true, localScreen: scr }));
    await waitFor(() => expect(ids()).toContain(`${OWN_DEV}:screen`));
    expect(li(`${OWN_DEV}:person`).querySelector('video')).toHaveAttribute('data-mirror', 'true');
    expect(li(`${OWN_DEV}:person`).querySelector('video')!.srcObject).toBe(cam);
    expect(li(`${OWN_DEV}:screen`).querySelector('video')).not.toHaveAttribute('data-mirror');
    expect(li(`${OWN_DEV}:screen`).querySelector('video')!.srcObject).toBe(scr);
  });

  it('the person tile keeps its id when the camera goes on and off, so a focused person stays focused', async () => {
    const app = await inVoice({ voice: room(0) });
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] }));
    await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:person`));
    await app.user.click(within(li(`${BOB_DEV}:person`)).getByRole('button', { name: 'bob' }));
    expect(focusedIds()).toEqual([`${BOB_DEV}:person`]);
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, new MediaStream(), null)] }));
    await waitFor(() => expect(li(`${BOB_DEV}:person`)).toHaveAttribute('data-source', 'camera'));
    expect(focusedIds()).toEqual([`${BOB_DEV}:person`]);
    act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] }));
    await waitFor(() => expect(li(`${BOB_DEV}:person`)).toHaveAttribute('data-source', 'avatar'));
    expect(focusedIds()).toEqual([`${BOB_DEV}:person`]);
  });

  it('the own tile and its screen are named by the display name, as the user panel shows it', async () => {
    const app = await inVoice({ voice: room(0) });
    app.client.publish(`members:${A}`, MEMBERS.map(m => m.userId === ME.id ? { ...m, display: 'Ada L' } : m));
    act(() => app.session.update({ sharing: true, localScreen: new MediaStream() }));
    await waitFor(() => expect(ids()).toContain(`${OWN_DEV}:screen`));
    // Signal §5.6: the own tile is "{name} (you)", as the prototype's "Mira (you)".
    expect(within(li(`${OWN_DEV}:person`)).getByRole('button', { name: t('callStage.tile.you', { name: 'Ada L' }) })).toBeInTheDocument();
    expect(within(li(`${OWN_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'Ada L' }) })).toBeInTheDocument();
  });

  describe('R26: auto-focus', () => {
    // Task 8c fix round 2 (Ruling AV, superseding R26's "never the own share"): the own share takes the focus when it starts, as
    // the prototype's Go live does; another person's new share then does not steal it.
    it('the own share takes the focus when it starts, and a later share of another person does not steal it', async () => {
      const app = await inVoice({ voice: room(0) });
      act(() => app.session.update({ sharing: true, localScreen: new MediaStream() }));
      await waitFor(() => expect(focusedIds()).toEqual([`${OWN_DEV}:screen`]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
      expect(focusedIds()).toEqual([`${OWN_DEV}:screen`]);
    });

    it('another person’s new screen takes the focus when nothing is focused', async () => {
      const app = await inVoice({ voice: room(0) });
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
    });

    it('after Escape the same share does not take the focus again, and a second share does not steal it', async () => {
      const app = await inVoice({ voice: room(0) });
      const bobScreen = new MediaStream();
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen)] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen), tile('c1', 'carl', null, new MediaStream())] }));
      await waitFor(() => expect(ids()).toContain('c1:screen'));
      expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]);
      within(li(`${BOB_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'bob' }) }).focus();
      await app.user.keyboard('{Escape}');
      await waitFor(() => expect(focusedIds()).toEqual([]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, new MediaStream(), bobScreen), tile('c1', 'carl', null, new MediaStream())] }));
      await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:person`));
      expect(focusedIds()).toEqual([]);
    });

    it('a pause and a resume, which withhold every remote stream, do not focus an unfocused share again', async () => {
      const app = await inVoice({ voice: room(FLAG_SCREEN) });
      const bobScreen = new MediaStream();
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen)] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      within(li(`${BOB_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'bob' }) }).focus();
      await app.user.keyboard('{Escape}');
      await waitFor(() => expect(focusedIds()).toEqual([]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] })); // paused: the room still says bob shares
      await waitFor(() => expect(ids()).not.toContain(`${BOB_DEV}:screen`));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen)] })); // resumed
      await waitFor(() => expect(ids()).toContain(`${BOB_DEV}:screen`));
      expect(focusedIds()).toEqual([]);
    });

    it('a share that really ended (the room flag is gone) and restarted may focus again', async () => {
      const app = await inVoice({ voice: room(FLAG_SCREEN) });
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      within(li(`${BOB_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'bob' }) }).focus();
      await app.user.keyboard('{Escape}');
      await waitFor(() => expect(focusedIds()).toEqual([]));
      app.client.publish('voice', room(0));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] }));
      await waitFor(() => expect(ids()).not.toContain(`${BOB_DEV}:screen`));
      app.client.publish('voice', room(FLAG_SCREEN));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
    });

    it('when the focused share ends, another person’s new share takes the focus', async () => {
      const app = await inVoice({ voice: room(0) });
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null), tile('c1', 'carl', null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual(['c1:screen']));
    });

    it('leaving and joining again starts with a clean set: the same share focuses again', async () => {
      const app = await inVoice({ voice: room(FLAG_SCREEN) });
      const bobScreen = new MediaStream();
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen)] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      within(li(`${BOB_DEV}:screen`)).getByRole('button', { name: t('stage.screen', { name: 'bob' }) }).focus();
      await app.user.keyboard('{Escape}');
      await waitFor(() => expect(focusedIds()).toEqual([]));
      await app.user.click(main().getByRole('button', { name: 'Leave' }));
      await main().findByRole('button', { name: 'Join voice' });
      await app.user.click(main().getByRole('button', { name: 'Join voice' }));
      await waitFor(() => expect(app.sessions).toHaveLength(2));
      app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
      await main().findByRole('toolbar', { name: 'voice channel controls' });
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, bobScreen)] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
    });

    it('the focused tile leaving returns the stage to the grid, and a new share focuses again', async () => {
      const app = await inVoice({ voice: room(0) });
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, null)] }));
      await waitFor(() => expect(ids()).not.toContain(`${BOB_DEV}:screen`));
      expect(focusedIds()).toEqual([]);
      expect(document.querySelector('[data-focused-stage]')).toBeNull();
      act(() => app.session.update({ tiles: [tile(BOB_DEV, BOB, null, new MediaStream())] }));
      await waitFor(() => expect(focusedIds()).toEqual([`${BOB_DEV}:screen`]));
    });
  });
});

// web3.9 (C4): a voice channel's stage opens with the channel header in its 48 bar; a DM call has none.
describe('the voice stage header (web3.9)', () => {
  it('shows the voice channel header in the stage bar on a desktop, with the volume glyph', async () => {
    renderVoiceShell({ path: `/c/${A}/${GENV}` });
    const stage = (await main().findByRole('button', { name: 'Join voice' })).closest('section') as HTMLElement;
    const bar = stage.querySelector('.d-call-stage__bar') as HTMLElement;
    expect(within(bar).getByRole('heading', { name: 'general' })).toBeInTheDocument();
    expect(bar.querySelector('svg[data-icon="volume-2"]')).not.toBeNull();
  });

  it('a DM call stage has no channel header', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await startDmCall(app.user, 'bob');
    await main().findByRole('toolbar', { name: 'call controls' });
    expect(document.querySelector('.d-call-stage__bar')).toBeNull();
  });
});

// web3.9 WEB-DOCK (R26): the dock as Discord's: no meter, the channel name over the link stats, three icon buttons; speaking is a
// ring on the list row and on the tile and nothing else.
describe('WEB-DOCK: the voice dock as Discord\'s, and speaking as a ring', () => {
  const dockEl = () => document.querySelector<HTMLElement>('.dw-voice-dock')!;
  const remote = (speaking: boolean) => [{ deviceId: BOB_DEV, userId: BOB, state: 'ok' as const, speaking, video: null, screen: null, screenAudio: false }];

  it('has no meter, no level text and no line, and keeps the dock to its two lines and three buttons', async () => {
    const app = await inVoice({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'Pause' } });
    act(() => app.session.update({ level: 40, audio: { attached: 1, expected: 2 } }));
    await offStage();
    const dock = dockEl();
    expect(dock.querySelector('.d-audio-meter, [role="meter"], .d-voice-dock__meter')).toBeNull();
    expect(dock).not.toHaveTextContent(t('settings.voice.level'));
    expect(dock.querySelector('.dw-voice-dock__mode, [data-audio-expected]')).toBeNull();
  });

  it('MP5: line 1 is the channel name (the server in its tooltip and accessible text); Ruling U: it is text, not a button', async () => {
    await inVoice();
    await offStage();
    expect(within(dockEl()).queryByRole('button', { name: 'general · Midgard' })).toBeNull();
    expect(dockEl().querySelector('.d-voice-dock__line1')).toHaveAttribute('title', 'general · Midgard');
    expect(dockEl().querySelector('.d-voice-dock__text')).toHaveTextContent(/^general$/);
    expect(dockEl()).not.toHaveTextContent('voice connected');
  });

  it('MP5: line 2 reads "— ms" before the first sample, then the ping and rates from call.state.link, naming the quality', async () => {
    const app = await inVoice();
    await offStage();
    const stats = () => dockEl().querySelector('.d-voice-dock__stats')!;
    expect(dockEl().querySelector('.d-voice-dock__where')).toHaveTextContent(/^— ms$/);
    act(() => app.session.update({ link: { rttMs: 24, upKbps: 48, downKbps: 96, lossPct: 0 } }));
    await waitFor(() => expect(dockEl().querySelector('.d-voice-dock__where')).toHaveTextContent('24 ms · ↑ 48 ↓ 96 kb/s'));
    expect(stats()).toHaveAttribute('data-quality', 'good');
    expect(stats()).toHaveTextContent('Ping 24 milliseconds, good. Sending 48, receiving 96 kilobits per second.');
    act(() => app.session.update({ link: { rttMs: 0, upKbps: 1, downKbps: 0, lossPct: 0 } }));
    await waitFor(() => expect(dockEl().querySelector('.d-voice-dock__where')).toHaveTextContent('<1 ms · ↑ 1 ↓ 0 kb/s'));
    act(() => app.session.update({ link: { rttMs: 210, upKbps: 5, downKbps: 6, lossPct: 0 } }));
    await waitFor(() => expect(stats()).toHaveAttribute('data-quality', 'poor'));
    act(() => app.session.update({ link: { rttMs: 40, upKbps: 5, downKbps: 6, lossPct: 6 } }));
    await waitFor(() => expect(stats()).toHaveAttribute('data-quality', 'bad'));
    expect(stats()).toHaveTextContent('Ping 40 milliseconds, bad.');
    act(() => app.session.update({ phase: 'reconnecting', link: undefined }));
    await waitFor(() => expect(dockEl().querySelector('.d-voice-dock__where')).toHaveTextContent(/^reconnecting$/));
  });

  it('leave is the dock’s one control, a 28px icon button (Signal §5.4, Ruling U)', async () => {
    await inVoice();
    await offStage();
    const buttons = within(screen.getByRole('toolbar', { name: 'voice controls' })).getAllByRole('button');
    expect(buttons).toHaveLength(1);
    for (const b of buttons) { expect(b).toHaveClass('d-icon-button'); expect(b).toHaveAttribute('data-size', '28'); }
  });

  it('a remote tile at volume 70 shows "70 %" in its chip', async () => {
    const app = await inVoice({ settings: { [`voice.volume.${BOB}`]: '70' } });
    act(() => app.session.update({ tiles: remote(false) }));
    await waitFor(() => expect(document.querySelector('.d-call-tile__chip')).toHaveTextContent('70 %'));
  });

  it('a speaking participant has the ring on the list row and on the tile and no other speaking mark', async () => {
    const app = await inVoice({ voice: { [GENV]: { callId: GENV, participants: [
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 0, own: false, web: false, bot: false },
      { userId: ME.id, deviceId: OWN_DEV, name: 'me', flags: 0, own: true, web: true, bot: false },
    ] } } });
    act(() => app.session.update({ tiles: remote(true) }));
    const tile = await waitFor(() => { const e = document.querySelector('.d-call-tile[data-speaking="true"]'); expect(e).not.toBeNull(); return e!; });
    expect(tile.querySelector('.d-call-tile__avatar-ring')).toHaveAttribute('data-speaking', 'true');
    expect(tile.querySelector('[data-state="speaking"]')).toBeNull();
    await waitFor(() => expect(document.querySelector('.d-voice-row__person[data-speaking="true"]')).not.toBeNull());
    expect(document.querySelector('[data-state="speaking"]')).toBeNull();
    await offStage();
    expect(dockEl().querySelector('[data-speaking], [data-state="speaking"]')).toBeNull();
  });

  it('the hearing count stands in the call bar\'s stats, not in the dock; no push-to-talk line (fix round 1)', async () => {
    const app = await inVoice({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'Pause' } });
    act(() => app.session.update({ audio: { attached: 1, expected: 2 } }));
    const stage = main().getByRole('toolbar', { name: 'voice channel controls' }).closest('.dw-voice-stage')!;
    await waitFor(() => expect(stage.querySelector('.d-call-stage__stats [data-audio-expected]')).toHaveTextContent(t('voice.diag.tracks', { attached: 1, expected: 2 })));
    expect(document.querySelector('.dw-voice-dock__mode')).toBeNull();
    expect(dockEl().querySelector('[data-audio-expected]')).toBeNull();
  });
});

describe('fix round 1 (Jonas): the sidebar\'s person menu and the voice row\'s call code', () => {
  it('a right-click on a voice occupant opens the person menu; the voice row\'s menu holds the call code in a call', async () => {
    const app = await inVoice({ voice: { [GENV]: { callId: GENV, participants: [
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 0, own: false, web: false, bot: false }] } } });
    const person = document.querySelector<HTMLElement>('.d-voice-row__person')!;
    fireEvent.contextMenu(person, { clientX: 20, clientY: 20 });
    const menu = await screen.findByRole('menu', { name: t('callStage.menu.label', { name: 'bob' }) });
    expect(within(menu).getByRole('menuitemcheckbox', { name: t('callStage.menu.muteForMe') })).toHaveAttribute('aria-checked', 'false');
    await app.user.click(within(menu).getByRole('menuitemcheckbox', { name: t('callStage.menu.muteForMe') }));
    expect(app.client.callsOf('setSetting')).toContainEqual({ m: 'setSetting', key: `voice.volume.${BOB}`, value: '0' });
    await app.user.keyboard('{Escape}');
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV, code: '123451234512345123451234512345' }));
    fireEvent.contextMenu(document.querySelector<HTMLElement>('.d-voice-item')!, { clientX: 20, clientY: 20 });
    await app.user.click(await screen.findByRole('menuitem', { name: t('voice.code') }));
    expect(await screen.findByText('12345 12345 12345 12345 12345 12345')).toBeInTheDocument();
  });
});

describe('fix round 1 (Jonas): no sound-source picker for a share', () => {
  it('a share without the browser\'s own sound offers no select and no pick: the default source stands', async () => {
    const app = await inVoice();
    app.session.sharePool = { labeled: true, sources: [{ id: 'mic2', label: 'USB mic', monitor: false }] };
    act(() => app.session.update({ sharing: true, shareAudio: 'none', localScreen: new MediaStream() }));
    await waitFor(() => expect(main().getByRole('button', { name: t('callStage.bar.stopShare') })).toBeInTheDocument());
    expect(main().queryByRole('combobox')).toBeNull();
    expect(document.querySelector('.d-call-stage__notice-action')).toBeNull();
  });
});
