import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { MessageToolbar, type MessageToolbarProps } from './MessageToolbar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

function items(spy = vi.fn()): MessageToolbarProps['items'] {
  return [
    { id: 'react', label: 'react', onAction: () => spy('react') },
    { id: 'reply', label: 'reply', onAction: () => spy('reply') },
    { id: 'thread', label: 'reply in thread', onAction: () => spy('thread') },
    { id: 'edit', label: 'edit', onAction: () => spy('edit') },
    { id: 'pin', label: 'pin', onAction: () => spy('pin') },
    { id: 'delete', label: 'delete', danger: true, onAction: () => spy('delete') },
  ];
}
const bar = (p: Partial<MessageToolbarProps> = {}) => <MessageToolbar moreLabel="more actions" label="actions for this message" tabbable items={items()} {...p} />;
const stops = () => Array.from(document.querySelectorAll<HTMLButtonElement>('.d-message-toolbar__btn'));

describe('MessageToolbar', () => {
  it('is the prototype\'s icon toolbar: react, reply, thread, then "more" holding edit, pin and delete', async () => {
    const user = userEvent.setup();
    render(bar());
    const toolbar = screen.getByRole('toolbar', { name: 'actions for this message' });
    expect(toolbar).toHaveAttribute('aria-orientation', 'horizontal');
    expect(stops().map(b => b.getAttribute('aria-label'))).toEqual(['react', 'reply', 'reply in thread', 'more actions']);
    expect(stops().slice(0, 3).every(b => b.querySelector('svg') !== null && b.textContent === '')).toBe(true);
    await user.click(screen.getByRole('button', { name: 'more actions' }));
    expect(screen.getAllByRole('menuitem').map(m => m.textContent)).toEqual(['edit', 'pin', 'delete']);
    expect(screen.getByRole('menuitem', { name: 'delete' })).toHaveAttribute('data-danger', 'true');
  });

  it('has no "more" button when every item has its own icon', () => {
    render(bar({ items: items().slice(0, 2) }));
    expect(screen.queryByRole('button', { name: 'more actions' })).toBeNull();
    expect(stops()).toHaveLength(2);
  });

  it('is one tab stop when tabbable and none when not', () => {
    const { rerender } = render(bar());
    expect(stops().map(b => b.tabIndex)).toEqual([0, -1, -1, -1]);
    rerender(bar({ tabbable: false }));
    expect(stops().map(b => b.tabIndex)).toEqual([-1, -1, -1, -1]);
  });

  it('moves focus with the arrows, wrapping, and with Home and End; the stop follows focus', () => {
    render(bar());
    const b = stops();
    b[0]!.focus();
    fireEvent.keyDown(b[0]!, { key: 'ArrowRight' });
    expect(b[1]).toHaveFocus();
    fireEvent.keyDown(b[1]!, { key: 'End' });
    expect(b[3]).toHaveFocus();
    fireEvent.keyDown(b[3]!, { key: 'ArrowRight' });
    expect(b[0]).toHaveFocus();
    fireEvent.keyDown(b[0]!, { key: 'ArrowLeft' });
    expect(b[3]).toHaveFocus();
    fireEvent.keyDown(b[3]!, { key: 'Home' });
    expect(b[0]).toHaveFocus();
    fireEvent.keyDown(b[0]!, { key: 'ArrowRight' });
    expect(stops().map(x => x.tabIndex)).toEqual([-1, 0, -1, -1]);
  });

  it('calls each action once: a button with itself as the anchor, a menu item with the toolbar', async () => {
    const user = userEvent.setup();
    const spy = vi.fn();
    const anchors: HTMLElement[] = [];
    render(bar({ items: items(spy).map(i => ({ ...i, onAction: (a: HTMLElement) => { anchors.push(a); i.onAction(a); } })) }));
    await user.click(screen.getByRole('button', { name: 'reply' }));
    expect(anchors[0]).toBe(screen.getByRole('button', { name: 'reply' }));
    await user.click(screen.getByRole('button', { name: 'more actions' }));
    await user.click(screen.getByRole('menuitem', { name: 'pin' }));
    expect(anchors[1]).toBe(screen.getByRole('toolbar'));
    screen.getByRole('button', { name: 'react' }).focus();
    await user.keyboard('{Enter}');
    expect(spy.mock.calls.map(c => c[0])).toEqual(['reply', 'pin', 'react']);
  });

  it('lets Escape bubble to the row', () => {
    const onKeyDown = vi.fn();
    render(<div onKeyDown={e => onKeyDown(e.key)}>{bar()}</div>);
    fireEvent.keyDown(stops()[2]!, { key: 'Escape' });
    expect(onKeyDown).toHaveBeenCalledWith('Escape');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root">{bar()}</div>);
    await expectNoAxeViolations(container);
  });
});
