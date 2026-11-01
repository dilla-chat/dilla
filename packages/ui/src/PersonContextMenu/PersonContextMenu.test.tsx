import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { PersonContextMenu } from './PersonContextMenu.tsx';
import { tileMenu } from '../CallStage/stage.fixture.tsx';

const base = { open: true, at: { x: 10, y: 10 }, onOpenChange: () => {}, returnFocus: () => null };

describe('PersonContextMenu (fix round 1)', () => {
  it('shows who, where, the volume and the client-side items only; the range leads the arrow order', () => {
    render(<PersonContextMenu {...tileMenu('Ole')} {...base} />);
    expect(screen.getByText('Ole')).toBeInTheDocument();
    expect(screen.getByText('in harbour')).toBeInTheDocument();
    const range = screen.getByRole('slider', { name: 'User volume' });
    expect(range).toHaveFocus();
    expect([...document.querySelectorAll('[role^="menuitem"]')].map(i => i.textContent)).toEqual(['Mute for me', 'Copy username']);
    for (const wave of ['Move to', 'Server mute', 'Disconnect']) expect(screen.queryByText(wave)).toBeNull();
    fireEvent.keyDown(range, { key: 'ArrowDown' });
    expect(screen.getByRole('menuitemcheckbox', { name: 'Mute for me' })).toHaveFocus();
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

  it('yourself: no volume and no Mute for me', () => {
    render(<PersonContextMenu {...tileMenu('Mira', { own: true, title: 'Mira (you)' })} {...base} />);
    expect(screen.queryByRole('slider')).toBeNull();
    expect([...document.querySelectorAll('[role^="menuitem"]')].map(i => i.textContent)).toEqual(['Copy username']);
  });

  it('passes axe', async () => {
    const { baseElement } = render(<PersonContextMenu {...tileMenu('Ole')} {...base} />);
    expect((await axe(baseElement, { rules: { 'color-contrast': { enabled: false }, region: { enabled: false } } })).violations).toEqual([]);
  });
});
