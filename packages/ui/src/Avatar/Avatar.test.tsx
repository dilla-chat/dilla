import { render, screen } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';
import { Avatar, AVATAR_PALETTE, avatarColour } from './Avatar.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';
import { contrastRatio } from '@dilla/design-tokens';

// HSL to RGB conversion
function hslToRgb(h: number, s: number, l: number): [number, number, number] {
  s /= 100;
  l /= 100;
  const c = (1 - Math.abs(2 * l - 1)) * s;
  const hp = h / 60;
  const x = c * (1 - Math.abs((hp % 2) - 1));
  let r = 0, g = 0, b = 0;

  if (hp < 1) [r, g, b] = [c, x, 0];
  else if (hp < 2) [r, g, b] = [x, c, 0];
  else if (hp < 3) [r, g, b] = [0, c, x];
  else if (hp < 4) [r, g, b] = [0, x, c];
  else if (hp < 5) [r, g, b] = [x, 0, c];
  else [r, g, b] = [c, 0, x];

  const m = l - c / 2;
  return [
    Math.round((r + m) * 255),
    Math.round((g + m) * 255),
    Math.round((b + m) * 255)
  ];
}

function rgbToHex(r: number, g: number, b: number): string {
  return `#${[r, g, b].map(x => x.toString(16).padStart(2, '0')).join('').toUpperCase()}`;
}

describe('Avatar', () => {
  it('derives initials and announces presence in the name', () => {
    render(<Avatar name="jonas" presence="online" />);
    const img = screen.getByRole('img', { name: 'jonas, online' });
    expect(img).toHaveTextContent('JO');
  });
  it('uses given initials and omits presence text when unknown', () => {
    render(<Avatar name="Skald" initials="SK" />);
    expect(screen.getByRole('img', { name: 'Skald' })).toHaveTextContent('SK');
  });
  it('shows presence with a glyph, not only colour', () => {
    render(<Avatar name="lina" presence="idle" />);
    expect(screen.getByRole('img', { name: 'lina, idle' }).querySelector('[data-presence="idle"]')).not.toBeNull();
  });
  it.each([['online', 'ok'], ['idle', 'idle'], ['dnd', 'dnd'], ['offline', 'off']] as const)('presence %s is a %s state dot', (presence, tone) => {
    render(<Avatar name="lina" presence={presence} />);
    const dot = screen.getByRole('img', { name: `lina, ${presence}` }).querySelector('.d-state-dot');
    expect(dot).toHaveAttribute('data-tone', tone);
    expect(dot).toHaveAttribute('data-size', 'md');
  });
  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Avatar name="erik" presence="dnd" size="lg" /></div>);
    await expectNoAxeViolations(container);
  });
  it('maintains ≥4.5:1 contrast with white across all avatar colours', () => {
    // Task 11: the prototype's palette (hue, saturation, lightness per entry) with white initials; imported, not copied.
    const MIN_CONTRAST = 4.5;

    AVATAR_PALETTE.forEach(({ h: hue, s, l }) => {
      const [r, g, b] = hslToRgb(hue, s, l);
      const bgHex = rgbToHex(r, g, b);
      const ratio = contrastRatio('#FFFFFF', bgHex);
      expect(ratio, `Hue ${hue}° has contrast ${ratio.toFixed(2)}:1 with white`).toBeGreaterThanOrEqual(MIN_CONTRAST);
    });
  });
});
const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

describe('Task 11: the prototype sample people get the prototype colours', () => {
  it('maps Ada, Björn, Mira, Sven, Liv, Kari and Ole to the prototype C table', () => {
    const want = { Ada: [298, 45, 42], Björn: [211, 45, 42], Mira: [234, 45, 42], Sven: [28, 50, 40], Liv: [160, 40, 34], Kari: [12, 55, 42], Ole: [48, 45, 34] };
    for (const [name, [h, s, l]] of Object.entries(want)) expect(avatarColour(name), name).toEqual({ h, s, l });
  });
});

describe('Avatar shape', () => {
  it('is a circle by default and marks a bot with data-shape="bot"', () => {
    const { rerender } = render(<Avatar name="Skald" />);
    expect(screen.getByRole('img', { name: 'Skald' })).toHaveAttribute('data-shape', 'circle');
    rerender(<Avatar name="Skald" shape="bot" />);
    expect(screen.getByRole('img', { name: 'Skald' })).toHaveAttribute('data-shape', 'bot');
  });
});

describe('Avatar sizes xs and row (UI-1)', () => {
  it('xs is 24px and row is 32px, by data-size', () => {
    const css = read('./Avatar.css');
    expect(css).toMatch(/\.d-avatar\[data-size="xs"\] \{[^}]*width: 1\.5rem[^}]*height: 1\.5rem/);
    expect(css).toMatch(/\.d-avatar\[data-size="row"\] \{[^}]*width: 2rem[^}]*height: 2rem/);
    const { rerender } = render(<Avatar name="ada" size="xs" />);
    expect(screen.getByRole('img', { name: 'ada' })).toHaveAttribute('data-size', 'xs');
    rerender(<Avatar name="ada" size="row" />);
    expect(screen.getByRole('img', { name: 'ada' })).toHaveAttribute('data-size', 'row');
  });
});
