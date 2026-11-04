import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { PersonContextMenu } from './PersonContextMenu.tsx';
import { tileMenu } from '../CallStage/stage.fixture.tsx';

const base = { open: true, at: { x: 10, y: 10 }, onOpenChange: () => {}, returnFocus: () => null };
const names = () => [...document.querySelectorAll('[role^="menuitem"]')].map(i => i.textContent);

describe('PersonContextMenu (fix round 1)', () => {
  it('shows who, where, the volume, Profile, Message, Mention, Mute for me, Copy username and Edit roles; the range leads the arrow order', () => {
    render(<PersonContextMenu {...tileMenu('Ole')} {...base} />);
    expect(screen.getByText('Ole')).toBeInTheDocument();
    expect(screen.getByText('in harbour')).toBeInTheDocument();
    const range = screen.getByRole('slider', { name: 'User volume' });
    expect(range).toHaveFocus();
    expect(names()).toEqual(['Profile', 'Message', 'Mention', 'Mute for me', 'Copy username', 'Edit roles']);
    for (const wave of ['Server mute', 'Disconnect', 'Move to']) expect(screen.queryByText(wave)).toBeNull();
    fireEvent.keyDown(range, { key: 'ArrowDown' });
    expect(screen.getByRole('menuitem', { name: 'Profile' })).toHaveFocus();
  });

  it('without Manage roles the app leaves Edit roles out (the prototype order otherwise)', () => {
    render(<PersonContextMenu {...tileMenu('Ole', { roles: false })} {...base} />);
    expect(names()).toEqual(['Profile', 'Message', 'Mention', 'Mute for me', 'Copy username']);
  });

  it('a bot (Glenn) has Profile, Mention, Mute for me, Copy username and Edit roles; no Message', () => {
    render(<PersonContextMenu {...tileMenu('Glenn', { bot: true })} {...base} />);
    expect(names()).toEqual(['Profile', 'Mention', 'Mute for me', 'Copy username', 'Edit roles']);
  });

  it('Mute for me is an on/off item that keeps the menu open; Copy username closes it', () => {
    const onSelect = vi.fn();
    const onOpenChange = vi.fn();
    render(<PersonContextMenu {...tileMenu('Ole', { muted: true })} {...base} onSelect={onSelect} onOpenChange={onOpenChange} />);
    const mute = screen.getByRole('menuitemcheckbox', { name: 'Mute for me' });
    expect(mute).toHaveAttribute('aria-checked', 'true');
    fireEvent.click(mute);
    expect(onSelect).toHaveBeenLastCalledWith('mute');
    expect(onOpenChange).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('menuitem', { name: 'Copy username' }));
    expect(onSelect).toHaveBeenLastCalledWith('copy');
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it('yourself: no volume, no Mute for me and no Copy username; Mute and Deafen carry their keys, Leave voice is the danger item', () => {
    render(<PersonContextMenu {...tileMenu('Mira', { own: true, title: 'Mira (you)' })} {...base} />);
    expect(screen.queryByRole('slider')).toBeNull();
    expect(names()).toEqual(['MuteCtrl⇧M', 'DeafenCtrl⇧D', 'Profile', 'lounge2', 'raid0', 'Leave voice']);
    expect(screen.getByRole('menuitem', { name: 'Leave voice' })).toHaveAttribute('data-danger', 'true');
    // The hint is decoration: the name is the word; the shortcut is aria-keyshortcuts.
    expect(screen.getByRole('menuitem', { name: 'Mute' })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+M');
    expect(document.querySelector('.d-action-menu__hint')).toHaveAttribute('aria-hidden', 'true');
  });

  it('Move to: a labelled group of rooms with the head count, after Profile and before Leave voice; one choice moves and closes', () => {
    const onMove = vi.fn();
    const onSelect = vi.fn();
    const onOpenChange = vi.fn();
    const menu = tileMenu('Mira', { own: true, title: 'Mira (you)' });
    render(<PersonContextMenu {...menu} move={{ ...menu.move!, onMove }} {...base} onSelect={onSelect} onOpenChange={onOpenChange} />);
    const group = screen.getByRole('group', { name: 'Move to' });
    expect(within(group).getAllByRole('menuitem').map(i => i.getAttribute('aria-label'))).toEqual(['lounge, 2 in voice', 'raid, 0 in voice']);
    expect(document.querySelector('.d-action-menu__heading')?.textContent).toBe('Move to');
    fireEvent.click(within(group).getByRole('menuitem', { name: 'lounge, 2 in voice' }));
    expect(onMove).toHaveBeenCalledWith('lounge');
    expect(onSelect).not.toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it('with no other voice channel, Move to is not drawn and Leave voice keeps its rule above it', () => {
    const menu = tileMenu('Mira', { own: true });
    render(<PersonContextMenu {...menu} move={{ label: 'Move to', rooms: [], roomLabel: () => '', onMove: () => {} }} {...base} />);
    expect(screen.queryByText('Move to')).toBeNull();
    expect(screen.getByRole('menuitem', { name: 'Leave voice' }).previousElementSibling).toHaveAttribute('role', 'separator');
  });

  it('passes axe', async () => {
    const { baseElement } = render(<PersonContextMenu {...tileMenu('Ole')} {...base} />);
    expect((await axe(baseElement, { rules: { 'color-contrast': { enabled: false }, region: { enabled: false } } })).violations).toEqual([]);
  });

  it('passes axe on your own menu', async () => {
    const { baseElement } = render(<PersonContextMenu {...tileMenu('Mira', { own: true })} {...base} />);
    expect((await axe(baseElement, { rules: { 'color-contrast': { enabled: false }, region: { enabled: false } } })).violations).toEqual([]);
  });
});
