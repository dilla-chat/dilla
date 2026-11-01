import { describe, it, expect } from 'vitest';
import { applyPreference, applyTheme, followSystemTheme, preferredTheme, THEME_QUERIES } from './theme.ts';

const matching = (on: string[]) => (q: string) => ({ matches: on.includes(q) });

describe('preferredTheme', () => {
  it('follows the OS, and contrast wins over the colour scheme', () => {
    expect(preferredTheme(matching([]))).toBe('signal');
    expect(preferredTheme(matching([THEME_QUERIES.light]))).toBe('light');
    expect(preferredTheme(matching([THEME_QUERIES.contrast]))).toBe('high-contrast');
    expect(preferredTheme(matching([THEME_QUERIES.light, THEME_QUERIES.contrast]))).toBe('high-contrast');
  });
});

describe('applyTheme', () => {
  it('sets the theme and the regular density', () => {
    const el = document.createElement('html');
    applyTheme(el, 'light');
    expect(el.dataset.theme).toBe('light');
    expect(el.dataset.density).toBe('regular');
  });
  it('keeps a chosen density when the theme changes', () => {
    const el = document.createElement('html');
    el.dataset.density = 'compact';
    applyTheme(el, 'light');
    expect(el.dataset.density).toBe('compact');
  });
});

describe('followSystemTheme', () => {
  it('applies now, follows changes and stops when asked', () => {
    const on = new Set<string>();
    const handlers = new Map<string, Set<() => void>>();
    const mql = (q: string) => ({
      get matches() { return on.has(q); },
      addEventListener: (_: 'change', h: () => void) => { handlers.set(q, (handlers.get(q) ?? new Set()).add(h)); },
      removeEventListener: (_: 'change', h: () => void) => { handlers.get(q)?.delete(h); },
    }) as unknown as MediaQueryList;
    const doc = document.implementation.createHTMLDocument('t');
    const stop = followSystemTheme({ matchMedia: mql, document: doc });
    expect(doc.documentElement.dataset.theme).toBe('signal');
    on.add(THEME_QUERIES.light);
    handlers.get(THEME_QUERIES.light)?.forEach(h => h());
    expect(doc.documentElement.dataset.theme).toBe('light');
    stop();
    expect(handlers.get(THEME_QUERIES.light)?.size ?? 0).toBe(0);
    expect(handlers.get(THEME_QUERIES.contrast)?.size ?? 0).toBe(0);
  });
});

describe('applyPreference', () => {
  function system() {
    const on = new Set<string>();
    const handlers = new Map<string, Set<() => void>>();
    const mql = (q: string) => ({
      get matches() { return on.has(q); },
      addEventListener: (_: 'change', h: () => void) => { handlers.set(q, (handlers.get(q) ?? new Set()).add(h)); },
      removeEventListener: (_: 'change', h: () => void) => { handlers.get(q)?.delete(h); },
    }) as unknown as MediaQueryList;
    const doc = document.implementation.createHTMLDocument('t');
    const fire = (q: string) => handlers.get(q)?.forEach(h => h());
    const listening = () => (handlers.get(THEME_QUERIES.light)?.size ?? 0) + (handlers.get(THEME_QUERIES.contrast)?.size ?? 0);
    return { on, win: { matchMedia: mql, document: doc }, doc, fire, listening };
  }
  it('follows the system for system, and a chosen theme stops the following', () => {
    const s = system();
    applyPreference(s.win, 'system');
    expect(s.doc.documentElement.dataset.theme).toBe('signal');
    expect(s.listening()).toBe(2);
    applyPreference(s.win, 'light');
    expect(s.doc.documentElement.dataset.theme).toBe('light');
    expect(s.listening()).toBe(0);
    s.on.add(THEME_QUERIES.contrast);
    s.fire(THEME_QUERIES.contrast);
    expect(s.doc.documentElement.dataset.theme).toBe('light');
    applyPreference(s.win, 'system');
    expect(s.doc.documentElement.dataset.theme).toBe('high-contrast');
    expect(s.listening()).toBe(2);
    applyPreference(s.win, 'system');
    expect(s.listening()).toBe(2);
    applyPreference(s.win, 'signal');
    expect(s.doc.documentElement.dataset.theme).toBe('signal');
    expect(s.doc.documentElement.dataset.density).toBe('regular');
    expect(s.listening()).toBe(0);
  });
});
