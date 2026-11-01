import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { IconButton } from './IconButton.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const cssText = () => readFileSync(join(dirname(fileURLToPath(import.meta.url)), './IconButton.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
const baseText = () => readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../styles/base.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

describe('IconButton', () => {
  it('is a button named by its label, with a decorative icon', () => {
    const { container } = render(<IconButton icon="settings" label="open settings" />);
    const btn = screen.getByRole('button', { name: 'open settings' });
    expect(btn).toHaveAttribute('type', 'button');
    expect(container.querySelector('svg')).toHaveAttribute('aria-hidden', 'true');
    expect(container.querySelector('svg')).toHaveAttribute('data-icon', 'settings');
  });

  it('calls onClick', async () => {
    const onClick = vi.fn();
    render(<IconButton icon="plus" label="add" onClick={onClick} />);
    await userEvent.click(screen.getByRole('button', { name: 'add' }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('pressed sets aria-pressed both ways; a plain button has none', () => {
    const { rerender } = render(<IconButton icon="mic" label="mute" pressed={false} />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'false');
    rerender(<IconButton icon="mic" label="mute" pressed />);
    expect(screen.getByRole('button', { name: 'mute' })).toHaveAttribute('aria-pressed', 'true');
    rerender(<IconButton icon="mic" label="mute" />);
    expect(screen.getByRole('button', { name: 'mute' })).not.toHaveAttribute('aria-pressed');
  });

  it('keys set aria-keyshortcuts', () => {
    render(<IconButton icon="search" label="search" keys={['Ctrl', 'K']} />);
    expect(screen.getByRole('button', { name: 'search' })).toHaveAttribute('aria-keyshortcuts', 'Control+K');
  });

  it('data-size follows size, default 32', () => {
    const { rerender } = render(<IconButton icon="plus" label="add" />);
    expect(screen.getByRole('button', { name: 'add' })).toHaveAttribute('data-size', '32');
    for (const size of [28, 40, 44] as const) {
      rerender(<IconButton icon="plus" label="add" size={size} />);
      expect(screen.getByRole('button', { name: 'add' })).toHaveAttribute('data-size', String(size));
    }
  });

  it('tooltip={false} renders no tooltip anchor and no description', async () => {
    const { container } = render(<IconButton icon="plus" label="add" keys={['N']} tooltip={false} />);
    expect(container.querySelector('.d-tooltip-anchor')).toBeNull();
    await userEvent.tab();
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('shows its label as a tooltip on keyboard focus', async () => {
    render(<IconButton icon="plus" label="add" keys={['N']} />);
    await userEvent.tab();
    expect(screen.getByRole('tooltip')).toHaveTextContent('add');
  });

  it('has no axe violations, plain and pressed', async () => {
    const { container } = render(<><IconButton icon="mic" label="mute" pressed /><IconButton icon="plus" label="add" tooltip={false} /></>);
    await expectNoAxeViolations(container);
  });

  describe('CSS pins', () => {
    it('sets the four sizes as square boxes and draws pressed as an accent glyph and a 2px bottom bar', () => {
      for (const size of [28, 32, 40, 44]) expect(cssText()).toMatch(new RegExp(`\\.d-icon-button\\[data-size="${size}"\\] \\{[^}]*width: ${size / 16}rem`));
      expect(cssText()).toMatch(/\.d-icon-button\[aria-pressed="true"\] \{[^}]*color: var\(--accent\)[^}]*box-shadow: inset 0 -2px 0 0 var\(--accent\)/);
    });
    // Except in the phone header's actions, drawn at the prototype's sizes (Jonas 2026-10-27).
    it('on a phone layout the hit area is 44 whatever the size (H4), outside the phone header\'s actions', () => {
      expect(baseText()).toMatch(/\.d-app-shell\[data-layout="phone"\] :is\(\.d-btn, \.d-segmented__option, \.d-icon-button\):not\(\.d-phone-header__actions \*, \.d-channel-list__head-btn, \.d-user-panel \*\) \{ min-height: 2\.75rem; min-width: 2\.75rem; \}/);
    });
  });
});
