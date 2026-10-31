import { afterEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, screen, waitFor, within } from '@testing-library/react';
import { SETTINGS } from '@dilla/client-core';
import type { Command } from '@dilla/client-core';
import { PHONE_QUERY } from '../layout.ts';
import { t } from '../strings/index.ts';
import { A, callState, GENV, MEMBERS, renderVoiceShell, TEXT, type VoiceHarnessOptions } from '../test/fake-client.ts';
import { account, ME } from '../test/fixtures.ts';
import { pushHistoryState, replaceHistoryState } from '../router.ts';

// web3.8 WS6: the desktop user panel at the bottom left (R23-R25), the call panel above it, settings behind the gear.
function media(phone: boolean) {
  window.matchMedia = vi.fn((query: string) => ({
    matches: phone && query === PHONE_QUERY, media: query, addEventListener: () => undefined, removeEventListener: () => undefined,
  })) as unknown as typeof window.matchMedia;
}
afterEach(() => {
  cleanup();
  replaceHistoryState(null, '', '/');
  Reflect.deleteProperty(window, 'matchMedia');
  // eslint-disable-next-line no-restricted-properties -- clears the tab memory the shell writes
  window.sessionStorage.clear();
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, 'mediaDevices');
});

const main = () => within(screen.getByRole('main'));
const panel = () => document.querySelector<HTMLElement>('.d-user-panel');
const inPanel = () => within(panel()!);
const stageBar = () => within(main().getByRole('toolbar', { name: 'voice channel controls' }));

/** The shell, with the worker's setSetting applied to the settings slice as the real worker publishes it. */
function shell(o: VoiceHarnessOptions = {}) {
  const app = renderVoiceShell({ path: `/c/${A}/${TEXT}`, ...o });
  const inner = app.client.handler;
  app.client.handler = (command: Command) => {
    if (command.m === 'setSetting') {
      const next = { ...(app.client.get('settings') ?? {}) };
      if (command.value === null) delete next[command.key]; else next[command.key] = command.value;
      app.client.publish('settings', next);
    }
    return inner(command);
  };
  return app;
}
const writes = (app: ReturnType<typeof shell>) => app.client.callsOf('setSetting').map(c => c.value);

async function joinVoice(app: ReturnType<typeof shell>) {
  act(() => pushHistoryState(null, '', `/c/${A}/${GENV}`));
  await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
  await waitFor(() => expect(app.sessions).toHaveLength(1));
  app.client.publish('call', callState({ phase: 'in', channelId: GENV, callId: GENV }));
  await main().findByRole('toolbar', { name: 'voice channel controls' });
}

describe('the desktop user panel', () => {
  it('shows the display name and, under it, the presence label (Signal §5.4), not @username', async () => {
    const app = shell();
    await screen.findByRole('main');
    expect(inPanel().getByText(t('presence.online'))).toBeInTheDocument();
    expect(inPanel().queryByText('@ada')).toBeNull();
    expect(panel()).toHaveTextContent('ada');
    app.client.publish(`members:${A}`, MEMBERS.map(m => (m.userId === ME.id ? { ...m, display: 'Ada Lovelace' } : m)));
    await waitFor(() => expect(panel()).toHaveTextContent('Ada Lovelace'));
    act(() => app.client.publish('account', account({ presenceChoice: 'dnd' })));
    expect(await inPanel().findByText(t('presence.dnd'))).toBeInTheDocument();
  });

  it('outside a call, mute writes voice.self muted and shows pressed; deafen then writes deafened, and again muted', async () => {
    const app = shell();
    await screen.findByRole('main');
    const mute = () => inPanel().getByRole('button', { name: t('voice.mute') });
    const deafen = () => inPanel().getByRole('button', { name: t('voice.deafen') });
    expect(mute()).toHaveAttribute('aria-pressed', 'false');
    await app.user.click(mute());
    expect(writes(app)).toEqual(['muted']);
    expect(app.client.callsOf('setSetting')[0].key).toBe(SETTINGS.voiceSelf);
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'true'));
    await app.user.click(deafen());
    expect(writes(app)).toEqual(['muted', 'deafened']);
    await waitFor(() => expect(deafen()).toHaveAttribute('aria-pressed', 'true'));
    await app.user.click(deafen());
    expect(writes(app)).toEqual(['muted', 'deafened', 'muted']);
    await waitFor(() => expect(deafen()).toHaveAttribute('aria-pressed', 'false'));
    expect(mute()).toHaveAttribute('aria-pressed', 'true');
  });

  it('outside a call, mute from deafened clears the setting (R25), and mute again sets it', async () => {
    const app = shell({ settings: { [SETTINGS.voiceSelf]: 'deafened' } });
    await screen.findByRole('main');
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(writes(app)).toEqual([null]);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(writes(app)).toEqual([null, 'muted']);
  });

  it('the next join starts muted after a mute before the call', async () => {
    const app = shell();
    await screen.findByRole('main');
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    await joinVoice(app);
    expect(app.module.startPageCall).toHaveBeenCalledTimes(1);
    expect(vi.mocked(app.module.startPageCall).mock.calls[0][2].self).toBe('muted');
  });

  it('in a call, mute calls the session and the button follows the session', async () => {
    const app = shell();
    await joinVoice(app);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(app.log).toContain('muted:true');
    expect(app.session.state.muted).toBe(true);
    expect(inPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-pressed', 'true');
    // The session's own deafen shows on the panel too.
    act(() => app.session.update({ deafened: true }));
    expect(inPanel().getByRole('button', { name: t('voice.deafen') })).toHaveAttribute('aria-pressed', 'true');
  });

  it('has no settings gear (Signal §5.4: settings is the top bar\'s gear, which opens the Settings root)', async () => {
    const app = shell();
    await screen.findByRole('main');
    expect(inPanel().queryByRole('button', { name: t('panel.settings') })).toBeNull();
    expect(inPanel().getAllByRole('button').map(b => b.getAttribute('aria-label') ?? '')).not.toContain(t('workspace.open'));
    await app.user.click(within(document.querySelector<HTMLElement>('.d-topbar')!).getByRole('button', { name: t('workspace.open') }));
    expect(window.location.pathname).toBe('/settings');
  });

  it('the avatar and name block is a button named by the current status; it opens the four statuses and a choice sets the account presence', async () => {
    const app = shell();
    await screen.findByRole('main');
    const who = inPanel().getByRole('button', { name: new RegExp(`${t('panel.status', { status: t('presence.online') })}$`) });
    expect(who).toHaveTextContent(t('presence.online'));
    // WCAG 2.5.3: the name holds the visible name and the status.
    expect(who).toHaveAccessibleName('ada, your status: online');
    await app.user.click(who);
    const menu = screen.getByRole('menu');
    expect(within(menu).getAllByRole('menuitemradio').map(i => i.textContent)).toEqual(
      [t('presence.online'), t('presence.idle'), t('presence.dnd'), t('presence.invisible')]);
    await app.user.click(within(menu).getByRole('menuitemradio', { name: t('presence.invisible') }));
    expect(app.client.callsOf('setPresenceChoice')).toEqual([{ m: 'setPresenceChoice', choice: 'invisible' }]);
    expect(window.location.pathname).not.toBe('/settings/devices');
  });

  it('the status button is named by the stored choice and has no old sidebar select beside it', async () => {
    const app = shell();
    await screen.findByRole('main');
    expect(screen.queryByRole('combobox', { name: t('presence.label') })).toBeNull();
    act(() => app.client.publish('account', account({ presenceChoice: 'dnd' })));
    expect(await inPanel().findByRole('button', { name: new RegExp(`${t('panel.status', { status: t('presence.dnd') })}$`) })).toBeInTheDocument();
    expect(panel()!.querySelector('.d-avatar__presence')).toHaveAttribute('data-presence', 'dnd');
  });

  it('names the person by their display name before they have joined any server', async () => {
    const app = shell();
    await screen.findByRole('main');
    act(() => {
      app.client.publish(`members:${A}`, []);
      app.client.publish('account', account({ user: { ...ME, display: 'Ada Lovelace' } }));
    });
    await waitFor(() => expect(panel()).toHaveTextContent('Ada Lovelace'));
    expect(inPanel().getByText(t('presence.online'))).toBeInTheDocument();
  });

  it('the rail has no settings button on desktop', async () => {
    shell();
    const rail = await screen.findByRole('navigation', { name: t('shell.rail.label') });
    expect(within(rail).queryByRole('button', { name: t('shell.rail.settings') })).toBeNull();
  });

  it('panel controls are named, and the panel sits in the bottom-left area', async () => {
    shell();
    await screen.findByRole('main');
    expect(screen.getByRole('toolbar', { name: t('panel.region') })).toBeInTheDocument();
    expect(panel()!.closest('.d-app-shell__panel')).not.toBeNull();
  });
});

describe('the R25 unmute row and the announcements outside a call', () => {
  const mute = () => inPanel().getByRole('button', { name: t('voice.mute') });
  const said = () => screen.getByTestId('shell-announcer');

  it('mute, then mute again, writes muted then none and ends unpressed (button)', async () => {
    const app = shell();
    await screen.findByRole('main');
    await app.user.click(mute());
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'true'));
    await app.user.click(mute());
    expect(writes(app)).toEqual(['muted', null]);
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'false'));
  });

  it('mute, then mute again, writes muted then none and ends unpressed (Ctrl+Shift+M)', async () => {
    const app = shell();
    await screen.findByRole('main');
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'true'));
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    expect(writes(app)).toEqual(['muted', null]);
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'false'));
  });

  it('announces mute and deafen through the shell announcer, from the button and from the shortcut', async () => {
    const app = shell();
    await screen.findByRole('main');
    await app.user.click(mute());
    await waitFor(() => expect(said()).toHaveTextContent(t('voice.announce.muted')));
    await app.user.click(mute());
    await waitFor(() => expect(said()).toHaveTextContent(t('voice.announce.unmuted')));
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    await waitFor(() => expect(said()).toHaveTextContent(t('voice.announce.deafened')));
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    await waitFor(() => expect(said()).toHaveTextContent(t('voice.announce.undeafened')));
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await waitFor(() => expect(said()).toHaveTextContent(t('voice.announce.unmuted')));
  });
});

describe('the shortcuts outside a call (C-10)', () => {
  it('Ctrl+Shift+M and Ctrl+Shift+D make the same writes as the buttons on desktop', async () => {
    const app = shell();
    await screen.findByRole('main');
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    expect(writes(app)).toEqual(['muted']);
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    expect(writes(app)).toEqual(['muted', 'deafened']);
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    expect(writes(app)).toEqual(['muted', 'deafened', 'muted']);
  });

  it('do nothing on the phone shell', async () => {
    media(true);
    const app = shell();
    await screen.findByRole('navigation', { name: t('nav.tabs.label') });
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    await app.user.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    expect(app.client.callsOf('setSetting')).toEqual([]);
    expect(document.querySelector('.d-user-panel')).toBeNull();
  });
});

describe('one set of shortcuts and the call panel (R3)', () => {
  it('exactly one element carries aria-keyshortcuts with Shift+M while the stage is shown, and it is the panel’s', async () => {
    const app = shell();
    await joinVoice(app);
    const carriers = [...document.querySelectorAll('[aria-keyshortcuts*="Shift+M"]')];
    expect(carriers).toHaveLength(1);
    expect(panel()!.contains(carriers[0])).toBe(true);
    expect(document.querySelectorAll('[aria-keyshortcuts*="Shift+D"]')).toHaveLength(1);
    expect(stageBar().getByRole('button', { name: t('callStage.bar.mute') })).not.toHaveAttribute('aria-keyshortcuts');
  });

  it('the call panel has leave alone (Signal §5.4, Ruling U: camera and share are the stage\'s) and no mute, and sits above the user panel', async () => {
    const app = shell();
    await joinVoice(app);
    const dock = screen.getByRole('toolbar', { name: 'voice controls' });
    expect(within(dock).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual([t('voice.leave')]);
    const callPanel = document.querySelector('.dw-voice-dock')!;
    expect(callPanel.compareDocumentPosition(panel()!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(callPanel.parentElement).toBe(panel()!.parentElement);
  });

  it('web3.9 R26, MP5: the call panel reads the channel ("general · Midgard" read in full) over the link stats, and has no meter or level text', async () => {
    const app = shell();
    await joinVoice(app);
    const callPanel = within(document.querySelector<HTMLElement>('.dw-voice-dock')!);
    // Ruling U: line 1 is text, not a button (Diagnostics is in Settings).
    expect(callPanel.queryByRole('button', { name: 'general · Midgard' })).toBeNull();
    expect(callPanel.getByText('general · Midgard')).toHaveClass('d-sr-only');
    expect(document.querySelector('.dw-voice-dock .d-voice-dock__text')).toHaveTextContent(/^general$/);
    expect(document.querySelector('.dw-voice-dock .d-voice-dock__where')).toHaveTextContent('— ms');
    expect(document.querySelector('.dw-voice-dock')).not.toHaveTextContent(t('settings.voice.level'));
    expect(document.querySelector('.dw-voice-dock [role="meter"], .dw-voice-dock .d-audio-meter')).toBeNull();
    for (const b of callPanel.getAllByRole('button').filter(x => x.closest('[role="toolbar"]') !== null)) expect(b).toHaveAttribute('data-size', '28');
  });

  it('Ctrl+Shift+M in a call toggles the session, once', async () => {
    const app = shell({ settings: {} });
    await joinVoice(app);
    await app.user.keyboard('{Control>}{Shift>}M{/Shift}{/Control}');
    expect(app.log.filter(l => l === 'muted:true')).toHaveLength(1);
    expect(app.client.callsOf('setSetting').map(c => c.value)).toEqual(['muted']);
  });
});

describe('fail closed (security round 1)', () => {
  it('a mute pressed while the join is pending leaves the session muted once it starts', async () => {
    const app = shell();
    await screen.findByRole('main');
    act(() => pushHistoryState(null, '', `/c/${A}/${GENV}`));
    const release = app.holdStart();
    await app.user.click(await main().findByRole('button', { name: 'Join voice' }));
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(writes(app)).toEqual(['muted']);
    // Until the session exists the panel shows the stored setting.
    expect(inPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-pressed', 'true');
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.state.muted).toBe(true));
    expect(app.log).toContain('muted:true');
    expect(inPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-pressed', 'true');
  });

  it('only ever toward privacy: with no setting the attach changes nothing, and a session started muted is never opened', async () => {
    const app = shell();
    await joinVoice(app);
    expect(app.log.filter(l => l.startsWith('muted:') || l.startsWith('deafened:'))).toEqual([]);
    act(() => app.session.update({ muted: true }));
    expect(app.log).not.toContain('muted:false');
  });

  it('two quick presses read one source: muted, then null', async () => {
    const app = shell();
    await screen.findByRole('main');
    // The worker answers late: the slice still says "none" when the second press lands.
    const inner = app.client.handler;
    const held: (() => void)[] = [];
    app.client.handler = (command: Command) => (command.m === 'setSetting'
      ? new Promise(resolve => { held.push(() => { void inner(command).then(resolve); }); }) : inner(command));
    const mute = () => inPanel().getByRole('button', { name: t('voice.mute') });
    await app.user.click(mute());
    await app.user.click(mute());
    expect(writes(app)).toEqual(['muted', null]);
    held.forEach(h => h());
  });

  /** The voice channel's stage with its join button, before any join. */
  async function openVoiceChannel() {
    await screen.findByRole('main');
    act(() => pushHistoryState(null, '', `/c/${A}/${GENV}`));
    return main().findByRole('button', { name: 'Join voice' });
  }
  /** setSetting answers only when its turn is released, one by one, in the order sent. */
  function holdWrites(app: ReturnType<typeof shell>) {
    const inner = app.client.handler;
    const held: (() => void)[] = [];
    app.client.handler = (command: Command) => (command.m === 'setSetting'
      ? new Promise(resolve => { held.push(() => { void inner(command).then(resolve); }); }) : inner(command));
    return { next: () => { act(() => { held.shift()?.(); }); }, all: () => { act(() => { held.splice(0).forEach(h => h()); }); } };
  }
  const selfLog = (app: ReturnType<typeof shell>) => app.log.filter(l => l.startsWith('muted:') || l.startsWith('deafened:'));
  const startOpts = (app: ReturnType<typeof shell>, n = 0) => vi.mocked(app.module.startPageCall).mock.calls[n][2];

  it('a deafen pressed while the join is pending: the session is muted first, then deafened (so an undeafen stays muted)', async () => {
    const app = shell();
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(writes(app)).toEqual(['deafened']);
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.state.deafened).toBe(true));
    expect(selfLog(app)).toEqual(['muted:true', 'deafened:true']);
  });

  it('the attach reads a write still unanswered, not only the slice, and the start is handed the same live reading', async () => {
    const app = shell();
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    const answers = holdWrites(app);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(writes(app)).toEqual(['deafened']);
    // The start read `self` before the press; its live getter sees the press at once.
    expect(startOpts(app).self).toBe(null);
    expect(startOpts(app).selfNow?.()).toBe('deafened');
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.state.deafened).toBe(true));
    expect(selfLog(app)).toEqual(['muted:true', 'deafened:true']);
    answers.all();
  });

  it('a press while the join is pending is pushed to the starting session at once, with selfNow already answering it', async () => {
    const app = shell();
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    const answers = holdWrites(app);
    const heard: (string | null)[] = [];
    const off = startOpts(app).watchSelf?.(() => { heard.push(startOpts(app).selfNow?.() ?? null); });
    expect(off).toBeTypeOf('function');
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    // Pushed with the press, before the worker answered the write.
    expect(heard).toEqual(['muted']);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(heard).toEqual(['muted', 'deafened']);
    off?.();
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(heard).toHaveLength(2);
    answers.all();
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
  });

  it('an in-call toggle (keepSelf) is pushed too', async () => {
    const app = shell();
    await joinVoice(app);
    const heard: (string | null)[] = [];
    startOpts(app).watchSelf?.(() => { heard.push(startOpts(app).selfNow?.() ?? null); });
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(heard).toEqual(['muted']);
  });

  it('a setting less private than the session never opens it: unmute or undeafen pressed while the join is pending', async () => {
    const app = shell({ settings: { [SETTINGS.voiceSelf]: 'muted' } });
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    // The start read "muted"; the press clears the setting while the start still runs.
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(writes(app)).toEqual([null]);
    expect(startOpts(app).self).toBe('muted');
    expect(startOpts(app).selfNow?.()).toBe(null);
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(inPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-pressed', 'true'));
    expect(app.session.state.muted).toBe(true);
    expect(selfLog(app)).toEqual([]);
  });

  it('an undeafen pressed while the join is pending never undeafens the session', async () => {
    const app = shell({ settings: { [SETTINGS.voiceSelf]: 'deafened' } });
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(writes(app)).toEqual(['muted']);
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(inPanel().getByRole('button', { name: t('voice.deafen') })).toHaveAttribute('aria-pressed', 'true'));
    expect(app.session.state).toMatchObject({ muted: true, deafened: true });
    expect(selfLog(app)).toEqual([]);
  });

  it('an answer clears only its own write: a deafen pressed after a mute is still read when the mute is answered', async () => {
    const app = shell();
    const join = await openVoiceChannel();
    const release = app.holdStart();
    await app.user.click(join);
    await waitFor(() => expect(app.client.callsOf('callJoin')).toHaveLength(1));
    const answers = holdWrites(app);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(writes(app)).toEqual(['muted', 'deafened']);
    // The mute is answered (the slice says muted); the deafen is not yet.
    answers.next();
    await waitFor(() => expect(app.client.get('settings')?.[SETTINGS.voiceSelf]).toBe('muted'));
    release();
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    await waitFor(() => expect(app.session.state.deafened).toBe(true));
    expect(selfLog(app)).toEqual(['muted:true', 'deafened:true']);
    answers.all();
  });

  it('an in-call mute whose write is refused is kept: the rejoin after a disconnect starts muted, and the panel says so', async () => {
    const app = shell();
    await joinVoice(app);
    const inner = app.client.handler;
    app.client.handler = (command: Command) => (command.m === 'setSetting' ? Promise.reject(new Error('E_INTERNAL')) : inner(command));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(app.session.state.muted).toBe(true);
    expect(app.client.get('settings')?.[SETTINGS.voiceSelf]).toBeUndefined();
    app.client.handler = inner;
    act(() => {
      app.session.update({ phase: 'ended', error: 'E_DISCONNECTED' });
      app.client.publish('call', callState({ phase: 'removed', channelId: GENV,
        error: { code: 'E_DISCONNECTED', detail: '', status: 0, retryAfterMs: null } }));
    });
    // Between the loss and the rejoin the panel shows the kept mute, not the stale setting.
    await waitFor(() => expect(app.sessions).toHaveLength(1));
    expect(inPanel().getByRole('button', { name: t('voice.mute') })).toHaveAttribute('aria-pressed', 'true');
    await waitFor(() => expect(app.sessions).toHaveLength(2), { timeout: 4_000 });
    expect(startOpts(app, 1).self).toBe('muted');
    expect(app.session.state.muted).toBe(true);
  });

  it('an in-call mute whose write is refused survives a reload: the resumed call starts muted (final review m3)', async () => {
    const app = shell();
    await joinVoice(app);
    const inner = app.client.handler;
    app.client.handler = (command: Command) => (command.m === 'setSetting' ? Promise.reject(new Error('E_STORE_BUSY')) : inner(command));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(app.session.state.muted).toBe(true);
    await app.user.click(inPanel().getByRole('button', { name: t('voice.deafen') }));
    expect(app.session.state.deafened).toBe(true);
    expect(app.client.get('settings')?.[SETTINGS.voiceSelf]).toBeUndefined();
    // The page reloads: nothing of the first page's memory is left, and the store never took the write.
    cleanup();
    const reloaded = shell();
    await joinVoice(reloaded);
    expect(startOpts(reloaded).self).toBe('deafened');
    expect(reloaded.session.state.muted).toBe(true);
    expect(reloaded.session.state.deafened).toBe(true);
  });

  it('a held mute is carried per account: another account in the same tab starts from its own setting, not the first one\'s mute (security fix, fail open to the setting)', async () => {
    const app = shell();
    await joinVoice(app);
    const inner = app.client.handler;
    app.client.handler = (command: Command) => (command.m === 'setSetting' ? Promise.reject(new Error('E_STORE_BUSY')) : inner(command));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    expect(app.session.state.muted).toBe(true);
    cleanup();
    const other = shell();
    act(() => other.client.publish('account', account({ user: { id: 'cc'.repeat(16), username: 'eve' } })));
    await joinVoice(other);
    expect(startOpts(other).self).toBe(null);
    expect(other.session.state.muted).toBe(false);
    // The first account, back in the tab, still finds its own held mute.
    cleanup();
    const again = shell();
    await joinVoice(again);
    expect(startOpts(again).self).toBe('muted');
  });

  it('sign-out and wipe leave no held mute in the tab\'s sessionStorage (m-b)', async () => {
    // eslint-disable-next-line no-restricted-properties -- reads back what the hook must have removed
    const held = () => Object.keys(window.sessionStorage).filter(k => k.startsWith('dilla.voiceSelfHeld'));
    const app = shell();
    await joinVoice(app);
    const inner = app.client.handler;
    app.client.handler = (command: Command) => (command.m === 'setSetting' ? Promise.reject(new Error('E_STORE_BUSY')) : inner(command));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    await waitFor(() => expect(held()).toEqual([`dilla.voiceSelfHeld.${ME.id}`]));
    // A reload passes through loading with no user: the held state must survive that.
    act(() => app.client.publish('account', account({ phase: 'loading', user: null })));
    expect(held()).toHaveLength(1);
    // The sign-out or wipe ends in cleared.
    act(() => app.client.publish('account', account({ phase: 'cleared', user: null })));
    expect(held()).toEqual([]);
  });

  it('a mute the store did take is not carried across a reload, and an answered write drops a carried one', async () => {
    const app = shell();
    await joinVoice(app);
    const inner = app.client.handler;
    let refuse = true;
    app.client.handler = (command: Command) => (command.m === 'setSetting' && refuse ? Promise.reject(new Error('E_STORE_BUSY')) : inner(command));
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    refuse = false;
    // The unmute is answered: whatever was carried is dropped, the stored setting is the truth again.
    await app.user.click(inPanel().getByRole('button', { name: t('voice.mute') }));
    await waitFor(() => expect(app.client.get('settings')?.[SETTINGS.voiceSelf]).toBeUndefined());
    cleanup();
    const reloaded = shell();
    await joinVoice(reloaded);
    expect(startOpts(reloaded).self).toBe(null);
    expect(reloaded.session.state.muted).toBe(false);
  });

  it('a refused setSetting leaves the panel showing what is stored, and the next press writes again', async () => {
    const app = shell();
    await screen.findByRole('main');
    const inner = app.client.handler;
    let refuse = true;
    app.client.handler = (command: Command) => (command.m === 'setSetting' && refuse ? Promise.reject(new Error('E_INTERNAL')) : inner(command));
    const mute = () => inPanel().getByRole('button', { name: t('voice.mute') });
    await app.user.click(mute());
    expect(mute()).toHaveAttribute('aria-pressed', 'false');
    refuse = false;
    await app.user.click(mute());
    expect(writes(app)).toEqual(['muted', 'muted']);
    await waitFor(() => expect(mute()).toHaveAttribute('aria-pressed', 'true'));
  });
});

it('defers an available update for a live call and leaves the call connected', async () => {
  let changed!: (entry: string) => void;
  const reload = vi.fn();
  const save = vi.fn(async () => {});
  const app = shell({ update: { currentEntry: '/assets/old.js',
    watch: options => { changed = entry => options.changed(entry); return () => {}; },
    store: { load: () => Promise.resolve(null), save, remove: async () => {} }, reload } });
  await joinVoice(app);
  act(() => changed('/assets/new.js'));
  expect(await screen.findByText(t('update.finishCall'))).toBeInTheDocument();
  await new Promise(resolve => setTimeout(resolve, 30));
  expect(reload).not.toHaveBeenCalled();
  expect(save).not.toHaveBeenCalled();
  expect(app.sessions[0].state.phase).not.toBe('ended');
});
