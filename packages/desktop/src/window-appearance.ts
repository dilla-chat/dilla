import { themes, type ThemeName } from '../../design-tokens/src/tokens.ts';

/** Paint the complete native surface even before Chromium has a content frame.
 * Transparent overlay surfaces expose compositor clears during resize/startup. */
export function windowAppearance(theme: ThemeName) {
  const colors = themes[theme];
  return { transparent: false, backgroundColor: colors.bg,
    titleBarOverlay: { color: colors.bg3, symbolColor: colors.fg2, height: 32 } };
}
