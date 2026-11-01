// The whole-branch fix wave of web-3 (wb-security.md, wb-client.md, wb-web-a11y.md): the shell's side.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { startDmCall } from '../../test/row-menu.ts';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { t } from '../../strings/index.ts';
import { A, BOB, callState, DM, GENV, ring, renderVoiceShell, TEXT, type VoiceHarnessOptions } from '../../test/fake-client.ts';

afterEach(() => { vi.unstubAllGlobals(); });

const main = () => within(screen.getByRole('main'));
const UMK = { code: 'E_UMK_CHANGED', detail: '', status: 0, retryAfterMs: null };
const STALE = { ...UMK, code: 'E_DEVICE_LIST_STALE' };

async function inVoice(o: VoiceHarnessOptions = {}) {
  const app = renderVoiceShell(o);
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
  return app;
}

describe('the hold after a refused rekey (wb-security I1)', () => {
  it('pauses the session, drops its media keys, says voice.held, and resumes on the next key', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: STALE, changedUser: null }));
    expect(await main().findByText(t('voice.held'))).toBeInTheDocument();
    await waitFor(() => expect(app.session.setPaused).toHaveBeenCalledWith(true));
    expect(app.session.dropKeys).toHaveBeenCalledTimes(1);
    expect(main().queryByRole('button', { name: 'compare safety numbers' })).toBeNull();
    // The same hold published again drops nothing more.
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: STALE, changedUser: null }));
    expect(app.session.dropKeys).toHaveBeenCalledTimes(1);
    app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
    await waitFor(() => expect(app.session.setPaused).toHaveBeenLastCalledWith(false));
    expect(screen.queryByText(t('voice.held'))).toBeNull();
  });

  it('an identity pause keeps the keys (it may resume on the same epoch); a hold after it drops them', async () => {
    const app = await inVoice();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: UMK, changedUser: BOB }));
    await waitFor(() => expect(app.session.setPaused).toHaveBeenCalledWith(true));
    expect(app.session.dropKeys).not.toHaveBeenCalled();
    app.client.publish('call', callState({ phase: 'paused', channelId: GENV, callId: GENV, error: { ...UMK, code: 'E_CORE_STATE' }, changedUser: null }));
    await waitFor(() => expect(app.session.dropKeys).toHaveBeenCalledTimes(1));
    expect(app.session.setPaused).toHaveBeenCalledTimes(1);
  });
});

const lasting = () => document.querySelector('.dw-voice-lasting');
const dockStatus = () => document.querySelector('.d-voice-dock [role="status"]');
const dockEl = () => document.querySelector<HTMLElement>('.dw-voice-dock');
const failingStart = (code: string) => vi.fn(async (deps: { bridge: { call: (c: unknown) => Promise<unknown> } }, target: { channelId: string; vdec: string }) => {
  await deps.bridge.call({ m: 'callJoin', channelId: target.channelId, vdec: target.vdec });
  throw Object.assign(new Error(code), { code });
});
const toLobby = async () => {
  window.history.pushState(null, '', `/c/${A}/${TEXT}`);
  window.dispatchEvent(new PopStateEvent('popstate'));
  await waitFor(() => expect(screen.queryByRole('toolbar', { name: 'voice channel controls' })).toBeNull());
};

describe('a join can be cancelled (wb-client I1)', () => {
  it('leave during the start cancels it: the worker is told, the shell returns to idle, and no notice says it failed', async () => {
    const app = renderVoiceShell();
    app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.log).toContain(`callJoin:${GENV}`));
    await waitFor(() => expect(dockEl()).not.toBeNull());
    // The stage has the controls while it is on screen; the dock has them off it (web3.7).
    await toLobby();
    await app.user.click(within(dockEl()!).getByRole('button', { name: 'leave' }));
    await waitFor(() => expect(app.log).toContain(`aborted:${GENV}`));
    expect(app.client.callsOf('callLeave').length).toBeGreaterThan(0);
    await waitFor(() => expect(dockEl()).toBeNull());
    window.history.pushState(null, '', `/c/${A}/${GENV}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
    expect(await main().findByRole('button', { name: 'Join voice' })).toBeInTheDocument();
    expect(screen.queryByText(t('voice.failed'))).toBeNull();
    expect(app.sessions).toHaveLength(0);
  });

  it('answering a ring while a join is in flight cancels that join and starts the DM call', async () => {
    const app = renderVoiceShell();
    app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.log).toContain(`callJoin:${GENV}`));
    app.client.publish('rings', [ring()]);
    await app.user.click(await main().findByRole('button', { name: 'answer' }));
    await waitFor(() => expect(app.log).toContain(`aborted:${GENV}`));
    await waitFor(() => expect(app.log).toContain(`callJoin:${DM}`));
    expect(app.log.indexOf(`aborted:${GENV}`)).toBeLessThan(app.log.indexOf(`callJoin:${DM}`));
  });
});

describe('a call that ends on its own says why (wb-client I3)', () => {
  it('a lost connection on the stage offers automatic rejoin and a way to leave', async () => {
    const app = await inVoice();
    act(() => app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' }));
    await waitFor(() => expect(screen.getByText(t('update.rejoiningVoice'))).toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'leave' })).toBeInTheDocument();
  });

  it('a call ended for everyone off the stage is announced in the region that outlives the dock', async () => {
    const app = await inVoice();
    await toLobby();
    act(() => app.session.update({ phase: 'ended', error: 'E_CALL_ENDED' }));
    await waitFor(() => expect(lasting()).toHaveTextContent(t('voice.ended')));
    expect(dockEl()).toBeNull();
  });
});

describe('leave and join at once (wb-client m1)', () => {
  it('a join right after a leave waits for that leave instead of failing as a call in progress', async () => {
    const app = await inVoice();
    let release: () => void = () => undefined;
    app.session.leaveGate = new Promise<void>(resolve => { release = resolve; });
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    expect(app.client.callsOf('callJoin')).toHaveLength(1);
    release();
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(2));
    expect(app.log.indexOf('left:user')).toBeLessThan(app.log.lastIndexOf(`callJoin:${GENV}`));
  });
});

describe('focus and announcements across the swaps (wb-web-a11y I1-I4)', () => {
  it('I1: the join moves focus to the first stage control at once, before the session attaches', async () => {
    const app = renderVoiceShell();
    app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(main().getByRole('toolbar', { name: 'voice channel controls' }).querySelector('button')).toHaveFocus());
    expect(app.sessions).toHaveLength(0);
  });

  it('I1: a removal while focus is on a stage control moves focus to the notice, never the page', async () => {
    const app = await inVoice();
    main().getByRole('button', { name: 'Deafen' }).focus();
    app.client.publish('call', callState({ phase: 'removed', channelId: GENV, callId: GENV }));
    await waitFor(() => expect(document.activeElement).toHaveTextContent(t('voice.removed')), { timeout: 3_500 });
    expect(document.activeElement).not.toBe(document.body);
  });

  it('I1: a failed join while focus is on a stage control moves focus to the notice', async () => {
    const app = renderVoiceShell();
    app.module.startPageCall = failingStart('E_CALL_FULL') as unknown as typeof app.module.startPageCall;
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(document.activeElement).toHaveTextContent(t('voice.full')));
  });

  it('I2: the DM strip region exists before its notice, so a refusal is announced', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    await screen.findByRole('navigation', { name: t('shell.dms.label') });
    const region = document.querySelector('.dw-call-strip [role="status"]');
    expect(region).not.toBeNull();
    expect(region).toHaveTextContent('');
    app.module.startPageCall = failingStart('E_CALL_FULL') as unknown as typeof app.module.startPageCall;
    // Ruling Z: the DM call is in the DM row's context menu.
    await startDmCall(app.user, 'bob');
    await waitFor(() => expect(region).toHaveTextContent(t('voice.full')));
    expect(region?.isConnected).toBe(true);
  });

  it('I3: joining the same channel twice says "You joined general." again; leaving twice says "You left general." again', async () => {
    const app = await inVoice();
    await waitFor(() => expect(dockStatus()).toHaveTextContent('You joined general.'));
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    await waitFor(() => expect(lasting()).toHaveTextContent('You left general.'));
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.sessions).toHaveLength(2));
    // The second join's regions start empty and then say it: a real change for a screen reader.
    expect(lasting()).toHaveTextContent('');
    await waitFor(() => expect(dockStatus()).toHaveTextContent('You joined general.'));
    await app.user.click(main().getByRole('button', { name: 'Leave' }));
    await waitFor(() => expect(lasting()).toHaveTextContent('You left general.'));
  });

  it('I4: the user panel toggle keeps its label with aria-pressed; the stage’s call bar says it by its word (Signal §5.6)', async () => {
    const app = await inVoice();
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    // The call bar's word carries the state (the prototype's Mute / Unmute), so it has no aria-pressed (WCAG 2.5.3, 4.1.2).
    expect(main().getByRole('button', { name: 'Unmute' })).not.toHaveAttribute('aria-pressed');
    await toLobby();
    expect(within(screen.getByRole('toolbar', { name: 'your account' })).getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.queryByRole('button', { name: 'unmute' })).toBeNull();
  });
});

describe('keyboard, rings and the voice section (wb-web-a11y m1, m3, m4, m5)', () => {
  it('m1: the mute shortcut follows the letter typed (AZERTY: the M key is code Semicolon)', async () => {
    const app = await inVoice();
    fireEvent.keyDown(window, { key: 'M', code: 'Semicolon', ctrlKey: true, shiftKey: true });
    expect(app.session.muted).toBe(true);
    // The physical KeyM typing another letter (Dvorak) does not toggle it.
    fireEvent.keyDown(window, { key: 'B', code: 'KeyM', ctrlKey: true, shiftKey: true });
    expect(app.session.muted).toBe(true);
    fireEvent.keyDown(window, { key: 'm', code: 'KeyM', metaKey: true, shiftKey: true });
    expect(app.session.muted).toBe(false);
  });

  it('m3: declining one of two rings keeps focus in the banner, on the other ring', async () => {
    const app = renderVoiceShell();
    const carol = ring({ callId: 'ab'.repeat(16), channelId: 'ab'.repeat(16), callerName: 'carol' });
    app.client.publish('rings', [ring(), carol]);
    const declines = await main().findAllByRole('button', { name: 'decline' });
    await app.user.click(declines[0]);
    app.client.publish('rings', [carol]);
    await waitFor(() => expect(main().getByRole('button', { name: 'answer' })).toHaveFocus());
  });

  it('m4: the voice row join is an icon button named join voice, in the trailing slot', async () => {
    renderVoiceShell({ path: `/c/${A}/${TEXT}` });
    const row = (await screen.findByRole('button', { name: 'voice music' })).closest('li') as HTMLElement;
    const join = within(row).getByRole('button', { name: 'join voice' });
    // web3.9: the join is an icon button in the row's trailing slot, named by its own label.
    expect(join).toHaveAttribute('aria-label', 'join voice');
    expect(join.closest('.d-slot')).not.toBeNull();
  });

  it('m5: leaving from the dock on another DM focuses the @ tile, not that DM\'s row', async () => {
    const app = await inVoice();
    window.history.pushState(null, '', `/dm/${DM}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
    await screen.findByRole('navigation', { name: t('shell.dms.label') });
    await app.user.click(within(screen.getByRole('toolbar', { name: 'voice controls' })).getByRole('button', { name: 'leave' }));
    await waitFor(() => expect(document.activeElement).toHaveAccessibleName('direct messages'));
  });
});
