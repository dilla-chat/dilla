import { render, screen } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { QrCode } from './QrCode.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const LABEL = 'Setup QR code';
const QUIET = 4;

function viewBoxSide(): number {
  const svg = screen.getByRole('img', { name: LABEL });
  const parts = svg.getAttribute('viewBox')!.split(' ').map(Number);
  expect(parts[0]).toBe(0);
  expect(parts[1]).toBe(0);
  expect(parts[2]).toBe(parts[3]);
  return parts[2]!;
}

describe('QrCode', () => {
  it('encodes HELLO as version 1: 21 modules plus a quiet zone of 4 on each side', () => {
    render(<QrCode value="HELLO" label={LABEL} />);
    expect(viewBoxSide()).toBe(21 + 2 * QUIET);
  });

  it('fits a 120 byte otpauth URI in version 10 or lower', () => {
    const uri = 'otpauth://totp/Dilla:' + 'a'.repeat(40) + '?secret=JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=Dilla&algorithm=SHA1&digits=6&period=30';
    expect(new TextEncoder().encode(uri).length).toBeGreaterThanOrEqual(120);
    render(<QrCode value={uri} label={LABEL} />);
    const modules = viewBoxSide() - 2 * QUIET;
    expect((modules - 17) % 4).toBe(0);
    expect((modules - 17) / 4).toBeLessThanOrEqual(10);
  });

  it('is one image named by the label, with a single path for the dark modules', () => {
    const { container } = render(<QrCode value="HELLO" label={LABEL} />);
    const svg = screen.getByRole('img', { name: LABEL });
    expect(svg.tagName.toLowerCase()).toBe('svg');
    expect(container.querySelectorAll('svg')).toHaveLength(1);
    expect(svg.querySelectorAll('path')).toHaveLength(1);
    expect(svg.querySelector('path')!.getAttribute('d')).toMatch(/^M/);
  });

  it('draws a different symbol for a different value', () => {
    const a = render(<QrCode value="HELLO" label={LABEL} />);
    const da = a.container.querySelector('path')!.getAttribute('d');
    a.unmount();
    const b = render(<QrCode value="WORLD" label={LABEL} />);
    expect(b.container.querySelector('path')!.getAttribute('d')).not.toBe(da);
  });

  it('sizes the image by the size prop, 192 by default', () => {
    const { rerender } = render(<QrCode value="HELLO" label={LABEL} />);
    expect(screen.getByRole('img', { name: LABEL })).toHaveAttribute('width', '192');
    rerender(<QrCode value="HELLO" label={LABEL} size={160} />);
    expect(screen.getByRole('img', { name: LABEL })).toHaveAttribute('height', '160');
  });

  it('pins the light theme on itself, so it stays dark on light inside a dark-theme root', () => {
    const { container } = render(<div className="d-root" data-theme="signal"><QrCode value="HELLO" label={LABEL} /></div>);
    const svg = screen.getByRole('img', { name: LABEL });
    expect(svg).toHaveAttribute('data-theme', 'light');
    expect(svg.closest('[data-theme]')).toBe(svg);
    expect(container.firstElementChild).toHaveAttribute('data-theme', 'signal');
    expect(svg.querySelector('.d-qr__light')).not.toBeNull();
    expect(svg.querySelector('.d-qr__dark')).not.toBeNull();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><QrCode value="HELLO" label={LABEL} /></div>);
    await expectNoAxeViolations(container);
  });
});
