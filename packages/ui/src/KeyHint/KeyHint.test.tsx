import { render, screen } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';
import { KeyHint } from './KeyHint.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('KeyHint', () => {
  it('renders each key as kbd and the label as text', () => {
    render(<KeyHint keys={['⌘', 'K']} label="cmd" />);
    expect(screen.queryAllByRole('presentation')).toHaveLength(0);
    expect(screen.getByText('⌘').tagName).toBe('KBD');
    expect(screen.getByText('K').tagName).toBe('KBD');
    expect(screen.getByText('cmd')).toBeInTheDocument();
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><KeyHint keys={['/']} label="search" /></div>);
    await expectNoAxeViolations(container);
  });
});
const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

describe('KeyHint chip (UI-1)', () => {
  it('is a 22px mono keycap with a 2px bottom border in --control-edge-key (the prototype #3A404A in Signal)', () => {
    const rule = read('./KeyHint.css').match(/\.d-keyhint__kbd \{[^}]*\}/)?.[0] ?? '';
    expect(rule).toContain('font-family: var(--font-mono)');
    expect(rule).toContain('height: 1.375rem');
    expect(rule).toContain('border: 1px solid var(--control-edge-key)');
    expect(rule).toContain('border-bottom-width: 2px');
    expect(rule).toContain('border-radius: var(--r-sm)');
  });
});
