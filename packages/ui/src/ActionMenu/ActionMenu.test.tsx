import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ActionMenu, type ActionMenuProps } from './ActionMenu.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const items = [{ id: 'kick', label: 'kick' }, { id: 'ban', label: 'ban', danger: true }, { id: 'copy', label: 'copy id' }];

/** Holds `open` the way a caller does, and reports selections. */
function Harness({ onSelect, at, startOpen = false, frame }: {
  onSelect?: (id: string) => void; at?: ActionMenuProps['at']; startOpen?: boolean; frame?: 'shell' | 'scroller';
}) {
  const [open, setOpen] = useState(startOpen);
  const inner = (
    <div>
      <button type="button">before</button>
      <ActionMenu label="actions for Astrid" items={items} open={open} onOpenChange={setOpen} onSelect={onSelect ?? (() => {})} at={at} />
      <button type="button">after</button>
    </div>
  );
  if (frame === 'shell') return <div className="d-app-shell">{inner}</div>;
  if (frame === 'scroller') return <div data-testid="scroller" style={{ overflowY: 'auto' }}>{inner}</div>;
  return inner;
}

/** Mocks getBoundingClientRect by a class name or test id on the element. */
function mockRects(rects: Record<string, { left?: number; top?: number; width: number; height: number }>) {
  vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
    const key = Object.keys(rects).find(k => this.classList.contains(k) || this.getAttribute('data-testid') === k);
    const r = key ? rects[key]! : { width: 0, height: 0 };
    const left = r.left ?? 0;
    const top = r.top ?? 0;
    return { left, top, x: left, y: top, width: r.width, height: r.height, right: left + r.width, bottom: top + r.height, toJSON() { return {}; } };
  });
}

const trigger = () => screen.getByRole('button', { name: 'actions for Astrid' });
const menuItems = () => screen.getAllByRole('menuitem');

afterEach(() => { delete document.documentElement.dataset.theme; vi.restoreAllMocks(); });

describe('ActionMenu', () => {
  it('is a menu button: closed it shows no menu and says so', () => {
    render(<Harness />);
    expect(trigger()).toHaveAttribute('aria-haspopup', 'menu');
    expect(trigger()).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it.each(['{Enter}', ' ', '{ArrowDown}'])('%j on the trigger opens the menu on the first item', async key => {
    const user = userEvent.setup();
    render(<Harness />);
    trigger().focus();
    await user.keyboard(key);
    expect(screen.getByRole('menu', { name: 'actions for Astrid' })).toBeInTheDocument();
    expect(trigger()).toHaveAttribute('aria-expanded', 'true');
    expect(menuItems()[0]).toHaveFocus();
  });

  it('ArrowUp on the trigger opens the menu on the last item', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    trigger().focus();
    await user.keyboard('{ArrowUp}');
    expect(menuItems()[2]).toHaveFocus();
  });

  it('arrows wrap; Home and End jump', async () => {
    const user = userEvent.setup();
    render(<Harness startOpen />);
    expect(menuItems()[0]).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(menuItems()[2]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(menuItems()[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(menuItems()[1]).toHaveFocus();
    await user.keyboard('{End}');
    expect(menuItems()[2]).toHaveFocus();
    await user.keyboard('{Home}');
    expect(menuItems()[0]).toHaveFocus();
  });

  it('Escape closes the menu and puts focus back on the trigger', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    trigger().focus();
    await user.keyboard('{Enter}');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(trigger()).toHaveFocus();
  });

  it('Tab closes the menu and moves on from the trigger', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    trigger().focus();
    await user.keyboard('{Enter}');
    await user.tab();
    expect(screen.queryByRole('menu')).toBeNull();
    expect(screen.getByRole('button', { name: 'after' })).toHaveFocus();
  });

  it('a pointerdown outside closes the menu; one inside does not', async () => {
    const user = userEvent.setup();
    render(<Harness startOpen />);
    await user.pointer({ keys: '[MouseLeft>]', target: menuItems()[0]! });
    expect(screen.getByRole('menu')).toBeInTheDocument();
    await user.pointer({ keys: '[MouseLeft>]', target: screen.getByRole('button', { name: 'before' }) });
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('selecting an item calls onSelect with its id and closes the menu', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<Harness onSelect={onSelect} />);
    await user.click(trigger());
    await user.click(screen.getByRole('menuitem', { name: 'ban' }));
    expect(onSelect).toHaveBeenCalledExactlyOnceWith('ban');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(trigger()).toHaveFocus();
  });

  it('Enter on a focused item selects it', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<Harness onSelect={onSelect} startOpen />);
    await user.keyboard('{ArrowDown}{Enter}');
    expect(onSelect).toHaveBeenCalledExactlyOnceWith('ban');
  });

  it('marks danger items for --danger', () => {
    render(<Harness startOpen />);
    expect(screen.getByRole('menuitem', { name: 'ban' })).toHaveAttribute('data-danger', 'true');
    expect(screen.getByRole('menuitem', { name: 'kick' })).not.toHaveAttribute('data-danger');
  });

  it('`at` places the popup at the pointer; the popup is clamped inside the viewport', () => {
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(160);
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(100);
    const { unmount } = render(<Harness startOpen at={{ x: 40, y: 50 }} />);
    expect(screen.getByRole('menu')).toHaveStyle({ position: 'fixed', left: '40px', top: '50px' });
    unmount();
    render(<Harness startOpen at={{ x: window.innerWidth - 10, y: window.innerHeight - 10 }} />);
    expect(screen.getByRole('menu')).toHaveStyle({ left: `${window.innerWidth - 160}px`, top: `${window.innerHeight - 100}px` });
  });

  it('inside the app shell `at` is measured from the shell’s corner and clamped to the shell, not the window', () => {
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(160);
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(100);
    mockRects({ 'd-app-shell': { left: 100, top: 50, width: 400, height: 300 } });
    const { unmount } = render(<Harness startOpen frame="shell" at={{ x: 150, y: 120 }} />);
    expect(screen.getByRole('menu')).toHaveStyle({ position: 'fixed', left: '50px', top: '70px' });
    unmount();
    render(<Harness startOpen frame="shell" at={{ x: 490, y: 340 }} />);
    expect(screen.getByRole('menu')).toHaveStyle({ left: '240px', top: '200px' });
  });

  it('without `at`, a trigger in the bottom half of its scroller opens the menu upward; in the top half, downward', () => {
    mockRects({ 'd-action-menu__trigger': { top: 280, width: 24, height: 24 }, scroller: { top: 0, width: 300, height: 300 } });
    const { unmount } = render(<Harness startOpen frame="scroller" />);
    expect(screen.getByRole('menu')).toHaveAttribute('data-placement', 'top');
    unmount();
    vi.restoreAllMocks();
    mockRects({ 'd-action-menu__trigger': { top: 10, width: 24, height: 24 }, scroller: { top: 0, width: 300, height: 300 } });
    render(<Harness startOpen frame="scroller" />);
    expect(screen.getByRole('menu')).toHaveAttribute('data-placement', 'bottom');
  });

  it('without a scroller the viewport decides', () => {
    mockRects({ 'd-action-menu__trigger': { top: window.innerHeight - 40, width: 24, height: 24 } });
    render(<Harness startOpen />);
    expect(screen.getByRole('menu')).toHaveAttribute('data-placement', 'top');
  });

  it('opens on its first item when `at` is set', () => {
    render(<Harness startOpen at={{ x: 5, y: 5 }} />);
    expect(menuItems()[0]).toHaveFocus();
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, closed and open', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const startOpen of [false, true]) {
      const { container, unmount } = render(<div className="d-root"><Harness startOpen={startOpen} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });
});

describe('ActionMenu focus key', () => {
  it('puts focusKey on the trigger as data-focus-key, so a dialog can find a re-mounted trigger', () => {
    render(<ActionMenu label="actions for Astrid" items={items} open={false} onOpenChange={() => {}} onSelect={() => {}} focusKey="member:u1" />);
    expect(trigger()).toHaveAttribute('data-focus-key', 'member:u1');
  });
});

describe('ActionMenu without a trigger (a context menu, Ruling X)', () => {
  function Bare({ onSelect = () => {} }: { onSelect?: (id: string) => void }) {
    const [open, setOpen] = useState(true);
    return (
      <div>
        <button type="button" id="row">general</button>
        <ActionMenu label="general menu" items={items} open={open} onOpenChange={setOpen} onSelect={onSelect} at={{ x: 10, y: 10 }}
          trigger="none" returnFocus={() => document.getElementById('row')} />
      </div>
    );
  }
  it('renders no trigger button, only the menu, and focuses its first item', () => {
    render(<Bare />);
    expect(screen.queryByRole('button', { name: 'general menu' })).toBeNull();
    expect(screen.getByRole('menu', { name: 'general menu' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'kick' })).toHaveFocus();
  });
  it('Escape closes it and gives focus back to the row', async () => {
    const user = userEvent.setup();
    render(<Bare />);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(screen.getByRole('button', { name: 'general' })).toHaveFocus();
  });
  it('a choice closes it, focuses the row and reports the id', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();
    render(<Bare onSelect={onSelect} />);
    await user.click(screen.getByRole('menuitem', { name: 'ban' }));
    expect(onSelect).toHaveBeenCalledWith('ban');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(screen.getByRole('button', { name: 'general' })).toHaveFocus();
  });
});

// Signal task 10 fix round 3 (WAI-ARIA APG): radio items that share a `group` sit in one labelled role="group".
describe('ActionMenu groups', () => {
  it('wraps consecutive items of one group in a labelled group and keeps aria-checked on the current one', () => {
    render(<ActionMenu label="general menu" open onOpenChange={() => {}} onSelect={() => {}}
      items={[{ id: 'pins', label: 'Pinned' }, { id: 'all', label: 'All', checked: false, group: 'Notifications' }, { id: 'none', label: 'Nothing', checked: true, group: 'Notifications' }]} />);
    const group = screen.getByRole('group', { name: 'Notifications' });
    expect(within(group).getAllByRole('menuitemradio')).toHaveLength(2);
    expect(within(group).getByRole('menuitemradio', { name: 'Nothing' })).toHaveAttribute('aria-checked', 'true');
    expect(within(group).queryByRole('menuitem', { name: 'Pinned' })).toBeNull();
  });

  // Merge of lanes 9 and 10: the group keeps task 9's tick on the current radio, its separator and the label's title.
  it('draws the tick on the checked radio inside a group, a separator before a sep item, and titles each label', () => {
    render(<ActionMenu label="general menu" open onOpenChange={() => {}} onSelect={() => {}}
      items={[{ id: 'mute', label: 'Mute', check: false }, { id: 'all', label: 'Notify me about every message in this conversation', checked: false, group: 'Notifications', sep: true },
        { id: 'none', label: 'Nothing', checked: true, group: 'Notifications' }]} />);
    const group = screen.getByRole('group', { name: 'Notifications' });
    const nothing = within(group).getByRole('menuitemradio', { name: 'Nothing' });
    expect(nothing.querySelector('.d-action-menu__tick')).not.toBeNull();
    expect(within(group).getByRole('menuitemradio', { name: /every message/ }).querySelector('.d-action-menu__tick')).toBeNull();
    expect(within(group).getByRole('separator')).toBeInTheDocument();
    expect(within(group).getByRole('menuitemradio', { name: /every message/ }).querySelector('.d-action-menu__label'))
      .toHaveAttribute('title', 'Notify me about every message in this conversation');
  });
});
