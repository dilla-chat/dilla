import { act, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { Tooltip } from './Tooltip.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const cssText = () => readFileSync(join(dirname(fileURLToPath(import.meta.url)), './Tooltip.css'), 'utf8');

function mount(keys?: string[]) {
  render(<Tooltip label="mute" keys={keys}><button type="button">mute</button></Tooltip>);
  return screen.getByRole('button', { name: 'mute' });
}
const focus = (el: HTMLElement) => act(() => el.focus());
const advance = (ms: number) => act(() => { vi.advanceTimersByTime(ms); });

// jsdom has no PointerEvent: without one, fireEvent drops `pointerType` and a touch pointer cannot be told from a mouse.
class FakePointerEvent extends MouseEvent {
  readonly pointerType: string;
  constructor(type: string, init: MouseEventInit & { pointerType?: string } = {}) { super(type, init); this.pointerType = init.pointerType ?? ''; }
}
beforeEach(() => { vi.stubGlobal('PointerEvent', FakePointerEvent); vi.useFakeTimers(); });
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

describe('Tooltip (R15)', () => {
  it('shows 400 ms after the pointer enters, not at 399', () => {
    const btn = mount();
    fireEvent.pointerEnter(btn, { pointerType: 'mouse' });
    advance(399);
    expect(screen.queryByRole('tooltip')).toBeNull();
    advance(1);
    expect(screen.getByRole('tooltip')).toHaveTextContent('mute');
  });

  it('shows at once on keyboard focus', () => {
    focus(mount());
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
  });

  it('hides on blur', () => {
    const btn = mount();
    focus(btn);
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
    act(() => btn.blur());
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('Escape hides it and focus stays; with no tooltip shown Escape is not consumed', () => {
    const outer = vi.fn();
    render(<div onKeyDown={outer}><Tooltip label="mute"><button type="button">mute</button></Tooltip></div>);
    const btn = screen.getByRole('button', { name: 'mute' });
    focus(btn);
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
    fireEvent.keyDown(btn, { key: 'Escape' });
    expect(screen.queryByRole('tooltip')).toBeNull();
    expect(btn).toHaveFocus();
    expect(outer).not.toHaveBeenCalledWith(expect.objectContaining({ key: 'Escape' }));
    fireEvent.keyDown(btn, { key: 'Escape' });
    expect(outer).toHaveBeenCalledWith(expect.objectContaining({ key: 'Escape' }));
  });

  it('a touch pointer never shows it', () => {
    const btn = mount();
    fireEvent.pointerDown(btn, { pointerType: 'touch' });
    fireEvent.pointerEnter(btn, { pointerType: 'touch' });
    advance(1000);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('a device with no hover never shows it', () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q === '(hover: none)', media: q, addEventListener() {}, removeEventListener() {} }));
    const btn = mount();
    fireEvent.pointerEnter(btn, { pointerType: 'mouse' });
    advance(1000);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('hides 100 ms after the pointer leaves', () => {
    const btn = mount();
    fireEvent.pointerEnter(btn, { pointerType: 'mouse' });
    advance(400);
    fireEvent.pointerLeave(btn, { pointerType: 'mouse' });
    advance(99);
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
    advance(1);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('moving from the trigger onto the tooltip keeps it (1.4.13 hoverable)', () => {
    const btn = mount();
    fireEvent.pointerEnter(btn, { pointerType: 'mouse' });
    advance(400);
    fireEvent.pointerLeave(btn, { pointerType: 'mouse' });
    advance(50);
    fireEvent.pointerEnter(screen.getByRole('tooltip'), { pointerType: 'mouse' });
    advance(1000);
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
    fireEvent.pointerLeave(screen.getByRole('tooltip'), { pointerType: 'mouse' });
    advance(100);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('leaving before 400 ms never shows it', () => {
    const btn = mount();
    fireEvent.pointerEnter(btn, { pointerType: 'mouse' });
    advance(200);
    fireEvent.pointerLeave(btn, { pointerType: 'mouse' });
    advance(1000);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('describes the control by its chord only when keys are given, never by the label', () => {
    const { unmount } = render(<Tooltip label="mute" keys={['Ctrl', 'M']}><button type="button">mute</button></Tooltip>);
    const btn = screen.getByRole('button', { name: 'mute' });
    expect(btn).toHaveAccessibleDescription('Ctrl M');
    focus(btn);
    expect(screen.getByRole('tooltip')).toHaveTextContent('mute');
    expect(btn).toHaveAccessibleDescription('Ctrl M');
    unmount();
    mount();
    expect(screen.getByRole('button', { name: 'mute' })).not.toHaveAttribute('aria-describedby');
  });

  it('keeps the child\'s own aria-describedby and adds the chord after it', () => {
    render(<Tooltip label="mute" keys={['M']}><button type="button" aria-describedby="why">mute</button></Tooltip>);
    expect(screen.getByRole('button', { name: 'mute' }).getAttribute('aria-describedby')).toMatch(/^why /);
  });

  it('sets the placement for the stylesheet', () => {
    render(<Tooltip label="mute" placement="right"><button type="button">mute</button></Tooltip>);
    focus(screen.getByRole('button'));
    expect(screen.getByRole('tooltip')).toHaveAttribute('data-placement', 'right');
  });

  // Task 9: the prototype's floating-layer look (the message toolbar's): --surface-2 in the --line-strong edge, 6px corners.
  it('is mono 11px on --surface-2 with the --line-strong edge and 6px corners, and stays in the viewport (fixed)', () => {
    const rule = cssText().replace(/\/\*[\s\S]*?\*\//g, '').match(/\.d-tooltip\s*\{[^}]*\}/)?.[0] ?? '';
    expect(rule).toContain('font-family: var(--font-mono)');
    expect(rule).toContain('font-size: 0.6875rem');
    expect(rule).toContain('background: var(--surface-2)');
    expect(rule).toContain('border: 1px solid var(--line-strong)');
    expect(rule).toContain('border-radius: var(--r-md)');
    expect(rule).toContain('position: fixed');
  });

  it('has no axe violations with the tooltip shown', async () => {
    mount(['M']);
    focus(screen.getByRole('button'));
    vi.useRealTimers();
    await expectNoAxeViolations(document.body);
  });

  it('follows its control when a container scrolls or the window resizes, one frame at a time', () => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => window.setTimeout(() => cb(0), 16));
    vi.stubGlobal('cancelAnimationFrame', (h: number) => window.clearTimeout(h));
    render(<div data-testid="scroller"><Tooltip label="mute"><button type="button">mute</button></Tooltip></div>);
    const btn = screen.getByRole('button', { name: 'mute' });
    Object.defineProperty(document.documentElement, 'clientHeight', { value: 800, configurable: true });
    Object.defineProperty(document.documentElement, 'clientWidth', { value: 1200, configurable: true });
    let top = 100;
    btn.parentElement!.getBoundingClientRect = () => ({ top, bottom: top + 20, left: 50, right: 70, width: 20, height: 20, x: 50, y: top, toJSON() {} });
    focus(btn);
    const at = () => parseFloat(screen.getByRole('tooltip').style.top);
    const before = at();
    top = 40;
    fireEvent.scroll(screen.getByTestId('scroller'));
    fireEvent.scroll(screen.getByTestId('scroller'));
    advance(16);
    expect(at()).toBe(before - 60);
    top = 200;
    fireEvent(window, new Event('resize'));
    advance(16);
    expect(at()).toBe(before + 100);
    act(() => btn.blur());
    top = 10;
    fireEvent.scroll(screen.getByTestId('scroller'));
    advance(16);
    expect(screen.queryByRole('tooltip')).toBeNull();
  });

  it('opens below a control that has no room above it, so it never covers the control; with room it stays above', () => {
    render(<Tooltip label="new channel"><button type="button">new channel</button></Tooltip>);
    const btn = screen.getByRole('button', { name: 'new channel' });
    Object.defineProperty(document.documentElement, 'clientHeight', { value: 800, configurable: true });
    Object.defineProperty(document.documentElement, 'clientWidth', { value: 1200, configurable: true });
    const real = Element.prototype.getBoundingClientRect;
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
      return this.classList.contains('d-tooltip')
        ? ({ top: 0, bottom: 24, left: 0, right: 60, width: 60, height: 24, x: 0, y: 0, toJSON() {} })
        : real.call(this);
    });
    let top = 10;
    btn.parentElement!.getBoundingClientRect = () => ({ top, bottom: top + 28, left: 50, right: 78, width: 28, height: 28, x: 50, y: top, toJSON() {} });
    focus(btn);
    const tip = () => screen.getByRole('tooltip');
    expect(parseFloat(tip().style.top)).toBeGreaterThanOrEqual(top + 28);
    expect(tip()).toHaveAttribute('data-placement', 'bottom');
    act(() => btn.blur());
    top = 200;
    focus(btn);
    expect(parseFloat(tip().style.top)).toBeLessThan(200);
    expect(tip()).toHaveAttribute('data-placement', 'top');
  });

  it('shows on keyboard focus after a touch pointer (a tablet with a hardware keyboard)', () => {
    const btn = mount();
    fireEvent.pointerDown(btn, { pointerType: 'touch' });
    focus(btn);
    expect(screen.getByRole('tooltip')).toBeInTheDocument();
  });
});

