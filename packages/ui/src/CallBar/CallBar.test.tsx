import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { CallBar, type CallBarProps } from './CallBar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels = { open: 'open call in harbour', mute: 'mute', leave: 'leave call', region: 'call' };
const props = (over: Partial<CallBarProps> = {}): CallBarProps => ({
  phase: 'connected', where: 'harbour', muted: false, labels, onOpen: () => {}, onMute: () => {}, onLeave: () => {}, ...over,
});

describe('CallBar', () => {
  it('is a named group with three named buttons', () => {
    render(<CallBar {...props()} />);
    expect(screen.getByRole('group', { name: 'call' })).toHaveClass('d-call-bar');
    expect(screen.getByRole('button', { name: 'open call in harbour' })).toHaveTextContent('harbour');
    expect(screen.getByRole('button', { name: 'mute' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'leave call' })).toBeInTheDocument();
  });
  it('exposes the mute state', () => {
    const { rerender } = render(<CallBar {...props()} />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'false');
    rerender(<CallBar {...props({ muted: true })} />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: 'mute' }).querySelector('[data-icon="mic-off"]')).not.toBeNull();
  });
  it('draws a pressed mute as off: the danger glyph on the danger tint, no accent fill', async () => {
    render(<CallBar {...props({ muted: true })} />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'true');
    // The test config reads no CSS (css: false) and the package has no Node types: read it as SettingsFrame.test does.
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    const css = fs.readFileSync(`${cwd}/src/CallBar/CallBar.css`, 'utf8');
    const rule = /\.d-call-bar \.d-call-bar__mute\[aria-pressed="true"\]\s*\{([^}]*)\}/.exec(css)?.[1] ?? '';
    // Signal: the toggled-on danger tint (the 0.1 fill, the 0.45 edge), never the accent fill.
    expect(rule).toContain('color: var(--danger)');
    expect(rule).toContain('border-color: var(--danger-border)');
    expect(rule).toContain('background: var(--danger-soft)');
    expect(rule).toContain('box-shadow: none');
  });
  it('calls the three callbacks', async () => {
    const user = userEvent.setup();
    const onOpen = vi.fn(); const onMute = vi.fn(); const onLeave = vi.fn();
    render(<CallBar {...props({ onOpen, onMute, onLeave })} />);
    await user.click(screen.getByRole('button', { name: 'open call in harbour' }));
    await user.click(screen.getByRole('button', { name: 'mute' }));
    await user.click(screen.getByRole('button', { name: 'leave call' }));
    expect([onOpen, onMute, onLeave].map(f => f.mock.calls.length)).toEqual([1, 1, 1]);
  });
  it.each([['connecting', 'pending'], ['reconnecting', 'pending'], ['connected', 'ok']] as const)('%s shows a %s dot', (phase, tone) => {
    const { container } = render(<CallBar {...props({ phase })} />);
    expect(container.querySelector('.d-state-dot')).toHaveAttribute('data-tone', tone);
  });
  it('shows the pause icon when paused', () => {
    const { container } = render(<CallBar {...props({ phase: 'paused' })} />);
    expect(container.querySelector('.d-state-dot')).toBeNull();
    expect(container.querySelector('[data-icon="pause"]')).not.toBeNull();
  });
  it('has no axe violations', async () => {
    const { container } = render(<div className="d-root"><CallBar {...props()} /></div>);
    await expectNoAxeViolations(container);
  });
});
