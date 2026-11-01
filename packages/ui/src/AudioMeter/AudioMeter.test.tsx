import { render, screen } from '@testing-library/react';
import { afterEach, describe, it, expect } from 'vitest';
import { axe } from 'vitest-axe';
import { AudioMeter, type AudioMeterProps } from './AudioMeter.tsx';
import meta from './AudioMeter.stories.tsx';

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('AudioMeter', () => {
  it('AudioMeter exposes its level as a meter 0..12', () => {
    const { rerender, container } = render(<AudioMeter level={7} label="input level" />);
    const meter = screen.getByRole('meter', { name: 'input level' });
    expect(meter).toHaveAttribute('aria-valuemin', '0');
    expect(meter).toHaveAttribute('aria-valuemax', '12');
    expect(meter).toHaveAttribute('aria-valuenow', '7');
    expect(container.querySelectorAll('[data-on="true"]')).toHaveLength(7);
    rerender(<AudioMeter level={13} label="input level" />);
    expect(screen.getByRole('meter', { name: 'input level' })).toHaveAttribute('aria-valuenow', '12');
    rerender(<AudioMeter level={-1} label="input level" />);
    expect(screen.getByRole('meter', { name: 'input level' })).toHaveAttribute('aria-valuenow', '0');
    rerender(<AudioMeter level={Number.NaN} label="input level" />);
    expect(screen.getByRole('meter', { name: 'input level' })).toHaveAttribute('aria-valuenow', '0');
  });

  it('bar i rises from 30% to 100% of the meter height', () => {
    const { container } = render(<AudioMeter level={5} label="input level" />);
    const bars = Array.from(container.querySelectorAll<HTMLElement>('.d-audio-meter__bar'));
    expect(bars).toHaveLength(12);
    bars.forEach((bar, i) => expect(bar.style.height).toBe(`${30 + Math.round((70 * i) / 11)}%`));
    expect(bars[0].style.height).toBe('30%');
    expect(bars[11].style.height).toBe('100%');
    expect(bars.map(b => b.dataset.on).join(',')).toBe('true,true,true,true,true,false,false,false,false,false,false,false');
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><AudioMeter {...(meta.args as AudioMeterProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
});
