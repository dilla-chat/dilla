// Signal values for packages/design-tokens/src/tokens.ts.
// 1) add 'signal' to ThemeName, 2) add fgBody, fgStrong, brand to ColorTokens (fill for every theme),
// 3) paste `signal` into `themes`, 4) replace the listed `structural` entries, 5) emit :root from themes.signal in css.ts.

export const signal = {
  bg: '#08090B', bg2: '#0B0D10', bg3: '#0D1014', surface: '#0D1014', surface2: '#0F1216', surfaceHi: '#171B22',
  hairline: '#1A1D23', hairline2: '#232831', edge: '#5C636E',
  fg: '#E4E8EE', fgBody: '#D5DAE1', fgStrong: '#B9BFC8', fg2: '#8C939E', fg3: '#767D89', fg4: '#5C636E', fgLink: '#5AA8E6',
  accent: '#5AA8E6', accent2: '#8CC4F0', accentInk: '#03121E', accentSoft: 'rgba(90,168,230,0.12)',
  brand: '#005799',
  danger: '#FF6E6E', warn: '#FFD16A', ok: '#7CFF8E', mention: '#FFD16A', mentionInk: '#1A1300',
  linkUnderline: 0 as const,
};

// Keep existing themes visually identical when adding the new keys:
export const backfill = {
  mesh: { fgBody: '#E8ECE8', fgStrong: '#A0A6A0', brand: '#7CFF8E' },
  light: { fgBody: '#14171A', fgStrong: '#3F453F', brand: '#1B7334' },
  'high-contrast': { fgBody: '#FFFFFF', fgStrong: '#E6E6E6', brand: '#9BFFA8' },
};

export const structuralSignal = {
  labelTrackingWide: '0.12em',
  radius: { sm: '4px', md: '6px', lg: '8px', xl: '10px', xxl: '12px', pill: '999px', avatar: '999px', bot: '6px' },
  shadow: { s1: '0 8px 20px rgba(0,0,0,0.4)', s2: '0 20px 50px rgba(0,0,0,0.55)', s3: '0 30px 80px rgba(0,0,0,0.65)' },
  motion: {
    press: '120ms', fast: '150ms', normal: '220ms', slow: '300ms', enter: '380ms', badge: '450ms',
    easeOut: 'cubic-bezier(0.16, 1, 0.3, 1)', toast: '220ms', pulse: '2s', caret: '1s', flash: '1.4s', meter: '120ms',
  },
  layout: {
    railW: '56px', railTile: '38px', slotW: '32px', sidebarW: '244px', membersW: '232px', threadW: '340px',
    topbarH: '2.75rem',       // 44px
    bottombarH: '1.625rem',   // unchanged
    channelHeaderH: '2.75rem',// 44px
    barH: '2.75rem',          // 44px
    panelH: '4rem',           // 64px: user panel AND voice call bar (top edges align)
    rowH: '1.75rem',          // 28px; 2.75rem on phone (base.css)
    rowHSub: '1.625rem',      // 26px voice occupant
    rowH2: '2.75rem',         // 44px two-line row
    listGap: '0.0625rem',     // 1px
  },
};

// Contrast pairs to add (see README 10.4).
export const TEXT_PAIRS_ADD = [
  ['fg2', 'surfaceHi'], ['fg3', 'bg'], ['fg3', 'bg2'], ['fg3', 'surface'], ['fg3', 'surface2'],
  ['fgBody', 'bg'], ['fgStrong', 'surface2'], ['accentInk', 'accent'], ['accentInk', 'accent2'],
  ['fgLink', 'bg'], ['mentionInk', 'mention'], ['ok', 'bg2'],
] as const;
export const UI_PAIRS_ADD = [
  ['accent', 'bg'], ['accent', 'bg2'], ['edge', 'bg'], ['edge', 'surface2'], ['danger', 'surface2'], ['ok', 'bg2'],
] as const;
