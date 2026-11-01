import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { UserPanel, type UserPanelProps } from './UserPanel.tsx';
import { VOICE_SHORTCUTS } from '../CallControls/CallControls.tsx';
import { readFileSync } from 'node:fs';
import meta from './UserPanel.stories.tsx';

const panelCss = readFileSync(`${process.cwd()}/src/UserPanel/UserPanel.css`, 'utf8');

const panel = (over: Partial<UserPanelProps> = {}): UserPanelProps => ({
  name: 'Ada Lovelace', username: 'ada', muted: false, deafened: false,
  shortcuts: { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' },
  labels: { region: 'your account', mute: 'mute', deafen: 'deafen', settings: 'settings' },
  status: {
    current: 'online', label: 'your status: Online',
    items: [{ id: 'online', label: 'Online' }, { id: 'idle', label: 'Idle' }, { id: 'dnd', label: 'Do Not Disturb' }, { id: 'invisible', label: 'Invisible' }],
    onChoose: () => {},
  },
  onMute: () => {}, onDeafen: () => {}, onSettings: () => {}, ...over,
});

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('UserPanel', () => {
  it('gives the truncated name and @username a title with the full text', () => {
    render(<UserPanel {...panel({ name: 'Ada Lovelace', username: 'ada' })} />);
    expect(screen.getByText('Ada Lovelace')).toHaveAttribute('title', 'Ada Lovelace');
    expect(screen.getByText('@ada')).toHaveAttribute('title', '@ada');
  });

  it('Signal §5.4: with `line` the status shows under the name in place of @username; without onSettings there is no gear', () => {
    render(<UserPanel {...panel({ name: 'Mira', username: 'mira', line: 'do not disturb', onSettings: undefined })} />);
    expect(screen.getByText('do not disturb')).toHaveClass('d-user-panel__username');
    expect(screen.queryByText('@mira')).toBeNull();
    expect(screen.queryByRole('button', { name: 'settings' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Mira, your status: Online' })).toBeInTheDocument();
  });

  it('is a plain div with no role and no name; its toolbar carries the region name', () => {
    const { container } = render(<UserPanel {...panel()} />);
    const root = container.firstElementChild!;
    expect(root).toHaveClass('d-user-panel');
    expect(root.tagName).toBe('DIV');
    expect(root).not.toHaveAttribute('role');
    expect(root).not.toHaveAttribute('aria-label');
    expect(root).not.toHaveAttribute('aria-labelledby');
    expect(screen.getByRole('toolbar', { name: 'your account' })).toBeInTheDocument();
    expect(screen.getAllByRole('toolbar')).toHaveLength(1);
    expect(screen.getAllByRole('toolbar')).toHaveLength(1);
  });

  it('names mute and deafen in the toolbar, as toggles carrying the shortcuts', () => {
    render(<UserPanel {...panel({ muted: true })} />);
    const bar = screen.getByRole('toolbar', { name: 'your account' });
    const mute = within(bar).getByRole('button', { name: 'mute' });
    const deafen = within(bar).getByRole('button', { name: 'deafen' });
    expect(mute).toHaveAttribute('aria-pressed', 'true');
    expect(deafen).toHaveAttribute('aria-pressed', 'false');
    expect(mute).toHaveAttribute('aria-keyshortcuts', VOICE_SHORTCUTS.mute);
    expect(deafen).toHaveAttribute('aria-keyshortcuts', VOICE_SHORTCUTS.deafen);
  });

  it('mute and deafen call their handlers; the settings button, outside the toolbar, calls onSettings', async () => {
    const [onMute, onDeafen, onSettings] = [vi.fn(), vi.fn(), vi.fn()];
    render(<UserPanel {...panel({ onMute, onDeafen, onSettings })} />);
    await userEvent.click(screen.getByRole('button', { name: 'mute' }));
    await userEvent.click(screen.getByRole('button', { name: 'deafen' }));
    expect(onMute).toHaveBeenCalledTimes(1);
    expect(onDeafen).toHaveBeenCalledTimes(1);
    const gear = screen.getByRole('button', { name: 'settings' });
    expect(within(screen.getByRole('toolbar')).queryByRole('button', { name: 'settings' })).toBeNull();
    expect(gear.querySelector('[data-icon="settings"]')).not.toBeNull();
    await userEvent.click(gear);
    expect(onSettings).toHaveBeenCalledTimes(1);
  });

  it('makes the avatar and name block one button named by the current status, with the name and @username inside', () => {
    render(<UserPanel {...panel()} />);
    const who = screen.getByRole('button', { name: 'Ada Lovelace @ada, your status: Online' });
    expect(who).toHaveAttribute('aria-haspopup', 'menu');
    expect(who).toHaveTextContent('Ada Lovelace');
    expect(who).toHaveTextContent('@ada');
    expect(who.querySelector('.d-avatar')).not.toBeNull();
    // The name block, the toolbar (one stop: mute, deafen) and the gear are the three tab stops.
    expect(screen.getAllByRole('button').filter(b => b.tabIndex === 0)).toHaveLength(3);
  });

  it('opens a status menu with the four statuses, and a choice calls onChoose with its id', async () => {
    const onChoose = vi.fn();
    render(<UserPanel {...panel({ status: { ...panel().status, onChoose } })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Ada Lovelace @ada, your status: Online' }));
    const menu = screen.getByRole('menu', { name: 'your status: Online' });
    expect(within(menu).getAllByRole('menuitemradio').map(i => i.textContent)).toEqual(['Online', 'Idle', 'Do Not Disturb', 'Invisible']);
    await userEvent.click(within(menu).getByRole('menuitemradio', { name: 'Do Not Disturb' }));
    expect(onChoose).toHaveBeenCalledWith('dnd');
    expect(screen.queryByRole('menu')).toBeNull();
  });

  it('is keyboard reachable: Enter opens it on the first item, Escape closes it and refocuses the name block', async () => {
    render(<UserPanel {...panel()} />);
    const who = screen.getByRole('button', { name: 'Ada Lovelace @ada, your status: Online' });
    who.focus();
    await userEvent.keyboard('{Enter}');
    expect(screen.getByRole('menuitemradio', { name: 'Online' })).toHaveFocus();
    await userEvent.keyboard('{ArrowDown}');
    expect(screen.getByRole('menuitemradio', { name: 'Idle' })).toHaveFocus();
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).toBeNull();
    expect(who).toHaveFocus();
  });

  it.each([['online', 'online'], ['idle', 'idle'], ['dnd', 'dnd'], ['invisible', 'offline']] as const)(
    'shows %s as a small dot on the avatar (%s)', (current, dot) => {
      const { container } = render(<UserPanel {...panel({ status: { ...panel().status, current } })} />);
      expect(container.querySelector('.d-user-panel .d-avatar__presence')).toHaveAttribute('data-presence', dot);
    });

  it('names the status button by its visible text (WCAG 2.5.3): the name, the @username and the status', () => {
    render(<UserPanel {...panel()} />);
    const who = screen.getByRole('button', { name: /your status/ });
    expect(who).toHaveAccessibleName('Ada Lovelace @ada, your status: Online');
    for (const visible of ['Ada Lovelace', '@ada']) expect(who.textContent).toContain(visible);
    expect(who.getAttribute('aria-label')).toBeNull();
  });

  it('marks the current status in the menu', async () => {
    render(<UserPanel {...panel({ status: { ...panel().status, current: 'idle' } })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Ada Lovelace @ada, your status: Online' }));
    expect(screen.getByRole('menuitemradio', { name: 'Idle' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('menuitemradio', { name: 'Online' })).toHaveAttribute('aria-checked', 'false');
  });

  it('E3: mute, deafen and the gear are the same 32px icon buttons as the dock, with tooltips', () => {
    render(<UserPanel {...panel()} />);
    for (const name of ['mute', 'deafen', 'settings']) {
      const b = screen.getByRole('button', { name });
      expect(b).toHaveClass('d-icon-button');
      expect(b).toHaveAttribute('data-size', '32');
      expect(b.parentElement).toHaveClass('d-tooltip-anchor');
    }
    expect(document.querySelectorAll('.d-user-panel .d-icon-button[data-size="32"]')).toHaveLength(3);
    expect(document.querySelector('.d-voice-toolbar')).toBeNull();
  });

  it('mute and deafen are one roving Tab stop: arrows wrap, Home/End jump, Tab goes on to the gear', async () => {
    render(<UserPanel {...panel()} />);
    const bar = screen.getByRole('toolbar', { name: 'your account' });
    await userEvent.keyboard('{Tab}{Tab}');
    expect(within(bar).getByRole('button', { name: 'mute' })).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(within(bar).getByRole('button', { name: 'deafen' })).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(within(bar).getByRole('button', { name: 'mute' })).toHaveFocus();
    await userEvent.keyboard('{End}');
    expect(within(bar).getByRole('button', { name: 'deafen' })).toHaveFocus();
    await userEvent.keyboard('{Home}');
    expect(within(bar).getByRole('button', { name: 'mute' })).toHaveFocus();
    await userEvent.keyboard('{Tab}');
    expect(screen.getByRole('button', { name: 'settings' })).toHaveFocus();
  });

  it('shows the shortcut in the mute tooltip on keyboard focus', async () => {
    render(<UserPanel {...panel()} />);
    screen.getByRole('button', { name: 'mute' }).focus();
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAccessibleDescription('Ctrl Shift M');
  });

  it('is --panel-h (64) high with a hairline top that meets the composer band (CSS pin, G3 is UI-4)', () => {
    expect(panelCss).toMatch(/^\.d-user-panel \{[^}]*height: var\(--panel-h\)/m);
    expect(panelCss).toMatch(/^\.d-user-panel \{[^}]*border-top: 1px solid var\(--hairline\)/m);
    expect(panelCss).not.toContain('d-user-panel__settings');
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><UserPanel {...(meta.args as UserPanelProps)} /></div>);
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  it('push to talk: a hidden PTT tag on the mic, "push to talk" in its description, and the ring while held', () => {
    const { rerender } = render(<UserPanel {...panel({ ptt: { tag: 'PTT', description: 'push to talk', held: false } })} />);
    const mic = screen.getByRole('button', { name: 'mute' });
    expect(mic).toHaveAccessibleDescription(/push to talk/);
    const tag = screen.getByText('PTT');
    expect(tag).toHaveAttribute('aria-hidden', 'true');
    expect(mic).toHaveAccessibleName('mute');
    expect(document.querySelector('.d-user-panel__avatar')).not.toHaveAttribute('data-speaking');
    rerender(<UserPanel {...panel({ ptt: { tag: 'PTT', description: 'push to talk', held: true } })} />);
    expect(document.querySelector('.d-user-panel__avatar')).toHaveAttribute('data-speaking', 'true');
  });
  it('without push to talk there is no tag, no description and no ring (control)', () => {
    render(<UserPanel {...panel()} />);
    expect(screen.queryByText('PTT')).toBeNull();
    expect(screen.getByRole('button', { name: 'mute' })).not.toHaveAccessibleDescription(/push to talk/);
    expect(document.querySelector('.d-user-panel__avatar')).not.toHaveAttribute('data-speaking');
  });
});
