import { render, screen, within } from '@testing-library/react';
import { beforeAll, describe, it, expect } from 'vitest';
import { SettingsRow } from './SettingsRow.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-02: notify.permission.label, notify.body, notify.permission.ask, notify.default.*.
const BODY = 'Desktop notifications show while a dilla tab is open. Nothing is shown when every tab is closed.';
const DEFAULTS = [
  { value: 'dms-mentions', label: 'direct messages and mentions' }, { value: 'everything', label: 'every message' }, { value: 'nothing', label: 'nothing' },
] as const;

describe('SettingsRow', () => {
  it('is a group named by its visible label, described by its hint, holding its control', () => {
    const { container } = render(
      <SettingsRow id="notify-permission" label="desktop notifications" hint={BODY}><button type="button">turn on</button></SettingsRow>,
    );
    const group = screen.getByRole('group', { name: 'desktop notifications' });
    expect(container.firstElementChild).toBe(group);
    expect(group).toHaveClass('d-settings-row');
    expect(group).toHaveAccessibleDescription(BODY);
    expect(screen.getByText('desktop notifications')).toHaveAttribute('id', 'notify-permission-label');
    expect(within(group).getByRole('button', { name: 'turn on' })).toBeInTheDocument();
  });

  it('renders its label as a heading of the given level, still naming the group, without a heading by default', () => {
    const { rerender } = render(<SettingsRow id="a" label="passkeys" headingLevel={3}><span>x</span></SettingsRow>);
    const heading = screen.getByRole('heading', { name: 'passkeys', level: 3 });
    expect(heading).toHaveAttribute('id', 'a-label');
    expect(heading).toHaveClass('d-settings-row__label');
    expect(screen.getByRole('group', { name: 'passkeys' })).toBeInTheDocument();
    rerender(<SettingsRow id="a" label="passkeys"><span>x</span></SettingsRow>);
    expect(screen.queryByRole('heading')).toBeNull();
  });

  it('has no description without a hint', () => {
    render(<SettingsRow id="notify-default" label="notify me about"><span>x</span></SettingsRow>);
    expect(screen.getByRole('group', { name: 'notify me about' })).not.toHaveAttribute('aria-describedby');
  });

  it('has no serious axe violations around a segmented control', async () => {
    const { container } = render(
      <div className="d-root">
        <SettingsRow id="notify-default" label="notify me about">
          <Segmented id="notify-default-control" label="notify me about" options={DEFAULTS} value="dms-mentions" onChange={() => {}} />
        </SettingsRow>
      </div>,
    );
    await expectNoAxeViolations(container);
  });
});

// R17, I2: a two-column grid with a 16rem control column; on a phone the label stacks over a full-width control (H5).
// jsdom computes no layout, so these read the rules themselves (the Settings/SettingsRow stories measure the result).
describe('SettingsRow grammar (R17)', () => {
  let row = '';
  let base = '';
  beforeAll(async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    row = fs.readFileSync(`${cwd}/src/SettingsRow/SettingsRow.css`, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
    base = fs.readFileSync(`${cwd}/src/styles/base.css`, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
  });

  it('marks the row with data-row', () => {
    render(<SettingsRow id="a" label="a"><span>x</span></SettingsRow>);
    expect(screen.getByRole('group', { name: 'a' })).toHaveAttribute('data-row');
  });

  // Task 10 (README §5.9): a wrapping row of at least 56px with 10px 16px padding, drawn as a hairline box of radius 10;
  // consecutive rows overlap by 1px and square their joined corners, so a run of rows is one group.
  it('is a wrapping 56px row in a 10px-radius hairline box, and consecutive rows form one group', () => {
    const rule = /\.d-settings-row \{([^}]*)\}/.exec(row)?.[1] ?? '';
    expect(rule).toMatch(/display: flex; flex-wrap: wrap;/);
    expect(rule).toMatch(/min-height: 3\.5rem/);
    expect(rule).toMatch(/padding: 10px 16px;/);
    expect(rule).toMatch(/border: 1px solid var\(--hairline\); border-radius: var\(--r-xl\);/);
    // Fix round 2: no overlap; a group of n rows is 56n + 2 (the prototype's 56px rows in a 1px-edged box).
    expect(row).not.toMatch(/margin-top: -1px/);
    expect(row).toMatch(/\.d-settings-row:has\(\+ \.d-settings-row\), li:has\(\+ li\) > \.d-settings-row \{ min-height: calc\(3\.5rem \+ 1px\); border-bottom: 0;/);
    expect(row).toMatch(/\.d-settings-row \+ \.d-settings-row:has\(\+ \.d-settings-row\), li \+ li:has\(\+ li\) > \.d-settings-row \{ min-height: 3\.5rem; \}/);
  });

  it('draws the 32px icon tile and the tag when given', () => {
    render(<SettingsRow id="t" label="Noise suppression" icon="mic" tag="BETA"><span>x</span></SettingsRow>);
    expect(document.querySelector('.d-settings-row__icon [data-icon="mic"]')).toHaveAttribute('width', '15');
    expect(document.querySelector('.d-settings-row__icon')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('group', { name: 'Noise suppression BETA' })).toBeInTheDocument();
  });

  it('keeps one hairline under every row, whatever its parent, and lets a long button label grow its height', () => {
    expect(row).not.toMatch(/\.d-settings-row:last-child/);
    const button = /\.d-settings-row__control \.d-btn \{([^}]*)\}/.exec(row)?.[1] ?? '';
    expect(button).toMatch(/height: auto;/);
    expect(button).toMatch(/min-height: 2rem;/);
  });

  it('stacks the label over a full-width control only in the app-provided phone layout', () => {
    const phone = /\.d-settings-frame\[data-layout="phone"\] \.d-settings-row[^{]*\{([^}]*)\}/.exec(base)?.[1] ?? '';
    expect(phone).toMatch(/grid-template-columns: minmax\(0, 1fr\);/);
    expect(base).toMatch(/\[data-phone\] \.d-settings-row/);
    expect(base).not.toContain('@container d-settings');
  });
});
