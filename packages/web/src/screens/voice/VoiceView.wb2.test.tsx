// The second whole-branch fix wave of web-3 (review/wb-product-walk.md): the shell's side.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { dmMenuItems, startDmCall } from '../../test/row-menu.ts';
import { act, screen, waitFor, within } from '@testing-library/react';
import { t } from '../../strings/index.ts';
import { A, MUSIC, BOB, BOB_DEV, bobChanged, callState, DM, GENV, OWN_DEV, renderVoiceShell } from '../../test/fake-client.ts';
import { ME } from '../../test/fixtures.ts';
import { RING_MS, UNANSWERED_LEAVE_MS } from './useCall.tsx';

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); Reflect.deleteProperty(document, 'fullscreenEnabled'); });

const main = () => within(screen.getByRole('main'));
const dockStatus = () => document.querySelector('.d-voice-dock [role="status"]');

/** Starts a DM call with bob from the DM's strip; the worker then reports it in with `outgoing`. */
async function calling(outgoing: { ringing: string[]; declined: string[]; notRung: string[] }) {
  const app = renderVoiceShell({ path: `/dm/${DM}` });
  await startDmCall(app.user, 'bob');
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: DM, callId: DM, outgoing }));
  return app;
}

describe('the outgoing ring says what the instance did (product walk I6)', () => {
  it('a participant the instance did not ring is said as not rung, never as ringing, and no answer waits for the ring time', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const app = await calling({ ringing: [], declined: [], notRung: ['bob'] });
    await waitFor(() => expect(dockStatus()).toHaveTextContent(t('call.outgoing.notRung', { names: 'bob' })));
    expect(dockStatus()).not.toHaveTextContent('ringing bob');
    expect(dockStatus()).not.toHaveTextContent(t('call.outgoing.noAnswer'));
    // bob can still join from the DM's call button during the ring time; then nobody did.
    await act(() => vi.advanceTimersByTimeAsync(RING_MS));
    await waitFor(() => expect(dockStatus()).toHaveTextContent(t('call.outgoing.noAnswer')));
    expect(app.session.leave).not.toHaveBeenCalled();
  });

  it('after no answer the caller leaves by itself, and the strip says why', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const app = await calling({ ringing: [], declined: ['bob'], notRung: [] });
    await waitFor(() => expect(dockStatus()).toHaveTextContent('bob declined.'));
    await act(() => vi.advanceTimersByTimeAsync(UNANSWERED_LEAVE_MS - 100));
    expect(app.session.leave).not.toHaveBeenCalled();
    await act(() => vi.advanceTimersByTimeAsync(200));
    await waitFor(() => expect(app.session.leave).toHaveBeenCalledWith('user'));
    const strip = document.querySelector('.dw-call-strip__notice');
    await waitFor(() => expect(strip).toHaveTextContent('bob declined.'));
    expect(await dmMenuItems(app.user, 'bob')).toContain('call');
  });

  it('a call someone answered never ends by itself when they leave again', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const app = await calling({ ringing: ['bob'], declined: [], notRung: [] });
    act(() => app.session.update({ tiles: [{ deviceId: 'b1', userId: 'bob', state: 'ok', speaking: false, video: null, screen: null, screenAudio: false }] }));
    app.client.publish('call', callState({ phase: 'in', channelId: DM, callId: DM, outgoing: { ringing: [], declined: [], notRung: [] } }));
    act(() => app.session.update({ tiles: [] }));
    await act(() => vi.advanceTimersByTimeAsync(RING_MS + UNANSWERED_LEAVE_MS + 1000));
    expect(app.session.leave).not.toHaveBeenCalled();
  });
});

// jsdom has no MediaStream: a bare stand-in is enough, a tile only hands it to a <video>'s srcObject.
if (typeof globalThis.MediaStream === 'undefined') {
  (globalThis as { MediaStream?: unknown }).MediaStream = class MediaStream {};
}

async function inHarbour(o: Parameters<typeof renderVoiceShell>[0] = {}) {
  const app = renderVoiceShell({
    voice: { [GENV]: { callId: GENV, participants: [
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 0, own: false, web: false, bot: false },
      { userId: ME.id, deviceId: OWN_DEV, name: 'me', flags: 0, own: true, web: true, bot: false },
    ] } },
    ...o,
  });
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
  return app;
}
const tileOf = (name: string) => main().getAllByRole('listitem').find(li => within(li).queryByText(name, { exact: true }) !== null)!;

describe('the stage shows what is sent and shared (product walk I2, I3, M4, M13)', () => {
  it('I2: a person with camera and screen on gives a tile for each, with no inset (web3.8 R27)', async () => {
    const app = await inHarbour();
    const screenStream = new MediaStream();
    const cam = new MediaStream();
    act(() => app.session.update({ tiles: [{ deviceId: BOB_DEV, userId: BOB, state: 'ok', speaking: false, video: cam, screen: screenStream, screenAudio: false }] }));
    const video = (id: string) => document.querySelector<HTMLVideoElement>(`li[data-tile-id="${BOB_DEV}:${id}"] video`);
    await waitFor(() => { expect(video('screen')).not.toBeNull(); expect(video('person')).not.toBeNull(); });
    expect(video('screen')!.srcObject).toBe(screenStream);
    expect(video('person')!.srcObject).toBe(cam);
    expect(tileOf('bob').querySelectorAll('video')).toHaveLength(1);
  });

  it('I3: your own camera tile is mirrored, and your screen has a tile of its own, not mirrored', async () => {
    const app = await inHarbour();
    const cam = new MediaStream();
    act(() => app.session.update({ camera: true, localCamera: cam }));
    const own = await waitFor(() => { const t = document.querySelector('.d-call-tile[data-own="true"]')!; expect(t.querySelector('video')).not.toBeNull(); return t; });
    expect(own.querySelector('video')!.srcObject).toBe(cam);
    expect(own.querySelector('video')).toHaveAttribute('data-mirror', 'true');
    const screenStream = new MediaStream();
    act(() => app.session.update({ sharing: true, localScreen: screenStream }));
    const screenTile = await waitFor(() => {
      const t = document.querySelector('.d-call-tile[data-own="true"][data-source="screen"]')!;
      expect(t.querySelector('video')).not.toBeNull();
      return t;
    });
    expect(screenTile.querySelector('video')!.srcObject).toBe(screenStream);
    expect(screenTile.querySelector('video')).not.toHaveAttribute('data-mirror');
    expect(own.querySelector('video')).toHaveAttribute('data-mirror', 'true');
  });

  it('web3.8 WS7: the DM call box stays a compact grid: no stage mode, and a share takes no focus (it would collapse to 2 px)', async () => {
    Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: true });
    const app = await calling({ ringing: [], declined: [], notRung: [] });
    act(() => app.session.update({ tiles: [{ deviceId: BOB_DEV, userId: BOB, state: 'ok', speaking: false, video: null, screen: new MediaStream(), screenAudio: false }] }));
    await waitFor(() => expect(document.querySelector(`li[data-tile-id="${BOB_DEV}:screen"]`)).not.toBeNull());
    expect(document.querySelector('[data-tiles="stage"]')).toBeNull();
    expect(document.querySelector('[data-focused-stage]')).toBeNull();
    expect(document.querySelector('.d-call-tile__focus')).toBeNull();
    expect(document.querySelector('.d-call-tile__fullscreen')).toBeNull(); // positioned for stage tiles only
    expect(document.querySelector('li[data-focused="true"]')).toBeNull();
  });

  it('M4: your own tile says speaking when the SFU hears you, whatever the meter level', async () => {
    const app = await inHarbour();
    act(() => app.session.update({ speaking: true, level: 1 }));
    await waitFor(() => expect(document.querySelector('.d-call-tile[data-own="true"]')).toHaveAttribute('data-speaking', 'true'));
  });

  it('M13: no tile shows the client tag, yours included', async () => {
    const app = await inHarbour();
    act(() => app.session.update({ tiles: [{ deviceId: BOB_DEV, userId: BOB, state: 'ok', speaking: false, video: null, screen: null, screenAudio: false }] }));
    await waitFor(() => expect(main().getAllByRole('listitem')).toHaveLength(2));
    expect(main().queryByText(t('voice.tag.web'), { exact: true })).toBeNull();
  });

  it('M13: the people of a call you are not in carry no client tag either (the worker marks only this device as web)', async () => {
    renderVoiceShell({ voice: { [GENV]: { callId: GENV, participants: [
      { userId: BOB, deviceId: BOB_DEV, name: 'bob', flags: 0, own: false, web: false, bot: false },
      { userId: ME.id, deviceId: OWN_DEV, name: 'me', flags: 0, own: true, web: true, bot: false },
    ] } } });
    await main().findByRole('button', { name: 'Join voice' });
    // Signal §5.6: the not-joined preview shows the people as faces, with no tag of any kind.
    await waitFor(() => expect(document.querySelectorAll('.d-call-stage__face')).toHaveLength(2));
    expect(document.querySelector('.d-call-stage__preview .d-call-tile__tag')).toBeNull();
    expect(main().queryByText(t('voice.tag.web'), { exact: true })).toBeNull();
  });

  it('M3: the sidebar row of a person says speaking while the call hears them', async () => {
    const app = await inHarbour();
    const sidebar = () => within(document.querySelector('.dw-sidebar') as HTMLElement);
    expect(sidebar().queryByRole('img', { name: t('voice.state.speaking') })).toBeNull();
    act(() => app.session.update({ tiles: [{ deviceId: BOB_DEV, userId: BOB, state: 'ok', speaking: true, video: null, screen: null, screenAudio: false }] }));
    // web3.9: the ring is the mark (no glyph); the word stays for assistive technology.
    await waitFor(() => expect(sidebar().getByText(t('voice.state.speaking'))).toBeInTheDocument());
    expect(document.querySelector('.dw-sidebar .d-voice-row__person[data-speaking="true"]')).not.toBeNull();
    expect(sidebar().queryByRole('img', { name: t('voice.state.speaking') })).toBeNull();
  });
});

describe('Settings → Voice follows the call (product walk M1)', () => {
  it('switching noise suppression during a call applies it at once', async () => {
    const app = await inHarbour();
    app.client.publish('settings', { 'voice.noise': 'off' });
    await waitFor(() => expect(app.session.setNoise).toHaveBeenCalledWith('off'));
    app.client.publish('settings', { 'voice.noise': 'on' });
    await waitFor(() => expect(app.session.setNoise).toHaveBeenLastCalledWith('on'));
  });

  it('V1: the output device follows Settings during a call, and only a change moves it', async () => {
    const app = await inHarbour({ settings: { 'voice.output': 'spk' } });
    // The call started on 'spk' (startPageCall's output): nothing to move at the join.
    expect(app.session.setOutput).not.toHaveBeenCalled();
    app.client.publish('settings', { 'voice.output': 'headset' });
    await waitFor(() => expect(app.session.setOutput).toHaveBeenCalledWith('headset'));
    // Back to the system default.
    app.client.publish('settings', {});
    await waitFor(() => expect(app.session.setOutput).toHaveBeenLastCalledWith(''));
    expect(app.session.setOutput).toHaveBeenCalledTimes(2);
  });

  it('V1: a refused output device is said in the dock, and the call goes on', async () => {
    const app = await inHarbour();
    app.session.setOutput.mockRejectedValueOnce(new Error('NotFoundError'));
    app.client.publish('settings', { 'voice.output': 'gone' });
    await waitFor(() => expect(dockStatus()).toHaveTextContent(t('voice.outputFailed')));
    expect(app.session.leave).not.toHaveBeenCalled();
  });

  it('V1 fix 1: a refusal that arrives after the person chose another device again is not said (A → B → A)', async () => {
    const app = await inHarbour({ settings: { 'voice.output': 'spk' } });
    let refuse: (e: Error) => void = () => undefined;
    app.session.setOutput.mockImplementationOnce(() => new Promise<void>((_ok, no) => { refuse = no; }));
    app.client.publish('settings', { 'voice.output': 'headset' });
    await waitFor(() => expect(app.session.setOutput).toHaveBeenCalledWith('headset'));
    app.client.publish('settings', { 'voice.output': 'spk' });
    await waitFor(() => expect(app.session.setOutput).toHaveBeenLastCalledWith('spk'));
    await act(async () => { refuse(new Error('NotFoundError')); await Promise.resolve(); });
    await act(() => new Promise(r => setTimeout(r, 150)));
    expect(dockStatus()).not.toHaveTextContent(t('voice.outputFailed'));
  });

  it('V1 fix 1: after a refusal the device in force is the one before, so going back moves nothing and the refused one is tried again', async () => {
    const app = await inHarbour({ settings: { 'voice.output': 'spk' } });
    app.session.setOutput.mockRejectedValueOnce(new Error('NotFoundError'));
    app.client.publish('settings', { 'voice.output': 'headset' });
    await waitFor(() => expect(dockStatus()).toHaveTextContent(t('voice.outputFailed')));
    // 'spk' still plays: choosing it is no move at all.
    app.client.publish('settings', { 'voice.output': 'spk' });
    await act(() => new Promise(r => setTimeout(r, 50)));
    expect(app.session.setOutput).toHaveBeenCalledTimes(1);
    // Choosing the refused device again tries it again.
    app.client.publish('settings', { 'voice.output': 'headset' });
    await waitFor(() => expect(app.session.setOutput).toHaveBeenCalledTimes(2));
    expect(app.session.setOutput).toHaveBeenLastCalledWith('headset');
  });

  it('V1 fix 1: an output chosen while the call is still starting is applied once the call is up', async () => {
    const app = renderVoiceShell({ settings: { 'voice.output': 'spk' } });
    const release = app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.module.startPageCall).toHaveBeenCalledTimes(1));
    app.client.publish('settings', { 'voice.output': 'headset' });
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.setOutput).toHaveBeenCalledWith('headset'));
    expect(app.session.setOutput).toHaveBeenCalledTimes(1);
  });
});

describe('the dock says where and how (product walk M7, M8)', () => {
  it('M7, fix round 1 (Jonas): push to talk has no line on the stage or in the dock', async () => {
    await inHarbour({ settings: { 'voice.mode': 'ptt', 'voice.pttKey': 'Pause' } });
    expect(document.querySelector('.dw-voice-dock__mode')).toBeNull();
  });

  it('M8: a DM call reads as the call with its person, in the dock and what is said', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await startDmCall(app.user, 'bob');
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    const where = t('call.where', { name: 'bob' });
    await waitFor(() => expect(document.querySelector('.d-voice-dock__text')).toHaveTextContent(where));
    expect(document.querySelector('.d-voice-dock__where')).toHaveTextContent('— ms');
    expect(dockStatus()).toHaveTextContent(t('voice.announce.joined', { where }));
    await app.user.click(main().getByRole('button', { name: 'leave' }));
    await waitFor(() => expect(document.querySelector('.dw-voice-lasting')).toHaveTextContent(t('voice.announce.left', { where })));
  });
});

describe('removed from the server while in voice (product walk I8)', () => {
  it('says so and offers no join that cannot work, on the stage and in the sidebar', async () => {
    const app = await inHarbour();
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
      error: { code: 'E_NOT_FOUND', detail: '', status: 404, retryAfterMs: null } }));
    expect(await main().findByText(t('voice.removedNotMember'))).toBeInTheDocument();
    expect(main().queryByText(t('voice.removed'))).toBeNull();
    expect(main().queryByRole('button', { name: 'Join voice' })).toBeNull();
    const row = [...document.querySelectorAll('.d-voice-item')].find(li => li.querySelector('[aria-label="voice general"]') !== null);
    expect(row).toBeDefined();
    expect(row!.querySelector('.d-voice-row__join')).toBeNull();
    // A removal from the call alone (the channel still visible) keeps the join.
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV }));
    expect(await main().findByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(row!.querySelector('.d-voice-row__join')).not.toBeNull();
  });
});

describe('mute is kept across a reload (product walk M5)', () => {
  it('mute and deafen are kept per browser, and the next call starts in them', async () => {
    const app = await inHarbour();
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await waitFor(() => expect(app.client.callsOf('setSetting')).toContainEqual({ m: 'setSetting', key: 'voice.self', value: 'muted' }));
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    await waitFor(() => expect(app.client.callsOf('setSetting')).toContainEqual({ m: 'setSetting', key: 'voice.self', value: 'deafened' }));
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await waitFor(() => expect(app.client.callsOf('setSetting').at(-1)).toEqual({ m: 'setSetting', key: 'voice.self', value: null }));
  });

  it('a call started after a reload starts muted when this browser was muted', async () => {
    const app = renderVoiceShell({ settings: { 'voice.self': 'muted' } });
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.module.startPageCall).toHaveBeenCalled());
    expect(vi.mocked(app.module.startPageCall).mock.calls[0][2]).toMatchObject({ self: 'muted' });
  });
});

describe('the identity refusals lead somewhere (product walk M9, M10)', () => {
  const UMK = { code: 'E_UMK_CHANGED', detail: '', status: 0, retryAfterMs: null };
  it('M9: a join refused for a changed identity offers the comparison, as the pause does', async () => {
    const app = renderVoiceShell({ trust: [bobChanged()], withSettings: true,
      call: callState({ phase: 'blocked', channelId: GENV, error: UMK, changedUser: BOB }) });
    expect(await main().findByText(t('voice.blockedChanged', { name: 'bob' }))).toBeInTheDocument();
    const action = main().getByRole('button', { name: t('voice.pausedAction') });
    await waitFor(() => expect(action).toHaveFocus());
    await app.user.click(action);
    await waitFor(() => expect(window.location.pathname).toBe('/settings/privacy'));
  });

  it('M10: a block that offers the join again never says joining is paused', async () => {
    const app = await inHarbour({ sendBlocks: {} });
    app.client.publish('call', callState({ phase: 'blocked', channelId: GENV, callId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' } }));
    expect(await main().findByText(t('voice.blockedLeft'))).toBeInTheDocument();
    expect(main().getByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(main().queryByText(/paused/)).toBeNull();
  });

  it('M10: a refused join that offers the join again never says joining is paused', async () => {
    renderVoiceShell({ sendBlocks: {}, call: callState({ phase: 'blocked', channelId: GENV, error: { ...UMK, code: 'E_DEVICE_UNLISTED' } }) });
    expect(await main().findByText(t('voice.blockedRetry'))).toBeInTheDocument();
    expect(main().getByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(main().queryByText(/paused/)).toBeNull();
  });
});

describe('the hold names the epoch it drops (re-review N-m1)', () => {
  it('the page drops only the keys up to the held epoch', async () => {
    const app = await inHarbour();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, epoch: '7',
      error: { code: 'E_DEVICE_LIST_STALE', detail: '', status: 0, retryAfterMs: null }, changedUser: null }));
    await waitFor(() => expect(app.session.dropKeys).toHaveBeenCalledWith(7n));
  });
});

describe('fix round 3 (wave 2 client review m5)', () => {
  it('off the stage, a removal from the server is said as such in the region that outlives the dock', async () => {
    const app = await inHarbour();
    await app.user.click(screen.getByRole('button', { name: /^lobby/ }));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull());
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV }));
    await waitFor(() => expect(document.querySelector('.dw-voice-lasting')).toHaveTextContent(t('voice.removed')), { timeout: 3_500 });
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
      error: { code: 'E_NOT_FOUND', detail: '', status: 404, retryAfterMs: null } }));
    await waitFor(() => expect(document.querySelector('.dw-voice-lasting')).toHaveTextContent(t('voice.removedNotMember')));
  });
  it('in a community with an AFK channel the hold changes the timing, not the words (WEB-AFK review I-2)', async () => {
    const app = await inHarbour();
    app.client.publish(`afk:${A}`, { channelId: MUSIC, timeoutSeconds: 300, pending: false });
    await app.user.click(screen.getByRole('button', { name: /^lobby/ }));
    await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull());
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV }));
    await new Promise(r => setTimeout(r, 200));
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
      error: { code: 'E_NOT_FOUND', detail: '', status: 404, retryAfterMs: null } }));
    await waitFor(() => expect(document.querySelector('.dw-voice-lasting')).toHaveTextContent(t('voice.removedNotMember')));
    // The hold ends: the line stays the server line.
    await new Promise(r => setTimeout(r, 2_400));
    expect(document.querySelector('.dw-voice-lasting')).toHaveTextContent(t('voice.removedNotMember'));
  });
});
