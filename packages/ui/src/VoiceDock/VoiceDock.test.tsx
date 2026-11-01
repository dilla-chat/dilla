import { readFileSync } from 'node:fs';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { VoiceDock, type VoiceDockProps } from './VoiceDock.tsx';
import meta from './VoiceDock.stories.tsx';

const css = readFileSync(`${process.cwd()}/src/VoiceDock/VoiceDock.css`, 'utf8');

const dock: VoiceDockProps = {
  phase: 'connected', where: 'harbour', muted: false, deafened: false, announcement: '',
  labels: {
    region: 'voice controls', connecting: 'connecting', where: 'harbour · Midgard Crew', reconnecting: 'reconnecting',
    paused: 'paused', mute: 'mute', deafen: 'deafen', leave: 'leave', camera: 'camera', share: 'share screen',
    ping: '{ms} ms', pingSub: '<1 ms', pingNone: '— ms', rate: '↑ {up} ↓ {down} kb/s',
    stats: 'Ping {ms} milliseconds, {quality}. Sending {up}, receiving {down} kilobits per second.',
    quality: { good: 'good', poor: 'poor', bad: 'bad' },
  },
  onCamera: () => {}, onShare: () => {}, onLeave: () => {},
};
const link = { rttMs: 24, upKbps: 48, downKbps: 96, quality: 'good' } as const;

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('VoiceDock (MP5)', () => {
  it('line 1 is the state dot and the channel name; the server is its tooltip and part of its accessible name', () => {
    const { container } = render(<VoiceDock {...dock} onStatus={() => {}} />);
    expect(container.querySelector('.d-voice-dock__text')).toHaveTextContent(/^harbour$/);
    expect(screen.queryByText(/voice connected/)).toBeNull();
    const line1 = screen.getByRole('button', { name: 'harbour · Midgard Crew' });
    expect(line1).toHaveAttribute('title', 'harbour · Midgard Crew');
    expect(line1).not.toHaveAttribute('aria-label'); // its name comes from its content: the visible name is aria-hidden, the full one is sr-only
  });

  it('without labels.where the channel is the whole name; as plain text the server is still in the tooltip', () => {
    const { container, rerender } = render(<VoiceDock {...dock} labels={{ ...dock.labels, where: undefined }} onStatus={() => {}} />);
    expect(screen.getByRole('button', { name: 'harbour' })).toBeInTheDocument();
    rerender(<VoiceDock {...dock} />);
    expect(container.querySelector('.d-voice-dock__line1')).toHaveAttribute('title', 'harbour · Midgard Crew');
  });

  it('line 2 reads the ping and the rates, e.g. "24 ms · ↑ 48 ↓ 96 kb/s"', () => {
    const { container } = render(<VoiceDock {...dock} stats={link} />);
    expect(container.querySelector('.d-voice-dock__where')).toHaveTextContent('24 ms · ↑ 48 ↓ 96 kb/s');
  });

  it('FD1: a ping that rounds to 0 reads "<1 ms", not "0 ms"', () => {
    const { container } = render(<VoiceDock {...dock} stats={{ ...link, rttMs: 0 }} />);
    expect(container.querySelector('.d-voice-dock__where')).toHaveTextContent('<1 ms · ↑ 48 ↓ 96 kb/s');
  });

  it('L5: a direction that could not be read is a dash, never "null" or 0', () => {
    const { container } = render(<VoiceDock {...dock} stats={{ ...link, upKbps: null }} />);
    expect(container.querySelector('.d-voice-dock__where')).toHaveTextContent('24 ms · ↑ — ↓ 96 kb/s');
    expect(container.querySelector('.d-voice-dock__stats')).not.toHaveTextContent('null');
  });

  it('the stats name their quality for a screen reader and carry it as data, never as colour alone', () => {
    const { container, rerender } = render(<VoiceDock {...dock} stats={link} />);
    const stats = container.querySelector('.d-voice-dock__stats')!;
    expect(stats).toHaveAttribute('data-quality', 'good');
    expect(stats).toHaveTextContent('Ping 24 milliseconds, good. Sending 48, receiving 96 kilobits per second.');
    expect(stats.querySelector('.d-sr-only')).not.toBeNull();
    rerender(<VoiceDock {...dock} stats={{ rttMs: 200, upKbps: 1, downKbps: 2, quality: 'poor' }} />);
    expect(container.querySelector('.d-voice-dock__stats')).toHaveAttribute('data-quality', 'poor');
    expect(container.querySelector('.d-voice-dock__stats')).toHaveTextContent('Ping 200 milliseconds, poor.');
    rerender(<VoiceDock {...dock} stats={{ rttMs: 400, upKbps: 1, downKbps: 2, quality: 'bad' }} />);
    expect(container.querySelector('.d-voice-dock__stats')).toHaveTextContent('Ping 400 milliseconds, bad.');
  });

  it('before the first sample line 2 reads "— ms"', () => {
    const { container } = render(<VoiceDock {...dock} />);
    expect(container.querySelector('.d-voice-dock__where')).toHaveTextContent(/^— ms$/);
    expect(container.querySelector('.d-voice-dock__stats')).toHaveAttribute('data-quality', 'none');
  });

  it('while not connected line 2 shows the phase words in place of the stats', () => {
    const { container, rerender } = render(<VoiceDock {...dock} phase="connecting" stats={link} />);
    const line2 = () => container.querySelector('.d-voice-dock__where');
    expect(line2()).toHaveTextContent(/^connecting$/);
    rerender(<VoiceDock {...dock} phase="reconnecting" stats={link} />);
    expect(line2()).toHaveTextContent(/^reconnecting$/);
    rerender(<VoiceDock {...dock} phase="paused" stats={link} />);
    expect(line2()).toHaveTextContent(/^paused$/);
    expect(container.querySelector('.d-voice-dock__stats')).toBeNull();
  });

  it('the stats are never in the status region and use tabular numerals (CSS pin)', () => {
    render(<VoiceDock {...dock} stats={link} announcement="" />);
    expect(screen.getByRole('status')).toHaveTextContent(/^$/);
    expect(css).toMatch(/\.d-voice-dock__stats \{[^}]*font-variant-numeric: tabular-nums/);
    expect(css).toMatch(/\.d-voice-dock__stats\[data-quality="poor"\] \{[^}]*var\(--warn\)/);
    expect(css).toMatch(/\.d-voice-dock__stats\[data-quality="bad"\] \{[^}]*var\(--danger\)/);
  });

  it('draws no meter (the legacy `meter` prop is gone, WEB-DOCK)', () => {
    const { container } = render(<VoiceDock {...dock} />);
    expect(container.querySelector('.d-voice-dock__meter, .d-audio-meter, [role="meter"]')).toBeNull();
  });

  it('the status is a button calling onStatus (it opens Diagnostics, R26)', async () => {
    const onStatus = vi.fn();
    render(<VoiceDock {...dock} onStatus={onStatus} />);
    await userEvent.click(screen.getByRole('button', { name: 'harbour · Midgard Crew' }));
    expect(onStatus).toHaveBeenCalledTimes(1);
  });

  it('without onStatus the channel is plain text, not a button', () => {
    render(<VoiceDock {...dock} />);
    expect(screen.queryByRole('button', { name: /harbour/ })).toBeNull();
    expect(document.querySelector('.d-voice-dock__text')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByText('harbour · Midgard Crew')).toHaveClass('d-sr-only'); // the server is read in plain text too
  });

  it('camera, share and leave are 28px icon buttons in one toolbar, each with a tooltip anchor; there is no mute or deafen', () => {
    const { container } = render(<VoiceDock {...dock} onMute={() => {}} onDeafen={() => {}} />);
    const group = screen.getByRole('toolbar', { name: 'voice controls' });
    const buttons = within(group).getAllByRole('button');
    expect(buttons.map(b => b.getAttribute('aria-label'))).toEqual(['camera', 'share screen', 'leave']);
    for (const b of buttons) {
      expect(b).toHaveClass('d-icon-button');
      expect(b).toHaveAttribute('data-size', '28');
      expect(b.parentElement).toHaveClass('d-tooltip-anchor');
    }
    expect(screen.queryByRole('button', { name: 'mute' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'deafen' })).toBeNull();
    expect(container.querySelector('.d-voice-toolbar')).toBeNull();
  });

  it('is one toolbar with one Tab stop; ArrowLeft/Right wrap, Home/End go to the ends, Tab leaves in one stop', async () => {
    render(<><VoiceDock {...dock} /><button type="button">after</button></>);
    const bar = screen.getByRole('toolbar', { name: 'voice controls' });
    const name = (n: string) => within(bar).getByRole('button', { name: n });
    expect(within(bar).getAllByRole('button').filter(b => b.tabIndex === 0)).toHaveLength(1);
    await userEvent.keyboard('{Tab}');
    expect(name('camera')).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(name('share screen')).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}{ArrowRight}');
    expect(name('camera')).toHaveFocus();
    await userEvent.keyboard('{ArrowLeft}');
    expect(name('leave')).toHaveFocus();
    await userEvent.keyboard('{Home}');
    expect(name('camera')).toHaveFocus();
    await userEvent.keyboard('{End}');
    expect(name('leave')).toHaveFocus();
    // The stop follows focus: Tab out and Shift+Tab back lands on leave.
    await userEvent.keyboard('{Tab}');
    expect(screen.getByRole('button', { name: 'after' })).toHaveFocus();
    await userEvent.keyboard('{Shift>}{Tab}{/Shift}');
    expect(name('leave')).toHaveFocus();
  });

  it('the tooltip shows on focus while roving, and Alt/Ctrl/Meta/Shift arrows are left alone', async () => {
    render(<VoiceDock {...dock} />);
    await userEvent.keyboard('{Tab}{ArrowRight}');
    expect(screen.getByRole('tooltip')).toHaveTextContent('share screen');
    await userEvent.keyboard('{Control>}{ArrowRight}{/Control}');
    expect(screen.getByRole('button', { name: 'share screen' })).toHaveFocus();
  });

  it('an inert camera stays in the roving order and keeps its description; activating it does nothing', async () => {
    const why = { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' };
    const onCamera = vi.fn();
    render(<VoiceDock {...dock} labels={{ ...dock.labels, why }} canCamera={false} paused onCamera={onCamera} />);
    const bar = screen.getByRole('toolbar', { name: 'voice controls' });
    const camera = within(bar).getByRole('button', { name: 'camera' });
    expect(camera).toHaveAttribute('aria-disabled', 'true');
    expect(camera).toHaveAccessibleDescription('paused');
    await userEvent.keyboard('{Tab}');
    expect(camera).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}{ArrowRight}');
    expect(within(bar).getByRole('button', { name: 'leave' })).toHaveFocus();
    await userEvent.keyboard('{ArrowRight}');
    expect(camera).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(onCamera).not.toHaveBeenCalled();
    expect(screen.getByRole('tooltip')).toHaveTextContent('camera');
  });

  it('a keyboard-focused control shows its tooltip', async () => {
    render(<VoiceDock {...dock} />);
    await userEvent.keyboard('{Tab}');
    expect(screen.getByRole('button', { name: 'camera' })).toHaveFocus();
    expect(screen.getByRole('tooltip')).toHaveTextContent('camera');
  });

  it('without camera and share handlers (or their labels) the dock keeps leave only', () => {
    render(<VoiceDock {...dock} onCamera={undefined} onShare={undefined} />);
    expect(within(screen.getByRole('toolbar', { name: 'voice controls' })).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual(['leave']);
  });

  it.each([['connecting', 'span.d-state-dot[data-tone="pending"]'], ['reconnecting', 'span.d-state-dot[data-tone="pending"]'],
    ['connected', 'span.d-voice-dock__bars'], ['paused', 'svg[data-icon="pause"]']] as const)('the %s phase marker is drawn, not a font glyph', (phase, sel) => {
    const { container } = render(<VoiceDock {...dock} phase={phase} />);
    expect(container.querySelector(`.d-voice-dock__glyph ${sel}`)).not.toBeNull();
    expect(container.querySelector('.d-voice-dock__glyph')?.textContent).toBe('');
  });

  it('with controls={false} there is no toolbar, but the status, where and the status region stay', () => {
    render(<VoiceDock {...dock} controls={false} announcement="Joined harbour." />);
    expect(screen.queryByRole('toolbar')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.getByText('harbour · Midgard Crew')).toBeInTheDocument();
    expect(screen.getByText('— ms')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('Joined harbour.');
  });

  it('a "{where}" in the paused words is still filled', () => {
    render(<VoiceDock {...dock} phase="paused" labels={{ ...dock.labels, paused: 'paused in {where}' }} />);
    expect(screen.getByText('paused in harbour')).toBeInTheDocument();
  });

  it('camera, share and leave call their handlers', async () => {
    const handlers = { onCamera: vi.fn(), onShare: vi.fn(), onLeave: vi.fn() };
    render(<VoiceDock {...dock} {...handlers} />);
    for (const b of within(screen.getByRole('toolbar', { name: 'voice controls' })).getAllByRole('button')) await userEvent.click(b);
    for (const fn of Object.values(handlers)) expect(fn).toHaveBeenCalledTimes(1);
  });

  it('camera and share are pressed toggles whose glyph follows the state', () => {
    const icon = (name: string) => screen.getByRole('button', { name }).querySelector('svg')?.getAttribute('data-icon');
    const { rerender } = render(<VoiceDock {...dock} />);
    expect(screen.getByRole('button', { name: 'camera' })).toHaveAttribute('aria-pressed', 'false');
    expect(['camera', 'share screen', 'leave'].map(icon)).toEqual(['video-off', 'monitor-up', 'phone-off']);
    rerender(<VoiceDock {...dock} camera sharing />);
    expect(screen.getByRole('button', { name: 'camera' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'share screen' })).toHaveAttribute('aria-pressed', 'true');
    expect(['camera', 'share screen', 'leave'].map(icon)).toEqual(['video', 'monitor-x', 'phone-off']);
  });

  it('camera and share are inert under the stage rules: no camera, no capture, or joining (busy); leave stays live', async () => {
    const onCamera = vi.fn();
    const onShare = vi.fn();
    const { rerender } = render(<VoiceDock {...dock} canCamera={false} canShare={false} onCamera={onCamera} onShare={onShare} />);
    for (const name of ['camera', 'share screen']) {
      const b = screen.getByRole('button', { name });
      expect(b).toHaveAttribute('aria-disabled', 'true');
      expect(b).not.toBeDisabled();
      await userEvent.click(b);
    }
    expect(onCamera).not.toHaveBeenCalled();
    expect(onShare).not.toHaveBeenCalled();
    rerender(<VoiceDock {...dock} busy onCamera={onCamera} onShare={onShare} />);
    expect(screen.getByRole('button', { name: 'camera' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'share screen' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'leave' })).not.toHaveAttribute('aria-disabled');
    rerender(<VoiceDock {...dock} onCamera={onCamera} onShare={onShare} />);
    for (const name of ['camera', 'share screen']) expect(screen.getByRole('button', { name })).not.toHaveAttribute('aria-disabled');
  });

  it('a disabled camera or share says why: connecting, then paused, then not available; leave never', () => {
    const why = { paused: 'paused', joining: 'connecting', noShare: 'not available in this browser' };
    const withWhy: VoiceDockProps = { ...dock, labels: { ...dock.labels, why } };
    const { rerender } = render(<VoiceDock {...withWhy} phase="paused" paused canCamera={false} canShare={false} />);
    const b = (name: string) => screen.getByRole('button', { name });
    expect(b('camera')).toHaveAccessibleName('camera');
    expect(b('camera')).toHaveAccessibleDescription('paused');
    expect(b('share screen')).toHaveAccessibleDescription('paused');
    expect(b('leave')).not.toHaveAttribute('aria-describedby');
    rerender(<VoiceDock {...withWhy} phase="connecting" busy canCamera canShare />);
    expect(b('camera')).toHaveAccessibleDescription('connecting');
    rerender(<VoiceDock {...withWhy} canShare={false} />);
    expect(b('camera')).not.toHaveAttribute('aria-describedby');
    expect(b('share screen')).toHaveAccessibleDescription('not available in this browser');
    rerender(<VoiceDock {...withWhy} />);
    for (const name of ['camera', 'share screen']) expect(b(name)).not.toHaveAttribute('aria-describedby');
  });

  it('is the Signal card: the --live tints, radius 8px, padding 10px, margin 0 8px 8px, rising in over 340ms (CSS pin, §5.4)', () => {
    const rule = css.match(/^\.d-voice-dock \{([^}]*)\}/m)![1]!;
    expect(rule).toMatch(/background: var\(--ok-soft\);/);
    expect(rule).toMatch(/border: 1px solid var\(--ok-soft-border\);/);
    expect(rule).toMatch(/border-radius: var\(--r-lg\);/);
    expect(rule).toMatch(/padding: 10px;/);
    expect(rule).toMatch(/margin: 0 8px 8px;/);
    expect(rule).toMatch(/animation: d-up var\(--duration-dock\) var\(--ease-out\) both;/);
    expect(css).toMatch(/\.d-voice-dock__line1 \{[^}]*font-size: var\(--text-ui\);[^}]*color: var\(--ok-text\);/);
    expect(css).toMatch(/\.d-voice-dock \.d-voice-dock__leave:hover \{ color: var\(--danger-ink\); background: var\(--danger\); \}/);
  });

  it('the controls are one flex row (CSS pin)', () => {
    expect(css).toMatch(/\.d-voice-dock__controls \{[^}]*display: flex/);
  });

  it('V3: camera, share and leave sit together at the right end (the status takes the free width), with no auto margin on leave (CSS pin)', () => {
    expect(css).toMatch(/\.d-voice-dock__status \{[^}]*flex: 1;/);
    expect(css).toMatch(/\.d-voice-dock__controls \{[^}]*flex: none;/);
    expect(css).not.toMatch(/margin-left: auto/);
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe with all controls in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><VoiceDock {...dock} onStatus={() => {}} camera canShare={false} /></div>);
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><VoiceDock {...(meta.args as VoiceDockProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
});
