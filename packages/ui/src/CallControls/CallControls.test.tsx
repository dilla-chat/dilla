import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { CallControls, type CallControlsProps } from './CallControls.tsx';
import meta from './CallControls.stories.tsx';

const labels: CallControlsProps['labels'] = {
  region: 'call controls', mute: 'mute', deafen: 'deafen', camera: 'camera', share: 'share screen', leave: 'leave',
};
const base: CallControlsProps = {
  muted: false, deafened: false, camera: false, sharing: false, canCamera: true, canShare: true, busy: false, labels,
  onMute: () => {}, onDeafen: () => {}, onCamera: () => {}, onShare: () => {}, onLeave: () => {},
};

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('CallControls', () => {
  it('a real hold still says paused (control)', () => {
    render(<CallControls {...base} canCamera={false} canShare={false} paused
      labels={{ ...labels, why: { paused: 'paused', joining: 'connecting', noShare: 'n/a' } }} />);
    expect(screen.getByRole('button', { name: 'camera' })).toHaveAccessibleDescription(/paused/);
  });

  it('mute is live', async () => {
    const onMute = vi.fn();
    render(<CallControls {...base} onMute={onMute} />);
    const mute = screen.getByRole('button', { name: 'mute' });
    expect(mute).not.toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(mute);
    expect(onMute).toHaveBeenCalledTimes(1);
  });

  it('hideShare leaves the share control out, and the rest keep their order and roving focus', async () => {
    render(<CallControls {...base} hideShare />);
    const buttons = within(screen.getByRole('toolbar', { name: 'call controls' })).getAllByRole('button');
    expect(buttons.map(b => b.getAttribute('aria-label'))).toEqual(['mute', 'deafen', 'camera', 'leave']);
    expect(buttons.filter(b => b.tabIndex === 0)).toEqual([buttons[0]]);
    await userEvent.keyboard('{Tab}{End}');
    expect(buttons[3]).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(buttons[0]).toHaveFocus();
  });

  it('is one toolbar named call controls with one tab stop and roving arrows', async () => {
    render(<CallControls {...base} />);
    const bar = screen.getByRole('toolbar', { name: 'call controls' });
    const buttons = within(bar).getAllByRole('button');
    ['mute', 'deafen', 'camera', 'share screen', 'leave'].forEach((name, i) => expect(buttons[i]).toHaveAccessibleName(name));
    expect(buttons).toHaveLength(5);
    expect(buttons.filter(b => b.tabIndex === 0)).toEqual([buttons[0]]);
    await userEvent.keyboard('{Tab}{End}');
    expect(buttons[4]).toHaveFocus();
    expect(buttons[4].tabIndex).toBe(0);
    await userEvent.keyboard('{ArrowRight}');
    expect(buttons[0]).toHaveFocus();
  });

  // WB a11y I4 (ruling 2026-10-14): a toggle keeps one visible label; aria-pressed and the aria-hidden [x] mark carry its
  // state, so "mute, toggle button, pressed" means muted and is never contradicted by a swapped label.
  it('toggles keep their label whatever their state, and each control calls its handler', async () => {
    const handlers = { onMute: vi.fn(), onDeafen: vi.fn(), onCamera: vi.fn(), onShare: vi.fn(), onLeave: vi.fn() };
    render(<CallControls {...base} {...handlers} muted deafened camera sharing />);
    for (const name of ['mute', 'deafen', 'camera', 'share screen', 'leave']) await userEvent.click(screen.getByRole('button', { name }));
    for (const gone of ['unmute', 'undeafen', 'stop camera', 'stop sharing']) expect(screen.queryByText(gone)).toBeNull();
    for (const fn of Object.values(handlers)) expect(fn).toHaveBeenCalledTimes(1);
  });

  it('busy, or a missing camera or share, is aria-disabled: still focusable, and it does nothing', async () => {
    const onCamera = vi.fn();
    const onShare = vi.fn();
    const onMute = vi.fn();
    const { rerender } = render(<CallControls {...base} canCamera={false} canShare={false} onCamera={onCamera} onShare={onShare} onMute={onMute} />);
    const camera = screen.getByRole('button', { name: 'camera' });
    expect(camera).toHaveAttribute('aria-disabled', 'true');
    expect(camera).not.toBeDisabled();
    await userEvent.click(camera);
    await userEvent.click(screen.getByRole('button', { name: 'share screen' }));
    expect(onCamera).not.toHaveBeenCalled();
    expect(onShare).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'mute' })).not.toHaveAttribute('aria-disabled');
    rerender(<CallControls {...base} busy onMute={onMute} />);
    for (const b of screen.getAllByRole('button')) expect(b).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(screen.getByRole('button', { name: 'mute' }));
    expect(onMute).not.toHaveBeenCalled();
  });

  it('mute, deafen, camera and share are Button toggles with aria-pressed; mute and deafen carry their shortcuts (M4, M5)', () => {
    const { rerender } = render(<CallControls {...base} />);
    const mute = screen.getByRole('button', { name: 'mute' });
    expect(mute).toHaveAttribute('aria-pressed', 'false');
    expect(mute).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+M Meta+Shift+M');
    expect(screen.getByRole('button', { name: 'deafen' })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+D Meta+Shift+D');
    expect(screen.getByRole('button', { name: 'camera' })).not.toHaveAttribute('aria-keyshortcuts');
    expect(screen.getByRole('button', { name: 'leave' })).not.toHaveAttribute('aria-pressed');
    rerender(<CallControls {...base} muted deafened camera sharing />);
    for (const name of ['mute', 'deafen', 'camera', 'share screen']) {
      expect(screen.getByRole('button', { name })).toHaveAttribute('aria-pressed', 'true');
    }
  });

  // web3.8 (R3): the stage toolbar keeps its own mute and deafen, but the user panel's carry the shortcuts, so the
  // stage's tip still shows them and its buttons carry no aria-keyshortcuts (one element per shortcut).
  it('keyshortcuts={false}: no aria-keyshortcuts and no -keys description on mute and deafen, and the tip still shows the shortcut', async () => {
    const shortcuts = { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' };
    const { rerender } = render(<CallControls {...base} shortcuts={shortcuts} keyshortcuts={false} />);
    for (const name of ['mute', 'deafen']) {
      const b = screen.getByRole('button', { name });
      expect(b).not.toHaveAttribute('aria-keyshortcuts');
      expect(b.getAttribute('aria-describedby') ?? '').not.toContain('-keys');
    }
    const bar = screen.getByRole('toolbar', { name: 'call controls' });
    expect(bar.querySelector('[id$="-keys"]')).toBeNull();
    await userEvent.keyboard('{Tab}');
    const tip = bar.querySelector<HTMLElement>('[role="tooltip"]');
    expect(tip).toBeVisible();
    expect(tip).toHaveTextContent('mute');
    expect(tip).toHaveTextContent('Ctrl+Shift+M');
    // The default (control): the shortcuts are carried and described.
    rerender(<CallControls {...base} shortcuts={shortcuts} />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-keyshortcuts', 'Control+Shift+M Meta+Shift+M');
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAccessibleDescription('Ctrl+Shift+M');
  });

  // Voice controls as icons (plan 2026-10-15, VI-1): the name is the label via aria-label, the icon is decorative,
  // and the voice toolbar drops the Button primitive's [x] / [ ] mark; the swapped glyph carries the state instead.
  it('each control is an icon button named by its label, with no [x] mark', () => {
    render(<CallControls {...base} muted />);
    for (const name of ['mute', 'deafen', 'camera', 'share screen', 'leave']) {
      const b = screen.getByRole('button', { name });
      expect(b).toHaveAttribute('aria-label', name);
      expect(b.querySelectorAll('svg[aria-hidden="true"]')).toHaveLength(1);
      expect(b.textContent).not.toMatch(/\[[x ]\]/);
    }
  });

  it('a toggle swaps its glyph with its state, and leave is always phone-off', () => {
    const icon = (name: string) => screen.getByRole('button', { name }).querySelector('svg')?.getAttribute('data-icon');
    const { rerender } = render(<CallControls {...base} />);
    expect(['mute', 'deafen', 'camera', 'share screen', 'leave'].map(icon)).toEqual(['mic', 'headphones', 'video-off', 'monitor-up', 'phone-off']);
    rerender(<CallControls {...base} muted deafened camera sharing />);
    expect(['mute', 'deafen', 'camera', 'share screen', 'leave'].map(icon)).toEqual(['mic-off', 'headphone-off', 'video', 'monitor-x', 'phone-off']);
  });

  it('pressed toggles carry a tint: danger for mute and deafen, accent for camera and share; leave is the danger variant', () => {
    render(<CallControls {...base} muted deafened camera sharing />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('data-tint', 'danger');
    expect(screen.getByRole('button', { name: 'deafen' })).toHaveAttribute('data-tint', 'danger');
    expect(screen.getByRole('button', { name: 'camera' })).toHaveAttribute('data-tint', 'accent');
    expect(screen.getByRole('button', { name: 'share screen' })).toHaveAttribute('data-tint', 'accent');
    expect(screen.getByRole('button', { name: 'leave' })).toHaveAttribute('data-variant', 'danger');
  });

  // Review round 1 (#2, #3): one tooltip per toolbar, a sibling of the buttons, never part of a button's hit area.
  const shortcuts = { mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' };
  const tipOf = () => screen.getByRole('toolbar', { name: 'call controls' }).querySelector<HTMLElement>('[role="tooltip"]');

  it('one tooltip, outside every button: the label and shortcut on keyboard focus; only the shortcut describes', async () => {
    render(<CallControls {...base} shortcuts={shortcuts} />);
    const mute = screen.getByRole('button', { name: 'mute' });
    expect(screen.getByRole('toolbar', { name: 'call controls' }).querySelectorAll('[role="tooltip"]')).toHaveLength(1);
    expect(tipOf()).not.toBeVisible();
    await userEvent.keyboard('{Tab}');
    expect(mute).toHaveFocus();
    expect(tipOf()).toBeVisible();
    expect(tipOf()).toHaveTextContent('mute');
    expect(tipOf()).toHaveTextContent('Ctrl+Shift+M');
    for (const b of screen.getAllByRole('button')) expect(b.contains(tipOf())).toBe(false);
    expect(tipOf()).toHaveAttribute('data-for', 'mute');
    // A tip shown by focus never takes the pointer.
    expect(tipOf()).toHaveAttribute('data-origin', 'focus');
    expect(mute).toHaveAccessibleDescription('Ctrl+Shift+M');
    expect(mute).toHaveAccessibleName('mute');
    expect(mute).not.toHaveAttribute('title');
    await userEvent.keyboard('{ArrowRight}{ArrowRight}');
    const camera = screen.getByRole('button', { name: 'camera' });
    expect(tipOf()).toHaveAttribute('data-for', 'camera');
    expect(tipOf()).toHaveTextContent(/^camera$/);
    expect(camera).not.toHaveAttribute('aria-describedby');
  });

  it('Escape hides a visible tip wherever focus is, is marked handled, and leaves nothing else', async () => {
    const onLeave = vi.fn();
    const outer = vi.fn();
    render(<><input aria-label="message" onKeyDown={e => { outer(e.key); }} /><CallControls {...base} onLeave={onLeave} shortcuts={shortcuts} /></>);
    const field = screen.getByRole('textbox', { name: 'message' });
    field.focus();
    await userEvent.hover(screen.getByRole('button', { name: 'deafen' }));
    expect(tipOf()).toBeVisible();
    expect(tipOf()).toHaveAttribute('data-for', 'deafen');
    const esc = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    act(() => { field.dispatchEvent(esc); });
    expect(esc.defaultPrevented).toBe(true);
    expect(tipOf()).not.toBeVisible();
    expect(field).toHaveFocus();
    expect(outer).not.toHaveBeenCalled();
    expect(onLeave).not.toHaveBeenCalled();
    // With no tip showing, Escape is not touched.
    const esc2 = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    act(() => { field.dispatchEvent(esc2); });
    expect(esc2.defaultPrevented).toBe(false);
    expect(outer).toHaveBeenCalledWith('Escape');
    // The next hover shows it again.
    await userEvent.unhover(screen.getByRole('button', { name: 'deafen' }));
    await userEvent.hover(screen.getByRole('button', { name: 'deafen' }));
    expect(tipOf()).toBeVisible();
  });

  it('Escape on a focused control hides its tip; focus stays and the arrows still rove', async () => {
    render(<CallControls {...base} shortcuts={shortcuts} />);
    await userEvent.keyboard('{Tab}');
    await userEvent.keyboard('{Escape}');
    expect(tipOf()).not.toBeVisible();
    expect(screen.getByRole('button', { name: 'mute' })).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('button', { name: 'deafen' })).toHaveFocus();
    expect(tipOf()).toBeVisible();
    expect(tipOf()).toHaveAttribute('data-for', 'deafen');
  });

  it('hoverable: the pointer can move from the control onto its tip and the tip stays; leaving both hides it', async () => {
    // One user-event instance, so each move leaves the element before it (pointerout and pointerleave).
    const user = userEvent.setup();
    render(<><p>elsewhere</p><CallControls {...base} shortcuts={shortcuts} /></>);
    const mute = screen.getByRole('button', { name: 'mute' });
    await user.hover(mute);
    expect(tipOf()).toBeVisible();
    expect(tipOf()).toHaveAttribute('data-origin', 'hover');
    await user.hover(tipOf()!);
    expect(tipOf()).toBeVisible();
    expect(tipOf()).toHaveAttribute('data-for', 'mute');
    await user.hover(screen.getByText('elsewhere'));
    expect(tipOf()).not.toBeVisible();
  });

  it('one tip at a time: the most recent hover or focus wins', async () => {
    render(<CallControls {...base} shortcuts={shortcuts} />);
    await userEvent.keyboard('{Tab}');
    expect(tipOf()).toHaveAttribute('data-for', 'mute');
    await userEvent.hover(screen.getByRole('button', { name: 'camera' }));
    expect(tipOf()).toHaveAttribute('data-for', 'camera');
    await userEvent.keyboard('{ArrowRight}');
    expect(tipOf()).toHaveAttribute('data-for', 'deafen');
    await userEvent.unhover(screen.getByRole('button', { name: 'camera' }));
    expect(tipOf()).toHaveAttribute('data-for', 'deafen');
  });

  it('a click on a neighbour while a tip shows reaches that neighbour; a click never shows a focus tip', async () => {
    const onShare = vi.fn();
    const onLeave = vi.fn();
    render(<CallControls {...base} onShare={onShare} onLeave={onLeave} />);
    await userEvent.keyboard('{Tab}{End}');
    expect(tipOf()).toHaveAttribute('data-for', 'leave');
    await userEvent.click(screen.getByRole('button', { name: 'share screen' }));
    expect(onShare).toHaveBeenCalledTimes(1);
    expect(onLeave).not.toHaveBeenCalled();
    // The click focused share, but a pointer focus is not a keyboard focus: the tip is the hover's.
    expect(tipOf()).toHaveAttribute('data-origin', 'hover');
    await userEvent.unhover(screen.getByRole('button', { name: 'share screen' }));
    expect(tipOf()).not.toBeVisible();
  });

  it('a touch never pins a tip', async () => {
    const onMute = vi.fn();
    const user = userEvent.setup();
    render(<CallControls {...base} onMute={onMute} shortcuts={shortcuts} />);
    await user.pointer({ keys: '[TouchA]', target: screen.getByRole('button', { name: 'mute' }) });
    expect(onMute).toHaveBeenCalledTimes(1);
    expect(tipOf()).not.toBeVisible();
  });

  // Jonas, 2026-10-15: a disabled camera or share says why, in its tooltip and as its description; the name is unchanged.
  describe('a disabled control says why', () => {
    const why = { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' };
    const withWhy = { ...base, labels: { ...labels, why } };
    const tipOf = () => screen.getByRole('toolbar', { name: 'call controls' }).querySelector<HTMLElement>('[role="tooltip"]');
    const focusOn = async (name: string) => {
      await userEvent.keyboard('{Tab}');
      const order = ['mute', 'deafen', 'camera', 'share screen', 'leave'];
      for (let i = 0; i < order.indexOf(name); i += 1) await userEvent.keyboard('{ArrowRight}');
      expect(screen.getByRole('button', { name })).toHaveFocus();
    };

    it('paused: camera and share say "paused" in the tip and the description', async () => {
      render(<CallControls {...withWhy} paused canCamera={false} canShare={false} />);
      for (const name of ['camera', 'share screen']) {
        const b = screen.getByRole('button', { name });
        expect(b).toHaveAccessibleName(name);
        expect(b).toHaveAccessibleDescription('paused');
      }
      await focusOn('camera');
      expect(tipOf()).toHaveTextContent(/^camera — paused$/);
      await userEvent.keyboard('{ArrowRight}');
      expect(tipOf()).toHaveTextContent(/^share screen — paused$/);
    });

    it('joining comes first: busy says "connecting", even while paused', async () => {
      render(<CallControls {...withWhy} busy paused canCamera={false} canShare={false} />);
      expect(screen.getByRole('button', { name: 'camera' })).toHaveAccessibleDescription('connecting');
      expect(screen.getByRole('button', { name: 'share screen' })).toHaveAccessibleDescription('connecting');
      await focusOn('camera');
      expect(tipOf()).toHaveTextContent(/^camera — connecting$/);
    });

    it('without screen capture, share says so and camera, enabled, says nothing', async () => {
      render(<CallControls {...withWhy} canShare={false} />);
      const share = screen.getByRole('button', { name: 'share screen' });
      expect(share).toHaveAccessibleName('share screen');
      expect(share).toHaveAccessibleDescription('not available in this browser');
      const camera = screen.getByRole('button', { name: 'camera' });
      expect(camera).not.toHaveAttribute('aria-describedby');
      await focusOn('camera');
      expect(tipOf()).toHaveTextContent(/^camera$/);
      await userEvent.keyboard('{ArrowRight}');
      expect(tipOf()).toHaveTextContent(/^share screen — not available in this browser$/);
    });

    it('a shortcut and no reason stay as they were; with no reason labels, nothing is said', () => {
      const { rerender } = render(<CallControls {...withWhy} shortcuts={{ mute: 'Ctrl+Shift+M', deafen: 'Ctrl+Shift+D' }} />);
      expect(screen.getByRole('button', { name: 'mute' })).toHaveAccessibleDescription('Ctrl+Shift+M');
      for (const name of ['camera', 'share screen', 'leave']) expect(screen.getByRole('button', { name })).not.toHaveAttribute('aria-describedby');
      rerender(<CallControls {...base} paused canCamera={false} canShare={false} />);
      expect(screen.getByRole('button', { name: 'camera' })).not.toHaveAttribute('aria-describedby');
    });
  });

  it('leave is set apart from the toggles by a divider, in the same roving order', async () => {
    render(<CallControls {...base} />);
    const bar = screen.getByRole('toolbar', { name: 'call controls' });
    const divider = bar.querySelector('.d-voice-toolbar__divider');
    expect(divider).toHaveAttribute('aria-hidden', 'true');
    expect(divider?.nextElementSibling).toBe(screen.getByRole('button', { name: 'leave' }));
    await userEvent.keyboard('{Tab}{ArrowLeft}');
    expect(screen.getByRole('button', { name: 'leave' })).toHaveFocus();
    await userEvent.keyboard('{ArrowLeft}');
    expect(screen.getByRole('button', { name: 'share screen' })).toHaveFocus();
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><CallControls {...(meta.args as CallControlsProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  describe('Signal §5.6: the call bar (labels.bar)', () => {
    const bar = { mute: 'Mute', unmute: 'Unmute', deafen: 'Deafen', undeafen: 'Undeafen', camera: 'Camera', share: 'Share', stopShare: 'Stop sharing', leave: 'Leave' };
    const withBar = { ...base, labels: { ...labels, bar }, shortcuts: { mute: '⌘⇧M', deafen: '⌘⇧D' } };

    it('each control is named by its word; mute and deafen show their keycaps; leave has its word', () => {
      const { container } = render(<CallControls {...withBar} />);
      for (const name of ['Mute', 'Deafen', 'Camera', 'Share', 'Leave']) expect(screen.getByRole('button', { name: new RegExp(`^${name}`) })).toBeInTheDocument();
      expect([...container.querySelectorAll('.d-voice-toolbar__keycap')].map(k => k.textContent)).toEqual(['⌘⇧M', '⌘⇧D']);
      expect(container.querySelector('.d-call-controls--bar')).not.toBeNull();
    });

    it('mute, deafen and share say their state by their word with no aria-pressed; camera keeps its word and aria-pressed', () => {
      const { container, rerender } = render(<CallControls {...withBar} />);
      const ctl = (id: string) => container.querySelector<HTMLButtonElement>(`[data-control="${id}"]`)!;
      for (const id of ['mute', 'deafen', 'share']) expect(ctl(id)).not.toHaveAttribute('aria-pressed');
      expect(ctl('camera')).toHaveAttribute('aria-pressed', 'false');
      rerender(<CallControls {...withBar} muted deafened camera sharing />);
      expect(ctl('mute')).toHaveTextContent('Unmute');
      expect(ctl('deafen')).toHaveTextContent('Undeafen');
      expect(ctl('share')).toHaveTextContent('Stop sharing');
      expect(ctl('camera')).toHaveAttribute('aria-pressed', 'true');
      for (const id of ['mute', 'deafen', 'camera', 'share']) expect(ctl(id)).toHaveAttribute('data-on', 'true');
    });

    it('deafened shows mute on too (mic-off, Unmute), as the prototype', () => {
      const { container } = render(<CallControls {...withBar} deafened />);
      const mute = container.querySelector('[data-control="mute"]')!;
      expect(mute).toHaveTextContent('Unmute');
      expect(mute.querySelector('svg')?.getAttribute('data-icon')).toBe('mic-off');
    });

    it('share keeps the monitor-up glyph while live (the prototype), and the label tooltip names the word', () => {
      const { container } = render(<CallControls {...withBar} sharing />);
      expect(container.querySelector('[data-control="share"] svg')?.getAttribute('data-icon')).toBe('monitor-up');
    });
  });
});
