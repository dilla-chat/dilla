import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { SensitivityMeter, formatDb, segmentStates } from './SensitivityMeter.tsx';

const base = { levelDb: -100, thresholdDb: -48, auto: true, testing: false, onThresholdChange: () => {}, label: 'Sensitivity threshold' };

describe('segmentStates (the prototype rule)', () => {
  it('lights nothing while not testing', () => {
    expect(segmentStates(-10, -48, false).every(s => s === 'off')).toBe(true);
  });
  it('under the threshold, over it, and over −10 dB', () => {
    const s = segmentStates(-4, -48, true);
    expect(s).toHaveLength(50);
    expect(s[0]).toBe('below'); // −100 dB
    expect(s[25]).toBe('below'); // −50 dB, under −48
    expect(s[26]).toBe('on'); // −48, at the line
    expect(s[45]).toBe('on'); // −10 is not over −10
    expect(s[46]).toBe('hot'); // −8
    expect(s[47]).toBe('hot'); // −6
    expect(s[48]).toBe('off'); // −4 is not under the level
  });
});

describe('SensitivityMeter', () => {
  it('the threshold is a slider with the dB as its text; the meter itself is hidden from assistive tech', () => {
    const { container } = render(<SensitivityMeter {...base} auto={false} />);
    const slider = screen.getByRole('slider', { name: 'Sensitivity threshold' });
    expect(slider).toHaveAttribute('aria-valuetext', '-48 dB');
    expect(slider).toHaveValue('-48');
    expect(container.querySelector('.d-sens__segs')).toHaveAttribute('aria-hidden', 'true');
    expect(container.querySelector('.d-sens__scale')).toHaveAttribute('aria-hidden', 'true');
    expect(container.querySelector('.d-sens__value')?.textContent).toBe('-48 dB');
  });

  it('dragging reports the new threshold; Page Up/Down move it 10 dB, clamped', () => {
    const onThresholdChange = vi.fn();
    render(<SensitivityMeter {...base} auto={false} thresholdDb={-5} onThresholdChange={onThresholdChange} />);
    const slider = screen.getByRole('slider');
    fireEvent.input(slider, { target: { value: '-30' } });
    expect(onThresholdChange).toHaveBeenLastCalledWith(-30, false);
    // Fix round 2: the native change commits (a pointer let go, a key step, or VoiceOver / TalkBack setting the value).
    fireEvent.change(slider);
    expect(onThresholdChange).toHaveBeenLastCalledWith(-30, true);
    // A move then a blur (or pointercancel) commits too.
    fireEvent.input(slider, { target: { value: '-25' } });
    fireEvent.blur(slider);
    expect(onThresholdChange).toHaveBeenLastCalledWith(-25, true);
    fireEvent.keyDown(slider, { key: 'PageUp' });
    expect(onThresholdChange).toHaveBeenLastCalledWith(0, true);
    fireEvent.keyDown(slider, { key: 'PageDown' });
    expect(onThresholdChange).toHaveBeenLastCalledWith(-15, true);
  });

  it('automatic sensitivity disables the slider', () => {
    const onThresholdChange = vi.fn();
    render(<SensitivityMeter {...base} onThresholdChange={onThresholdChange} />);
    const slider = screen.getByRole('slider');
    expect(slider).toBeDisabled();
    fireEvent.keyDown(slider, { key: 'PageUp' });
    expect(onThresholdChange).not.toHaveBeenCalled();
  });

  it('places the line at the threshold', () => {
    const { container } = render(<SensitivityMeter {...base} thresholdDb={-35} />);
    expect((container.querySelector('.d-sens__line') as HTMLElement).style.left).toBe('65%');
  });

  it('formats like the prototype and has no axe violations', async () => {
    expect(formatDb(-47.6)).toBe('-48 dB');
    expect(formatDb(12)).toBe('0 dB');
    const { container } = render(<SensitivityMeter {...base} auto={false} testing levelDb={-30} />);
    expect((await axe(container, { rules: { "color-contrast": { enabled: false } } })).violations).toEqual([]);
  });
});
