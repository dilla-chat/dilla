import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, screen, waitFor, within } from '@testing-library/react';
import { A, callState, DM, GENV, renderVoiceShell, ring, TEXT, type VoiceHarnessOptions } from '../../test/fake-client.ts';

afterEach(() => { vi.unstubAllGlobals(); });

const main = () => within(screen.getByRole('main'));

/** In voice 'general': the join answered and the worker reports the call in. */
async function inVoice(o: VoiceHarnessOptions = {}) {
  const app = renderVoiceShell(o);
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
  return app;
}

describe('rings (flow 06, R1–R8)', () => {
  it('Do Not Disturb stops ringing and notifications while keeping the in-app banner', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted' });
    app.client.publish('rings', [ring()]);
    expect(await screen.findByRole('alert')).toHaveTextContent('bob is calling');
    expect(app.sounds.ringing).toBe(true);
    expect(app.notifications).toHaveLength(1);
    app.client.publish('account', { ...app.client.get('account')!, presenceChoice: 'dnd' });
    expect(app.sounds.ringing).toBe(false);
    expect(app.notifications[0].closed).toBe(true);
    expect(screen.getByRole('alert')).toHaveTextContent('bob is calling');
  });
  it('an incoming ring shows the banner, plays the ring and raises one notification; state 0 stops all three', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted', settings: {} });
    app.client.publish('rings', [ring({ busy: false, quiet: false })]);
    expect(await screen.findByRole('alert')).toHaveTextContent('bob is calling');
    expect(app.sounds.ringing).toBe(true);
    expect(app.notifications).toHaveLength(1);
    expect(app.notifications[0].title).toBe('bob is calling');
    expect(app.notifications[0].options).toMatchObject({ body: 'Open dilla to answer.', tag: `dilla-ring-${DM}` });
    expect(document.querySelector('[data-doc-state="call-ring"]')).not.toBeNull();
    // The same ring republished raises nothing new.
    app.client.publish('rings', [ring({ busy: false, quiet: false })]);
    expect(app.notifications).toHaveLength(1);
    app.client.publish('rings', []);
    expect(screen.queryByRole('alert')).toBeNull();
    expect(app.sounds.ringing).toBe(false);
    expect(app.notifications[0].closed).toBe(true);
  });

  it('a ring in a muted server plays nothing; unmuted it rings (useSounds wiring)', async () => {
    const muted = renderVoiceShell({ settings: { [`mute.community.${A}`]: '1' } });
    muted.client.publish('rings', [ring({ channelId: TEXT })]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(muted.sounds.ringing).toBe(false);
    cleanup();
    const open = renderVoiceShell({ settings: {} });
    open.client.publish('rings', [ring({ channelId: TEXT })]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(open.sounds.ringing).toBe(true);
  });

  it('a busy ring shows the banner with the busy text but plays nothing and notifies nobody', async () => {
    const app = await inVoice({ hidden: true, permission: 'granted' });
    app.client.publish('rings', [ring({ busy: true })]);
    expect(await screen.findByRole('alert')).toHaveTextContent('bob is calling. Answering leaves general.');
    expect(app.sounds.ringing).toBe(false);
    expect(app.notifications).toEqual([]);
  });

  it('a muted DM rings with the banner only (R1)', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted' });
    app.client.publish('rings', [ring({ quiet: true })]);
    expect(await screen.findByRole('alert')).toHaveTextContent('bob is calling');
    expect(app.sounds.ringing).toBe(false);
    expect(app.notifications).toEqual([]);
    expect(app.sounds.played).toEqual([]);
  });

  it('sound.ring off: banner and notification, no ring sound', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted', settings: { 'sound.ring': 'off' } });
    app.client.publish('rings', [ring()]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(app.notifications).toHaveLength(1);
    expect(app.sounds.ringing).toBe(false);
  });

  it('a DM whose notify mode is nothing rings without a notification', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted', settings: { [`notify.channel.${DM}`]: 'nothing' } });
    app.client.publish('rings', [ring()]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(app.notifications).toEqual([]);
    expect(app.sounds.ringing).toBe(true);
  });

  it('notification permission denied or default: banner and sound only, never a permission prompt from a ring', async () => {
    for (const permission of ['denied', 'default'] as const) {
      const app = renderVoiceShell({ hidden: true, permission });
      app.client.publish('rings', [ring()]);
      expect(await screen.findByRole('alert')).toBeInTheDocument();
      expect(app.sounds.ringing).toBe(true);
      expect(app.notifications).toEqual([]);
      expect(app.requestPermission).not.toHaveBeenCalled();
      app.view.unmount();
      vi.unstubAllGlobals();
    }
  });

  it('no ring sound before the page saw a user activation; the banner still shows', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted', activated: false });
    app.client.publish('rings', [ring()]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(app.sounds.ringing).toBe(false);
    expect(app.notifications).toHaveLength(1);
  });

  it('answer runs callJoin for the DM (leaving the current voice first when busy); decline runs callDecline', async () => {
    const app = await inVoice();
    app.client.publish('rings', [ring({ busy: true })]);
    await screen.findByRole('alert');
    // While it shows, the banner's buttons are the first controls of the main landmark.
    const buttons = main().getAllByRole('button');
    expect(buttons[0]).toHaveTextContent('answer');
    expect(buttons[1]).toHaveTextContent('decline');
    const before = app.client.calls.length;
    await app.user.click(main().getByRole('button', { name: 'answer' }));
    await waitFor(() => expect(app.client.callsOf('callJoin').map(c => c.channelId)).toEqual([GENV, DM]));
    const after = app.client.calls.slice(before).filter(c => c.m === 'callLeave' || c.m === 'callJoin');
    expect(after).toEqual([{ m: 'callLeave', reason: 'switch' }, { m: 'callJoin', channelId: DM, vdec: 'vp8' }]);
    await waitFor(() => expect(window.location.pathname).toBe(`/dm/${DM}`));
    app.client.publish('rings', []);
    app.client.publish('call', callState({ phase: 'in', channelId: DM, callId: DM }));
    await waitFor(() => expect(main().getByRole('toolbar', { name: 'call controls' }).querySelector('button')).toHaveFocus());
    // A new ring is declined.
    const next = ring({ callId: 'ab'.repeat(16), channelId: 'ab'.repeat(16), ringTs: 1_900_000_100, callerName: 'carol' });
    app.client.publish('rings', [next]);
    await screen.findByRole('alert');
    expect(main().getAllByRole('button')[0]).toHaveTextContent('answer');
    await app.user.click(main().getByRole('button', { name: 'decline' }));
    expect(app.client.calls.at(-1)).toEqual({ m: 'callDecline', callId: 'ab'.repeat(16), ringTs: 1_900_000_100 });
  });

  it('a ring never moves focus', async () => {
    const app = renderVoiceShell({ path: `/dm/${DM}` });
    const composer = await screen.findByRole('textbox', { name: 'message @bob' });
    composer.focus();
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    expect(composer).toHaveFocus();
  });

  it('clicking the notification focuses the window, opens the DM and closes it', async () => {
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => undefined);
    const app = renderVoiceShell({ hidden: true, permission: 'granted' });
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    app.notifications[0].onclick?.(new Event('click'));
    expect(focus).toHaveBeenCalled();
    await waitFor(() => expect(window.location.pathname).toBe(`/dm/${DM}`));
    expect(app.notifications[0].closed).toBe(true);
  });

  // Ruling (task 9): flow 06 wins, the desktop notification shows only while the tab is hidden; the banner covers a visible tab.
  it('a visible tab rings with the banner and the sound, and notifies only once it is hidden; showing it again closes the notification', async () => {
    const app = renderVoiceShell({ permission: 'granted' });
    app.client.publish('rings', [ring()]);
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(app.sounds.ringing).toBe(true);
    expect(app.notifications).toEqual([]);
    app.setHidden(true);
    expect(app.notifications).toHaveLength(1);
    app.setHidden(false);
    expect(app.notifications[0].closed).toBe(true);
  });

  it('closes its notifications when the shell goes (m5)', async () => {
    const app = renderVoiceShell({ hidden: true, permission: 'granted' });
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    app.view.unmount();
    expect(app.notifications[0].closed).toBe(true);
    expect(app.sounds.ringing).toBe(false);
  });

  it('no ring sound for a busy ring outside a call, nor for a ring while this device is in a call (m5)', async () => {
    const app = renderVoiceShell();
    app.client.publish('rings', [ring({ busy: true })]);
    await screen.findByRole('alert');
    expect(app.sounds.ringing).toBe(false);
    app.view.unmount();
    const inCall = await inVoice();
    inCall.client.publish('rings', [ring({ busy: false })]);
    await screen.findByRole('alert');
    expect(inCall.sounds.ringing).toBe(false);
  });

  it('muting the DM while it rings stops the ring sound (m3)', async () => {
    const app = renderVoiceShell();
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    expect(app.sounds.ringing).toBe(true);
    app.client.publish('settings', { [`mute.channel.${DM}`]: '1' });
    expect(app.sounds.ringing).toBe(false);
  });

  it('a ring that ends while focus is on its buttons hands focus to the main landmark (I5)', async () => {
    const app = renderVoiceShell();
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    main().getByRole('button', { name: 'decline' }).focus();
    await app.user.keyboard('{Enter}');
    expect(app.client.calls.at(-1)).toMatchObject({ m: 'callDecline' });
    app.client.publish('rings', []);
    expect(screen.getByRole('main')).toHaveFocus();
    // A ring that ends while focus is elsewhere leaves focus alone.
    app.client.publish('rings', [ring()]);
    await screen.findByRole('alert');
    const join = main().getByRole('button', { name: 'Join voice' });
    join.focus();
    app.client.publish('rings', []);
    expect(join).toHaveFocus();
  });
});
