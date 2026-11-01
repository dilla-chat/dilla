import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { CommunityRail, type CommunityRailProps } from './CommunityRail.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const ITEMS = [
  { id: 'a1', name: 'Midgard Crew' },
  { id: 'b2', name: 'Night Owls' },
  { id: 'c3', name: 'raid planning' },
] as const;
const BADGED = [
  { id: 'a1', name: 'Midgard Crew' },
  { id: 'b2', name: 'Night Owls', unread: 4 },
  { id: 'c3', name: 'raid planning', unread: 1, mentions: 2 },
] as const;
// L-COPY-02 shell.rail.itemLabel, filled in as en.ts's t() will.
const itemLabel = (i: { name: string; unread: number; mentions: number }) => `${i.name}, unread ${i.unread}, mentions ${i.mentions}`;

function props(over: Partial<CommunityRailProps> = {}): CommunityRailProps {
  return { label: 'servers', items: ITEMS, activeId: 'b2', onSelect: vi.fn(), addLabel: 'add a server', onAdd: vi.fn(), ...over };
}
function wrapped(p: CommunityRailProps) {
  return <><button type="button">before</button><CommunityRail {...p} /><button type="button">after</button></>;
}
const btn = (name: string) => screen.getByRole('button', { name });

describe('CommunityRail', () => {
  it('is a navigation landmark of buttons named after each server, the active one marked current', () => {
    render(wrapped(props()));
    const nav = screen.getByRole('navigation', { name: 'servers' });
    expect(nav).toHaveClass('d-community-rail');
    expect(within(nav).getAllByRole('button').map(b => b.getAttribute('aria-current'))).toEqual([null, 'page', null, null]);
    expect(btn('Night Owls')).toHaveAttribute('aria-current', 'page');
    expect(btn('add a server')).toBeInTheDocument();
  });

  it('shows initials but names each tile in full', () => {
    render(wrapped(props()));
    const initials = within(btn('Midgard Crew')).getByText('MC');
    expect(initials).toHaveAttribute('aria-hidden', 'true');
    expect(within(btn('raid planning')).getByText('RP')).toBeInTheDocument();
    expect(within(btn('Night Owls')).getByText('NO')).toBeInTheDocument();
  });

  it('is a single tab stop that lands on the active server', async () => {
    const user = userEvent.setup();
    render(wrapped(props()));
    await user.tab();
    expect(btn('before')).toHaveFocus();
    await user.tab();
    expect(btn('Night Owls')).toHaveFocus();
    await user.tab();
    expect(btn('after')).toHaveFocus();
    await user.tab({ shift: true });
    expect(btn('Night Owls')).toHaveFocus();
  });

  it('moves focus with the arrows, Home and End, without wrapping or selecting, and remembers the stop', async () => {
    const user = userEvent.setup();
    const p = props();
    render(wrapped(p));
    await user.tab();
    await user.tab();
    await user.keyboard('{ArrowDown}');
    expect(btn('raid planning')).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{Home}');
    expect(btn('Midgard Crew')).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(btn('Midgard Crew')).toHaveFocus();
    await user.keyboard('{End}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(btn('raid planning')).toHaveFocus();
    await user.tab();
    expect(btn('after')).toHaveFocus();
    await user.tab({ shift: true });
    expect(btn('raid planning')).toHaveFocus();
    expect(p.onSelect).not.toHaveBeenCalled();
  });

  it('activates with Enter and Space', async () => {
    const user = userEvent.setup();
    const p = props();
    render(wrapped(p));
    await user.tab();
    await user.tab();
    await user.keyboard('{ArrowDown}{Enter}');
    expect(p.onSelect).toHaveBeenLastCalledWith('c3');
    await user.keyboard('{Home} ');
    expect(p.onSelect).toHaveBeenLastCalledWith('a1');
    await user.keyboard('{End}{Enter}');
    expect(p.onAdd).toHaveBeenCalledTimes(1);
  });

  it('makes the first server the stop when none is active', async () => {
    const user = userEvent.setup();
    render(wrapped(props({ activeId: null })));
    await user.tab();
    await user.tab();
    expect(btn('Midgard Crew')).toHaveFocus();
  });

  it('leaves Alt+Arrow to the shell', async () => {
    const user = userEvent.setup();
    render(wrapped(props()));
    await user.tab();
    await user.tab();
    await user.keyboard('{Alt>}{ArrowDown}{/Alt}');
    expect(btn('Night Owls')).toHaveFocus();
  });

  it('resets the stop to the newly active server', async () => {
    const user = userEvent.setup();
    const p = props();
    const { rerender } = render(wrapped(p));
    await user.tab();
    await user.tab();
    await user.keyboard('{Home}');
    await user.tab();
    rerender(wrapped({ ...p, activeId: 'c3' }));
    await user.tab({ shift: true });
    expect(btn('raid planning')).toHaveFocus();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><CommunityRail {...props()} /></div>);
    await expectNoAxeViolations(container);
  });

  it('badges a server with its counts as data and a pill', () => {
    render(wrapped(props({ items: BADGED })));
    const owls = btn('Night Owls 4 unread');
    expect([owls.getAttribute('data-unread'), owls.getAttribute('data-mentions')]).toEqual(['4', '0']);
    expect(within(owls).getByText('NO')).toHaveAttribute('aria-hidden', 'true');
    expect(btn('raid planning 2 mentions')).toHaveAttribute('data-mentions', '2');
    expect(btn('Midgard Crew')).toHaveAttribute('data-unread', '0');
  });

  it('names a badged server by itemLabel and leaves a quiet one to its name', () => {
    render(wrapped(props({ items: BADGED, itemLabel })));
    expect(btn('Night Owls, unread 4, mentions 0')).toBeInTheDocument();
    expect(btn('raid planning, unread 1, mentions 2')).toBeInTheDocument();
    expect(btn('Midgard Crew')).not.toHaveAttribute('aria-label');
  });

  it('puts the settings button last, inside the one tab stop', async () => {
    const user = userEvent.setup();
    const p = props({ settingsLabel: 'settings', onSettings: vi.fn() });
    render(wrapped(p));
    await user.tab();
    await user.tab();
    expect(btn('Night Owls')).toHaveFocus();
    await user.tab();
    expect(btn('after')).toHaveFocus();
    await user.tab({ shift: true });
    await user.keyboard('{End}');
    expect(btn('settings')).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{ArrowDown}{Enter}');
    expect(p.onSettings).toHaveBeenCalledTimes(1);
    const all = within(screen.getByRole('navigation', { name: 'servers' })).getAllByRole('button');
    expect(all[all.length - 1]).toBe(btn('settings'));
    expect(btn('settings').closest('ul')).toBe(btn('add a server').closest('ul'));
  });

  it('draws no settings button unless both its label and its handler are given', () => {
    render(wrapped(props({ settingsLabel: 'settings' })));
    expect(screen.queryByRole('button', { name: 'settings' })).toBeNull();
  });

  // v0.1.0-web3.1, flow 07: a fresh instance starts here. The one "+" tile ends the server list (before settings).
  it('ends the list with the one add tile, then settings, inside the one tab stop', async () => {
    const user = userEvent.setup();
    const p = props({ settingsLabel: 'settings', onSettings: vi.fn() });
    render(wrapped(p));
    const all = within(screen.getByRole('navigation', { name: 'servers' })).getAllByRole('button');
    expect(all.slice(-2)).toEqual([btn('add a server'), btn('settings')]);
    await user.tab();
    await user.tab();
    await user.keyboard('{End}{ArrowUp}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(p.onAdd).toHaveBeenCalledTimes(1);
  });

  it('has no serious axe violations with the add tile alone', async () => {
    const { container } = render(<div className="d-root"><CommunityRail {...props({ items: [], activeId: null,
      settingsLabel: 'settings', onSettings: () => {} })} /></div>);
    await expectNoAxeViolations(container);
  });

  it('has no serious axe violations with badges and the settings button', async () => {
    const { container } = render(<div className="d-root"><CommunityRail {...props({ items: BADGED, itemLabel, settingsLabel: 'settings', onSettings: () => {} })} /></div>);
    await expectNoAxeViolations(container);
  });
});

// web3.9 (UI-2), Signal: the rail with the `@` tile, a hairline, and one "+" tile.
describe('CommunityRail, the @ tile and the add tile', () => {
  const home = (over: Partial<NonNullable<CommunityRailProps['home']>> = {}): NonNullable<CommunityRailProps['home']> =>
    ({ label: 'home', selected: false, unread: 0, mentions: 0, itemLabel: 'home, 2 unread', onSelect: vi.fn(), ...over });

  it('puts the @ tile first, then a hairline, then the servers; it is named home and takes its filled name while unread', () => {
    const { rerender } = render(wrapped(props({ home: home() })));
    const nav = screen.getByRole('navigation', { name: 'servers' });
    const first = within(nav).getAllByRole('button')[0];
    expect(first).toHaveAccessibleName('home');
    expect(first.querySelector('svg')).toHaveAttribute('data-icon', 'at-sign');
    const items = [...nav.querySelectorAll('ul > li')];
    expect(items[0]).toContainElement(first);
    expect(items[1]).toHaveClass('d-community-rail__rule');
    expect(items[1]).toHaveAttribute('aria-hidden', 'true');
    expect(items[2]).toContainElement(btn('Midgard Crew'));
    rerender(wrapped(props({ home: home({ unread: 2 }) })));
    expect(btn('home, 2 unread')).toBeInTheDocument();
    expect(btn('home, 2 unread')).toHaveAttribute('data-unread', '2');
  });
  it('marks the @ tile current when selected, and then it is the one tab stop', async () => {
    const user = userEvent.setup();
    const h = home({ selected: true });
    render(wrapped(props({ home: h, activeId: null })));
    expect(btn('home')).toHaveAttribute('aria-current', 'page');
    await user.tab();
    await user.tab();
    expect(btn('home')).toHaveFocus();
    await user.keyboard('{Enter}{ArrowDown}');
    expect(h.onSelect).toHaveBeenCalledTimes(1);
    expect(btn('Midgard Crew')).toHaveFocus();
  });
  it('lands the tab stop on the active server when the @ tile is not selected', async () => {
    const user = userEvent.setup();
    render(wrapped(props({ home: home() })));
    await user.tab();
    await user.tab();
    expect(btn('Night Owls')).toHaveFocus();
  });
  it('draws no left-edge pill (Signal §5.3: selected is the tile itself, unread is for a screen reader)', () => {
    const { container } = render(wrapped(props({ items: BADGED, activeId: 'a1', home: home() })));
    expect(container.querySelector('.d-community-rail__pill')).toBeNull();
  });
  it('merges join and create into one "+" tile that only asks to add (the dialog is the caller\'s)', async () => {
    const user = userEvent.setup();
    const onAdd = vi.fn();
    const p = props({ addLabel: 'add a server', onAdd });
    render(wrapped(p));
    expect(screen.queryByRole('button', { name: 'join a server' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'new server' })).toBeNull();
    const plus = btn('add a server');
    expect(plus).toHaveAttribute('aria-haspopup', 'dialog');
    await user.click(plus);
    expect(onAdd).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('menu')).toBeNull();
  });
  it('the "+" tile is a stop of the roving group and opens by keyboard', async () => {
    const user = userEvent.setup();
    const onAdd = vi.fn();
    render(wrapped(props({ addLabel: 'add a server', onAdd })));
    await user.tab();
    await user.tab();
    await user.keyboard('{End}');
    expect(btn('add a server')).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(onAdd).toHaveBeenCalledTimes(1);
  });
  it('unread shows no visible number, and a mention is a count badge at the bottom right', () => {
    const { container } = render(wrapped(props({ items: BADGED, home: home({ unread: 3 }), addLabel: 'add a server', onAdd: () => {} })));
    const owls = btn('Night Owls 4 unread');
    expect(owls.querySelector('.d-community-rail__badge')).toBeNull();
    expect(owls.querySelector('.d-sr-only .d-pill')).not.toBeNull();
    expect(btn('home, 2 unread').querySelector('.d-community-rail__badge')).toBeNull();
    const mention = btn('raid planning 2 mentions').querySelector('.d-community-rail__badge');
    expect(mention?.querySelector('.d-pill')).toHaveAttribute('data-kind', 'mention');
    expect(container.querySelectorAll('.d-community-rail__badge')).toHaveLength(1);
  });
  it('has no caption text anywhere', () => {
    const { container } = render(<CommunityRail {...props({ addLabel: 'add a server' })} />);
    expect(container.textContent).not.toMatch(/\bjoin\b|\bnew\b/);
  });
  it('has no serious axe violations with the @ tile and the add tile', async () => {
    const { container } = render(<div className="d-root"><CommunityRail {...props({ items: BADGED, itemLabel, home: home({ unread: 1 }), addLabel: 'add a server', onAdd: () => {},
      settingsLabel: 'settings', onSettings: () => {} })} /></div>);
    await expectNoAxeViolations(container);
  });
});
