import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { act, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { pushHistoryState, replaceHistoryState } from './router.ts';
import { PHONE_QUERY, readPaneWidths, rememberedTab, rememberTab, writePaneWidths, usePhoneLayout, usePhoneSheet, useVisualViewportHeight } from './layout.ts';

const here = dirname(fileURLToPath(import.meta.url));

const FULL_QUERY = '(max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';
const PHONE_ATTR = '[data-layout="phone"]';

describe('the phone breakpoint', () => {
  it('is the 47.99rem width or a coarse pointer under 32rem tall (R9), in the query and base.css', () => {
    expect(PHONE_QUERY).toBe(FULL_QUERY);
    const css = readFileSync(join(here, '..', '..', 'ui', 'src', 'styles', 'base.css'), 'utf8');
    expect(css).toContain('@container d-shell (max-width: 47.99rem)');
    expect(css).toContain(`@media ${FULL_QUERY}`);
  });
});

describe('R9 (web half): app.css reads the same phone query and has the container twins', () => {
  const app = readFileSync(join(here, 'app.css'), 'utf8');
  const block = (css: string, open: number): string => {
    let depth = 0;
    for (let i = open; i < css.length; i += 1) {
      if (css[i] === '{') depth += 1;
      else if (css[i] === '}' && (depth -= 1) === 0) return css.slice(open + 1, i);
    }
    throw new Error('unbalanced braces');
  };
  const rules = (css: string) => [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .map(m => ({ selector: m[1].trim(), body: m[2].trim() }));
  const parts = (selector: string) => selector.split(',').map(x => x.trim());

  it('every @media rule with 47.99rem in app.css reads exactly the landscape query (pre-flight C-6)', () => {
    const found = [...app.matchAll(/@media[^{]*\{/g)].map(m => m[0].slice(0, -1).trim()).filter(p => p.includes('47.99rem'));
    expect(found).toEqual([`@media ${FULL_QUERY}`]);
  });

  it('every phone-only rule of the @container d-shell blocks has a [data-layout="phone"] twin with identical declarations (pre-flight C-7)', () => {
    const opens = [...app.matchAll(/@container d-shell \(max-width: 47\.99rem\) \{/g)];
    expect(opens.length).toBe(2);
    const flat = rules(app.replace(/@(container|media)[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}/g, ''));
    let count = 0;
    for (const m of opens) {
      for (const r of rules(block(app, m.index + m[0].length - 1))) {
        // A rule that already excludes the phone shell is desktop-only and has nothing to twin.
        if (r.selector.includes(`:not(${PHONE_ATTR})`)) continue;
        count += 1;
        const twin = flat.find(t => t.body === r.body && parts(t.selector).every(x => x.startsWith(`${PHONE_ATTR} `))
          && parts(t.selector).map(x => x.slice(PHONE_ATTR.length + 1)).join(', ') === parts(r.selector).join(', '));
        expect(twin, r.selector).toBeDefined();
      }
    }
    expect(count).toBeGreaterThan(10);
  });
});

describe('the phone shell follows the visual viewport (web3.7.2)', () => {
  const base = readFileSync(join(here, '..', '..', 'ui', 'src', 'styles', 'base.css'), 'utf8');
  const settings = readFileSync(join(here, '..', '..', 'ui', 'src', 'SettingsFrame', 'SettingsFrame.css'), 'utf8');
  const app = readFileSync(join(here, 'app.css'), 'utf8');
  const rule = (css: string, selector: string) => css.split('\n').find(l => l.trimStart().startsWith(selector)) ?? '';

  it('fixes the phone shell at --d-vvtop', () => {
    const shell = rule(base, '.d-app-shell[data-layout="phone"] {');
    expect(shell).toContain('position: fixed');
    expect(shell).toContain('top: var(--d-vvtop, 0px)');
  });
  it('moves the ring and the fullscreen modal Settings frame with it', () => {
    expect(rule(base, '.d-app-shell .d-app-shell__ring')).toContain('var(--d-vvtop, 0px)');
    expect(rule(settings, '.d-settings-frame.d-settings-frame[data-layout="phone"]')).toContain('var(--d-vvtop, 0px)');
    expect(base).not.toContain('.d-settings-frame.d-settings-frame[data-modeless]');
  });
  it('moves the search palette and the other-tab overlay with it', () => {
    expect(rule(app, '.dw-search {')).toContain('var(--d-vvtop, 0px)');
    expect(rule(app, '.dw-other-tab--overlay')).toContain('var(--d-vvtop, 0px)');
  });
  it('locks the page by the data-phone-shell attribute (R10), inside the landscape query, not by :has', () => {
    const phone = app.slice(app.indexOf(`@media ${FULL_QUERY}`));
    expect(phone).toMatch(/html\[data-phone-shell\], html\[data-phone-shell\] body \{[^}]*overflow: hidden/);
    expect(app).not.toContain(':has(.d-app-shell[data-layout="phone"])');
    expect(app).not.toMatch(/(^|\n)\s*html, body \{[^}]*overflow/);
  });
});

describe('usePhoneLayout', () => {
  afterEach(() => { Reflect.deleteProperty(window, 'matchMedia'); });

  it('is false without matchMedia', () => {
    const { result } = renderHook(() => usePhoneLayout());
    expect(result.current).toBe(false);
  });

  it('follows a change event of the query', () => {
    let matches = false;
    const listeners = new Set<() => void>();
    window.matchMedia = vi.fn((query: string) => ({
      get matches() { return query === PHONE_QUERY && matches; },
      addEventListener: (_: string, l: () => void) => listeners.add(l),
      removeEventListener: (_: string, l: () => void) => listeners.delete(l),
    })) as unknown as typeof window.matchMedia;
    const { result, unmount } = renderHook(() => usePhoneLayout());
    expect(result.current).toBe(false);
    act(() => { matches = true; listeners.forEach(l => l()); });
    expect(result.current).toBe(true);
    unmount();
    expect(listeners.size).toBe(0);
  });
});

describe('useVisualViewportHeight', () => {
  afterEach(() => { Reflect.deleteProperty(window, 'visualViewport'); document.documentElement.style.removeProperty('--d-vvh'); document.documentElement.style.removeProperty('--d-vvtop'); });

  it('sets --d-vvtop from the visual viewport offset, follows a scroll, and removes it on unmount', () => {
    const target = new EventTarget();
    const vv = Object.assign(target, { height: 500, offsetTop: 95 });
    Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
    const { unmount } = renderHook(() => useVisualViewportHeight());
    expect(document.documentElement.style.getPropertyValue('--d-vvtop')).toBe('95px');
    vv.offsetTop = 0;
    act(() => { target.dispatchEvent(new Event('scroll')); });
    expect(document.documentElement.style.getPropertyValue('--d-vvtop')).toBe('0px');
    unmount();
    expect(document.documentElement.style.getPropertyValue('--d-vvtop')).toBe('');
  });

  it('sets --d-vvh from the visual viewport on resize and scroll, and removes it on unmount', () => {
    const target = new EventTarget();
    const vv = Object.assign(target, { height: 600 });
    Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
    const { unmount } = renderHook(() => useVisualViewportHeight());
    expect(document.documentElement.style.getPropertyValue('--d-vvh')).toBe('600px');
    vv.height = 400;
    act(() => { target.dispatchEvent(new Event('resize')); });
    expect(document.documentElement.style.getPropertyValue('--d-vvh')).toBe('400px');
    vv.height = 350;
    act(() => { target.dispatchEvent(new Event('scroll')); });
    expect(document.documentElement.style.getPropertyValue('--d-vvh')).toBe('350px');
    unmount();
    expect(document.documentElement.style.getPropertyValue('--d-vvh')).toBe('');
  });

  it('sets data-phone-shell on <html> while enabled and removes it on unmount or when turned off (R10)', () => {
    Object.defineProperty(window, 'visualViewport', { configurable: true, value: Object.assign(new EventTarget(), { height: 600, offsetTop: 0 }) });
    const { rerender, unmount } = renderHook(({ on }) => useVisualViewportHeight(on), { initialProps: { on: true } });
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(true);
    rerender({ on: false });
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(false);
    rerender({ on: true });
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(true);
    unmount();
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(false);
  });

  it('sets data-phone-shell even where there is no visualViewport', () => {
    const { unmount } = renderHook(() => useVisualViewportHeight(true));
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(true);
    unmount();
    expect(document.documentElement.hasAttribute('data-phone-shell')).toBe(false);
  });

  describe('after a resize that shrinks the height (m7)', () => {
    let frames: FrameRequestCallback[] = [];
    afterEach(() => { vi.unstubAllGlobals(); document.body.replaceChildren(); });
    const setup = () => {
      frames = [];
      vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => { frames.push(cb); return frames.length; });
      const target = new EventTarget();
      const vv = Object.assign(target, { height: 600, offsetTop: 0 });
      Object.defineProperty(window, 'visualViewport', { configurable: true, value: vv });
      const field = document.createElement('input');
      document.body.append(field);
      field.focus();
      const scroll = vi.fn();
      field.scrollIntoView = scroll;
      const { unmount } = renderHook(() => useVisualViewportHeight(true));
      return { vv, target, scroll, unmount };
    };

    it('scrolls the focused field into view on the next frame', () => {
      const { vv, target, scroll } = setup();
      vv.height = 350;
      act(() => { target.dispatchEvent(new Event('resize')); });
      expect(scroll).not.toHaveBeenCalled();
      act(() => { frames.forEach(f => f(0)); });
      expect(scroll).toHaveBeenCalledWith({ block: 'nearest' });
    });

    it('does nothing when the height grows, or when focus is not on a field', () => {
      const { vv, target, scroll, unmount } = setup();
      vv.height = 700;
      act(() => { target.dispatchEvent(new Event('resize')); });
      act(() => { frames.forEach(f => f(0)); });
      expect(scroll).not.toHaveBeenCalled();
      (document.activeElement as HTMLElement).blur();
      vv.height = 300;
      act(() => { target.dispatchEvent(new Event('resize')); });
      act(() => { frames.forEach(f => f(0)); });
      expect(scroll).not.toHaveBeenCalled();
      unmount();
    });
  });

  it('does nothing when disabled', () => {
    Object.defineProperty(window, 'visualViewport', { configurable: true, value: Object.assign(new EventTarget(), { height: 600 }) });
    renderHook(() => useVisualViewportHeight(false));
    expect(document.documentElement.style.getPropertyValue('--d-vvh')).toBe('');
  });
});

describe('tab memory', () => {
  it('round-trips through sessionStorage', () => {
    // eslint-disable-next-line no-restricted-properties -- the tab memory test reads the one key it writes
    window.sessionStorage.clear();
    expect(rememberedTab('home')).toBeNull();
    rememberTab('home', '/dm/abc');
    expect(rememberedTab('home')).toBe('/dm/abc');
    // eslint-disable-next-line no-restricted-properties -- the tab memory test reads the one key it writes
    expect(window.sessionStorage.getItem('dilla.phoneTab.home')).toBe('/dm/abc');
  });

  it('a path remembered under a retired tab name reads back as home', () => {
    // eslint-disable-next-line no-restricted-properties -- the tab memory test writes the retired keys web3.8 left behind
    window.sessionStorage.clear();
    // eslint-disable-next-line no-restricted-properties -- the tab memory test writes the retired keys web3.8 left behind
    window.sessionStorage.setItem('dilla.phoneTab.dms', '/dm/abc');
    expect(rememberedTab('home')).toBe('/dm/abc');
    // eslint-disable-next-line no-restricted-properties -- the tab memory test writes the retired keys web3.8 left behind
    window.sessionStorage.clear();
    // eslint-disable-next-line no-restricted-properties -- the tab memory test writes the retired keys web3.8 left behind
    window.sessionStorage.setItem('dilla.phoneTab.servers', '/c/abc');
    expect(rememberedTab('home')).toBe('/c/abc');
  });

  it('returns null without throwing when sessionStorage throws', () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
    expect(rememberedTab('you')).toBeNull();
    expect(() => rememberTab('you', '/you')).not.toThrow();
    get.mockRestore();
    set.mockRestore();
  });
});

describe('pane widths (R18)', () => {
  afterEach(() => {
    // eslint-disable-next-line no-restricted-properties -- the pane test clears the one key it writes
    window.localStorage.clear();
  });

  it('round-trips through localStorage under dilla.paneWidths', () => {
    expect(readPaneWidths()).toEqual({});
    writePaneWidths({ sidebar: 288, aside: 264 });
    expect(readPaneWidths()).toEqual({ sidebar: 288, aside: 264 });
    // eslint-disable-next-line no-restricted-properties -- the pane test reads the one key it writes
    expect(JSON.parse(window.localStorage.getItem('dilla.paneWidths') ?? 'null')).toEqual({ sidebar: 288, aside: 264 });
    writePaneWidths({ aside: 300 });
    expect(readPaneWidths()).toEqual({ aside: 300 });
  });

  it('returns {} for a malformed value, a non-object, or non-positive and non-finite widths', () => {
    for (const bad of ['{nope', '[1,2]', '7', 'null', '"x"']) {
      // eslint-disable-next-line no-restricted-properties -- the pane test seeds the one key it reads
      window.localStorage.setItem('dilla.paneWidths', bad);
      expect(readPaneWidths(), bad).toEqual({});
    }
    // eslint-disable-next-line no-restricted-properties -- the pane test seeds the one key it reads
    window.localStorage.setItem('dilla.paneWidths', JSON.stringify({ sidebar: -4, aside: '300' }));
    expect(readPaneWidths()).toEqual({});
    // eslint-disable-next-line no-restricted-properties -- the pane test seeds the one key it reads
    window.localStorage.setItem('dilla.paneWidths', JSON.stringify({ sidebar: 0, aside: 250, extra: 1 }));
    expect(readPaneWidths()).toEqual({ aside: 250 });
  });

  it('returns {} without throwing when localStorage throws', () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
    expect(readPaneWidths()).toEqual({});
    expect(() => writePaneWidths({ sidebar: 280 })).not.toThrow();
    get.mockRestore();
    set.mockRestore();
  });
});

describe('usePhoneSheet', () => {
  afterEach(() => { replaceHistoryState(null, '', '/'); });

  it('pushes a sheet entry on open, closes on the system back, and pops its own entry on close()', async () => {
    replaceHistoryState({ phoneBack: true }, '', '/c/x');
    const onClose = vi.fn();
    const { result, rerender } = renderHook(({ open }) => usePhoneSheet('members', open, onClose), { initialProps: { open: false } });
    const before = window.history.length;
    rerender({ open: true });
    const state = window.history.state as Record<string, unknown>;
    expect(state).toMatchObject({ phoneBack: true, phoneSheet: 'members' });
    expect(typeof state.dillaHistoryIndex).toBe('number');
    expect(window.history.length).toBe(before + 1);
    const originalBack = window.history.back.bind(window.history);
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => originalBack());
    act(() => { result.current.close(); });
    expect(back).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    back.mockRestore();
  });

  it('close() twice before the pop arrives calls history.back() once', () => {
    replaceHistoryState({ phoneBack: true }, '', '/c/x');
    const { result, rerender } = renderHook(({ open }) => usePhoneSheet('members', open, vi.fn()), { initialProps: { open: false } });
    rerender({ open: true });
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    act(() => { result.current.close(); result.current.close(); });
    expect(back).toHaveBeenCalledTimes(1);
    back.mockRestore();
  });

  it('strips a stale sheet entry (a reload, a forward) so closing never pops the view', async () => {
    replaceHistoryState({ phoneBack: true, phoneSheet: 'members' }, '', '/c/x');
    const onClose = vi.fn();
    const { result, rerender } = renderHook(({ open }) => usePhoneSheet('members', open, onClose), { initialProps: { open: false } });
    expect(window.history.state).toMatchObject({ phoneBack: true });
    expect(typeof (window.history.state as Record<string, unknown>).dillaHistoryIndex).toBe('number');
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    rerender({ open: true });
    expect(window.history.state).toMatchObject({ phoneBack: true, phoneSheet: 'members' });
    expect(typeof (window.history.state as Record<string, unknown>).dillaHistoryIndex).toBe('number');
    act(() => { result.current.close(); });
    expect(back).toHaveBeenCalledTimes(1);
    back.mockRestore();
    // A forward onto a sheet entry while closed is stripped too, and its close is not a back.
    rerender({ open: false });
    replaceHistoryState(null, '', '/c/x');
    pushHistoryState({ phoneSheet: 'members' }, '', '/c/x');
    act(() => { window.history.back(); });
    await waitFor(() => expect(window.history.state).not.toHaveProperty('phoneSheet'));
    act(() => { window.history.forward(); });
    await waitFor(() => expect(window.history.state).not.toHaveProperty('phoneSheet'));
    const again = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    act(() => { result.current.close(); });
    expect(again).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalled();
    again.mockRestore();
  });

  it('close() does not pop an entry this instance did not push', () => {
    replaceHistoryState({ phoneSheet: 'members' }, '', '/c/x');
    const onClose = vi.fn();
    // Closed, so the stale key is stripped; an open sheet whose own entry was lost (a replace) is the other case.
    const { result, rerender } = renderHook(({ open }) => usePhoneSheet('members', open, onClose), { initialProps: { open: true } });
    rerender({ open: false });
    replaceHistoryState({ phoneSheet: 'members' }, '', '/c/x');
    const back = vi.spyOn(window.history, 'back').mockImplementation(() => undefined);
    act(() => { result.current.close(); });
    expect(back).not.toHaveBeenCalled();
    expect(onClose).toHaveBeenCalledTimes(1);
    back.mockRestore();
  });

  it('calls onClose at once when the sheet has no entry', () => {
    replaceHistoryState(null, '', '/c/x');
    const onClose = vi.fn();
    const { result } = renderHook(() => usePhoneSheet('thread', false, onClose));
    act(() => { result.current.close(); });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
