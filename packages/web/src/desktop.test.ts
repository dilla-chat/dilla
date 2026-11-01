import { describe, it, expect, vi } from 'vitest';
import type { DesktopBridge, DesktopCommand, DesktopEvent } from '../../desktop/src/contract.ts';
import { desktopNotificationCtor, startDesktopLifecycle, setDesktopGuard, startDesktopWindowLayout } from './desktop.ts';

function bridge() {
  const commands: DesktopCommand[] = [];
  const listeners = new Set<(event: DesktopEvent) => void>();
  const desktop: DesktopBridge = {
    info: () => Promise.resolve({ version: '0.1.0-beta.1', instance: 'https://chat.test/', capabilities: { applicationAudio: false, browserAuth: false, notifications: true, screenCapture: true } }),
    send: command => { commands.push(command); return Promise.resolve(); },
    onEvent: listener => { listeners.add(listener); return () => { listeners.delete(listener); }; },
  };
  return { desktop, commands, emit(event: DesktopEvent) { for (const listener of listeners) listener(event); } };
}

describe('desktop integration', () => {
  it('routes notification clicks to the existing notifier and disposes callbacks on close', () => {
    const fixture = bridge();
    const Ctor = desktopNotificationCtor(fixture.desktop, () => 'granted');
    const note = new Ctor('A message', { body: 'Hello', tag: 'dilla:channel' });
    let opens = 0;
    note.onclick = () => { opens++; };
    expect(fixture.commands).toContainEqual({ type: 'notify', id: 'dilla:channel', title: 'A message', body: 'Hello' });
    fixture.emit({ type: 'notification-click', id: 'other' });
    expect(opens).toBe(0);
    fixture.emit({ type: 'notification-click', id: 'dilla:channel' });
    expect(opens).toBe(1);
    note.close();
    fixture.emit({ type: 'notification-click', id: 'dilla:channel' });
    expect(opens).toBe(1);
  });

  it('answers quit probes with fresh call/draft state, including unsent form input', () => {
    const fixture = bridge();
    const stop = startDesktopLifecycle(fixture.desktop);
    setDesktopGuard({ activeCall: true, unsavedDraft: false }, fixture.desktop);
    fixture.emit({ type: 'prepare-quit', id: 'first' });
    expect(fixture.commands.at(-1)).toEqual({ type: 'quit-ack', id: 'first', activeCall: true, unsavedDraft: false });
    setDesktopGuard({ activeCall: false, unsavedDraft: true }, fixture.desktop);
    fixture.emit({ type: 'prepare-quit', id: 'second' });
    expect(fixture.commands.at(-1)).toEqual({ type: 'quit-ack', id: 'second', activeCall: false, unsavedDraft: true });
    setDesktopGuard({ activeCall: false, unsavedDraft: false }, fixture.desktop);
    const input = document.createElement('textarea');
    input.value = 'unsaved'; document.body.append(input);
    fixture.emit({ type: 'prepare-quit', id: 'third' });
    expect(fixture.commands.at(-1)).toMatchObject({ unsavedDraft: true });
    input.remove(); stop();
  });

  it('blocks reload while a controlled form or call has unsaved state', () => {
    const fixture = bridge();
    const stop = startDesktopLifecycle(fixture.desktop);
    const input = document.createElement('input'); input.value = 'edited'; input.defaultValue = 'edited'; document.body.append(input);
    const event = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    input.remove();
    setDesktopGuard({ activeCall: true, unsavedDraft: false }, fixture.desktop);
    const callEvent = new Event('beforeunload', { cancelable: true }); window.dispatchEvent(callEvent);
    expect(callEvent.defaultPrevented).toBe(true);
    setDesktopGuard({ activeCall: false, unsavedDraft: false }, fixture.desktop);
    stop();
  });

  it('replaced or OS-closed notifications cannot replay historical clicks or close their replacements', () => {
    const fixture = bridge();
    const Ctor = desktopNotificationCtor(fixture.desktop, () => 'granted');
    let oldClicks = 0, currentClicks = 0;
    const old = new Ctor('Old', { tag: 'dilla:channel' }); old.onclick = () => { oldClicks++; old.close(); };
    const current = new Ctor('Current', { tag: 'dilla:channel' }); current.onclick = () => { currentClicks++; };
    old.close();
    fixture.emit({ type: 'notification-click', id: 'dilla:channel' });
    expect(oldClicks).toBe(0); expect(currentClicks).toBe(1);
    expect(fixture.commands.filter(command => command.type === 'dismiss-notification')).toHaveLength(0);
    fixture.emit({ type: 'notification-close', id: 'dilla:channel' });
    fixture.emit({ type: 'notification-click', id: 'dilla:channel' });
    expect(currentClicks).toBe(1);
  });
});

it('ring actions are delivered only to the live notification and retired on close', () => {
 const fixture = bridge(); const Ctor = desktopNotificationCtor(fixture.desktop, () => 'granted');
 const note = new Ctor('Incoming call', { tag: 'dilla-ring-current' });
 let action = ''; note.onaction = next => { action = next; note.close(); };
 expect(fixture.commands[0]).toMatchObject({ type: 'notify-ring' });
 fixture.emit({ type: 'notification-action', id: 'dilla-ring-stale', action: 'answer' }); expect(action).toBe('');
 fixture.emit({ type: 'notification-action', id: 'dilla-ring-current', action: 'decline' }); expect(action).toBe('decline');
 fixture.emit({ type: 'notification-action', id: 'dilla-ring-current', action: 'answer' }); expect(action).toBe('decline');
});


it('initializes the desktop workspace only after the native resize has reached the renderer', async () => {
 const fixture = bridge(), ready = vi.fn();
 const width = Object.getOwnPropertyDescriptor(window, 'innerWidth')!;
 const height = Object.getOwnPropertyDescriptor(window, 'innerHeight')!;
 let frame: FrameRequestCallback | undefined;
 vi.stubGlobal('requestAnimationFrame', (callback: FrameRequestCallback) => { frame = callback; return 1; });
 vi.stubGlobal('cancelAnimationFrame', vi.fn());
 Object.defineProperty(window, 'innerWidth', { configurable: true, value: 400 });
 Object.defineProperty(window, 'innerHeight', { configurable: true, value: 500 });
 const stop = startDesktopWindowLayout('workspace', fixture.desktop, ready);
 try {
  await Promise.resolve(); expect(ready).not.toHaveBeenCalled();
  Object.defineProperty(window, 'innerWidth', { configurable: true, value: 1240 });
  Object.defineProperty(window, 'innerHeight', { configurable: true, value: 820 });
  frame?.(0); expect(ready).toHaveBeenCalledOnce();
 } finally { stop(); Object.defineProperty(window, 'innerWidth', width); Object.defineProperty(window, 'innerHeight', height); vi.unstubAllGlobals(); }
});
