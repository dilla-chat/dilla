import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { SettingsNav, type SettingsNavProps } from './SettingsNav.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-02: settings.nav.label and settings.nav.{devices,notifications,appearance}.
const ITEMS = [{ id: 'devices', label: 'devices' }, { id: 'notifications', label: 'notifications' }, { id: 'appearance', label: 'appearance' }] as const;
function props(over: Partial<SettingsNavProps> = {}): SettingsNavProps {
  return { label: 'settings sections', items: ITEMS, activeId: 'notifications', onSelect: vi.fn(), ...over };
}
const btn = (name: string) => screen.getByRole('button', { name });

describe('SettingsNav', () => {
  it('is a navigation landmark of buttons in order, the active one the current page', () => {
    render(<SettingsNav {...props()} />);
    const nav = screen.getByRole('navigation', { name: 'settings sections' });
    expect(nav).toHaveClass('d-settings-nav');
    const items = within(nav).getAllByRole('button');
    expect(items.map(b => b.textContent)).toEqual(['devices', 'notifications', 'appearance']);
    expect(items.map(b => b.getAttribute('aria-current'))).toEqual([null, 'page', null]);
  });

  it('is one tab stop on the active section; arrows, Home and End move focus without selecting; Enter selects', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<><button type="button">before</button><SettingsNav {...p} /><button type="button">after</button></>);
    await user.tab();
    await user.tab();
    expect(btn('notifications')).toHaveFocus();
    await user.tab();
    expect(btn('after')).toHaveFocus();
    await user.tab({ shift: true });
    await user.keyboard('{ArrowDown}');
    expect(btn('appearance')).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(btn('appearance')).toHaveFocus();
    await user.keyboard('{Home}');
    expect(btn('devices')).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(btn('devices')).toHaveFocus();
    await user.keyboard('{End}');
    expect(btn('appearance')).toHaveFocus();
    expect(p.onSelect).not.toHaveBeenCalled();
    await user.keyboard('{Enter}');
    expect(p.onSelect).toHaveBeenCalledTimes(1);
    expect(p.onSelect).toHaveBeenLastCalledWith('appearance');
  });

  it('selects on click', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<SettingsNav {...p} />);
    await user.click(btn('devices'));
    expect(p.onSelect).toHaveBeenLastCalledWith('devices');
  });

  it('renders group headings only for groups that have items', () => {
    render(<SettingsNav {...props({ items: [
      { id: 'profile', label: 'profile', group: 'Account' },
      { id: 'devices', label: 'devices', group: 'Account' },
      { id: 'appearance', label: 'appearance', group: 'Preferences' },
      { id: 'extra', label: 'extra' },
    ] })} />);
    const nav = screen.getByRole('navigation', { name: 'settings sections' });
    expect(within(nav).getAllByRole('heading', { level: 2 }).map(h => h.textContent)).toEqual(['Account', 'Preferences']);
    expect(within(nav).getAllByRole('button').map(b => b.textContent)).toEqual(['profile', 'devices', 'appearance', 'extra']);
  });

  it('keeps group headings out of the roving keyboard stops', async () => {
    const user = userEvent.setup();
    render(<SettingsNav {...props({ activeId: 'devices', items: [
      { id: 'profile', label: 'profile', group: 'Account' },
      { id: 'devices', label: 'devices', group: 'Account' },
      { id: 'appearance', label: 'appearance', group: 'Preferences' },
    ] })} />);
    await user.tab();
    expect(btn('devices')).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(btn('appearance')).toHaveFocus();
    await user.keyboard('{ArrowUp}');
    expect(btn('devices')).toHaveFocus();
  });

  it('scrolls the active section item into view when the selection changes', () => {
    const scroll = vi.fn();
    const descriptor = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView');
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
    const { rerender } = render(<SettingsNav {...props()} />);
    scroll.mockClear();
    rerender(<SettingsNav {...props({ activeId: 'appearance' })} />);
    expect(scroll).toHaveBeenCalledWith({ block: 'nearest' });
    if (descriptor) Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', descriptor);
    else Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView');
  });

  it('grouped navigation has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><SettingsNav {...props({ items: ITEMS.map(item => ({ ...item, group: 'Account' })) })} /></div>);
    await expectNoAxeViolations(container);
  });
});

describe('SettingsNav glyph column', () => {
  const ICONS = [{ id: 'general', label: 'general', icon: 'settings' }, { id: 'about', label: 'about' }, { id: 'keyboard', label: 'keyboard', icon: 'keyboard' }] as const;

  it('draws a 15px icon per item (README §5.9) and keeps the empty column for an item without one, so names align', () => {
    render(<SettingsNav {...props({ items: ICONS, activeId: 'general' })} />);
    const items = screen.getAllByRole('button');
    for (const item of items) expect(item.querySelectorAll('.d-settings-nav__glyph')).toHaveLength(1);
    expect(items[0].querySelector('[data-icon="settings"]')).toHaveAttribute('width', '15');
    expect(items[1].querySelector('.d-settings-nav__glyph svg')).toBeNull();
    expect(items.map(b => b.textContent)).toEqual(['general', 'about', 'keyboard']);
  });

  it('has no glyph column when no item has an icon (control)', () => {
    render(<SettingsNav {...props()} />);
    expect(document.querySelector('.d-settings-nav__glyph')).toBeNull();
  });

  it('keeps the accessible name the label alone', () => {
    render(<SettingsNav {...props({ items: ICONS })} />);
    expect(screen.getByRole('button', { name: 'general' })).toBeInTheDocument();
  });
});

// M4: the glyph column is 16px and never shrinks, which is what aligns the names (jsdom has no layout).
describe('SettingsNav glyph column CSS', () => {
  it('is 15px wide with flex: none', async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    const css = fs.readFileSync(`${cwd}/src/SettingsNav/SettingsNav.css`, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    const rule = /\.d-settings-nav__glyph \{([^}]*)\}/.exec(css)?.[1] ?? '';
    expect(rule).toMatch(/flex: none;/);
    expect(rule).toMatch(/width: 15px;/);
  });
});
