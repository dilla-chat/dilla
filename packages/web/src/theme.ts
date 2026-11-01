import type { ThemeName } from '@dilla/design-tokens';
import type { ThemePref } from './prefs.ts';

export const THEME_QUERIES = { light: '(prefers-color-scheme: light)', contrast: '(prefers-contrast: more)' } as const;

export function preferredTheme(matchMedia: (query: string) => { matches: boolean }): ThemeName {
  if (matchMedia(THEME_QUERIES.contrast).matches) return 'high-contrast';
  if (matchMedia(THEME_QUERIES.light).matches) return 'light';
  return 'signal';
}

export function applyTheme(root: HTMLElement, theme: ThemeName): void {
  root.dataset.theme = theme;
  if (!root.dataset.density) root.dataset.density = 'regular';
}

export function followSystemTheme(win: Pick<Window, 'matchMedia' | 'document'>): () => void {
  const light = win.matchMedia(THEME_QUERIES.light);
  const contrast = win.matchMedia(THEME_QUERIES.contrast);
  const update = () => applyTheme(win.document.documentElement, preferredTheme(win.matchMedia));
  update();
  light.addEventListener('change', update);
  contrast.addEventListener('change', update);
  return () => {
    light.removeEventListener('change', update);
    contrast.removeEventListener('change', update);
  };
}

let stopFollowing: (() => void) | null = null;

/**
 * Applies the person's theme preference (L-TS-26): 'system' follows the OS as followSystemTheme does, any other
 * preference applies that theme and detaches the system listeners. A second 'system' replaces the first follow.
 */
export function applyPreference(win: Pick<Window, 'matchMedia' | 'document'>, pref: ThemePref): void {
  stopFollowing?.();
  stopFollowing = null;
  if (pref === 'system') stopFollowing = followSystemTheme(win);
  else applyTheme(win.document.documentElement, pref);
}
