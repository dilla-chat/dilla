import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChannelList } from './ChannelList.tsx';
import type { VoiceParticipantView } from '../CallTile/CallTile.tsx';
import { tileMenu } from '../CallStage/stage.fixture.tsx';

const STATE = { speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
  connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound' };
const person = (name: string): VoiceParticipantView => ({ deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false,
  muted: false, deafened: false, video: false, screen: false, web: false, bot: false });

function List() {
  return (
    <ChannelList label="channels" title="Midgard Crew" activeId="harbour" onSelect={() => {}} emptyLabel="none"
      channels={[{ id: 'general', name: 'general', kind: 'text', readable: false }, { id: 'harbour', name: 'harbour', kind: 'voice', readable: false }]}
      rowMenu={{ label: name => `menu for ${name}`, items: id => (id === 'harbour' ? [{ id: 'code', label: 'call code' }] : []), onSelect: () => {} }}
      voice={{ labels: { open: 'voice {name}', count: '{n} in voice', playbackOnly: 'playback only', join: 'join voice', joined: 'connected', state: STATE },
        rooms: { harbour: { joined: true, participants: [{ ...person('Ada'), muted: true, video: true }, person('Ole')] } },
        personMenu: (_room, p) => tileMenu(p.name) }} />
  );
}

afterEach(() => { vi.useRealTimers(); });

describe('fix round 2: the person menu of a sidebar occupant', () => {
  // Task 11 (8 minor): the label is the name alone; the state words stay readable as the row's description.
  it('an occupant with a menu is named by the name and described by its state words', () => {
    render(<List />);
    const ada = screen.getByRole('listitem', { name: 'Ada' });
    // jsdom's description walk skips role=img children, so the ids are followed by hand: they hold every state word.
    const words = (ada.getAttribute('aria-describedby') ?? '').split(' ')
      .flatMap(id => [...(document.getElementById(id)?.querySelectorAll('[data-state]') ?? [])])
      .map(el => el.getAttribute('aria-label') ?? el.textContent);
    expect(words).toEqual(['camera on', 'muted']);
  });

  it('a long press on an occupant opens only the person menu, never the voice row\'s menu as well', () => {
    vi.useFakeTimers();
    render(<List />);
    const ole = screen.getByRole('listitem', { name: 'Ole' });
    // jsdom has no PointerEvent: a MouseEvent named pointerdown with a pointerType, as ChannelList.test does.
    act(() => { ole.dispatchEvent(Object.assign(new MouseEvent('pointerdown', { bubbles: true, clientX: 20, clientY: 20 }), { pointerType: 'touch' })); });
    act(() => { vi.advanceTimersByTime(600); });
    expect(screen.getAllByRole('menu')).toHaveLength(1);
    expect(screen.getByRole('menu', { name: 'actions for Ole' })).toBeInTheDocument();
    expect(screen.queryByRole('menu', { name: 'menu for harbour' })).toBeNull();
  });

  it('a right-click on the row itself still opens the row\'s menu (the call code)', () => {
    render(<List />);
    fireEvent.contextMenu(screen.getByRole('button', { name: 'voice harbour' }), { clientX: 20, clientY: 20 });
    expect(screen.getByRole('menu', { name: 'menu for harbour' })).toBeInTheDocument();
    expect(screen.queryByRole('menu', { name: 'actions for Ole' })).toBeNull();
  });

  it('the keyboard reaches an occupant (ArrowRight from the row, ArrowDown between people) and Shift+F10 opens its menu', () => {
    render(<List />);
    const row = screen.getByRole('button', { name: 'voice harbour' });
    row.focus();
    fireEvent.keyDown(row, { key: 'ArrowRight' });
    expect(screen.getByRole('listitem', { name: 'Ada' })).toHaveFocus();
    fireEvent.keyDown(screen.getByRole('listitem', { name: 'Ada' }), { key: 'ArrowDown' });
    const ole = screen.getByRole('listitem', { name: 'Ole' });
    expect(ole).toHaveFocus();
    fireEvent.keyDown(ole, { key: 'F10', shiftKey: true });
    expect(screen.getByRole('menu', { name: 'actions for Ole' })).toBeInTheDocument();
    expect(screen.queryByRole('menu', { name: 'menu for harbour' })).toBeNull();
    // A key inside the open menu bubbles through React to the person's row: it is not a new request.
    fireEvent.keyDown(screen.getByRole('menuitem', { name: 'Copy username' }), { key: 'F10', shiftKey: true });
    expect(screen.getAllByRole('menu')).toHaveLength(1);
    fireEvent.keyDown(ole, { key: 'ArrowLeft' });
    expect(row).toHaveFocus();
  });
});
