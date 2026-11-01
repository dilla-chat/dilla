// @vitest-environment node
// web3.8 FX (final review, the cramped phone stage): jsdom cannot lay out, so the phone rules are pinned as text; the docs
// shoot (voice-stage-phone.png) shows them in a real browser.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const stage = readFileSync(fileURLToPath(new URL('./CallStage.css', import.meta.url)), 'utf8');
const tile = readFileSync(fileURLToPath(new URL('../CallTile/CallTile.css', import.meta.url)), 'utf8');
const PHONE = '@media (max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem) {';

/** The bodies of every phone @media block of `css`, joined (kept for later pins). */
function phone(css: string): string {
  const out: string[] = [];
  let at = css.indexOf(PHONE);
  while (at >= 0) {
    let depth = 0;
    let end = at + PHONE.length - 1;
    for (; end < css.length; end += 1) {
      if (css[end] === '{') depth += 1;
      else if (css[end] === '}' && (depth -= 1) === 0) break;
    }
    out.push(css.slice(at + PHONE.length, end));
    at = css.indexOf(PHONE, end);
  }
  return out.join('\n');
}

/** The declarations of the last rule in `css` whose selector list contains `selector`. */
function rule(css: string, selector: string): string {
  const all = [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .filter(m => m[1].split(',').map(s => s.trim()).includes(selector));
  if (all.length === 0) throw new Error(`no rule for ${selector}`);
  return all[all.length - 1][2];
}

describe('Signal §5.6, the prototype\'s stage (CSS pins; the real-browser geometry is in scripts/story-a11y.ts)', () => {
  it('the grid is a wrapping, centred row of 16:9 tiles at the prototype\'s widths by count', () => {
    expect(rule(stage, '.d-call-stage__tiles[data-tiles="stage"]')).toMatch(/flex-wrap: wrap; align-content: safe center; align-items: safe center; justify-content: safe center;/);
    expect(rule(stage, '.d-call-stage__tile[data-stage]')).toMatch(/width: min\(31\.5%, 21\.25rem\); aspect-ratio: 16 \/ 9;/);
    expect(rule(stage, '.d-call-stage__tiles[data-band="two"] > .d-call-stage__tile[data-stage]')).toMatch(/width: min\(46%, 35rem\)/);
    expect(rule(stage, '.d-call-stage__tiles[data-band="four"] > .d-call-stage__tile[data-stage]')).toMatch(/width: min\(46%, 26\.25rem\)/);
  });

  it('focused: the tile is 16:9 at the row\'s height (never wider than the stage), the filmstrip 188px tiles (150px on a phone)', () => {
    expect(rule(stage, '.d-call-stage__tile[data-focused="true"] > .d-call-tile[data-stage]')).toMatch(/width: min\(100%, 100cqh \* 16 \/ 9\); height: 100%;/);
    expect(stage).toMatch(/--d-strip-w: 11\.75rem/);
    expect(rule(stage, '[data-layout="phone"] .d-call-stage')).toMatch(/--d-strip-w: 9\.375rem/);
  });

  it('the call bar is the 64px panel height with a hairline above on --bg-2; the phone shows it centred without the room', () => {
    expect(rule(stage, '.d-call-stage__controls[data-bar]')).toMatch(/min-height: var\(--panel-h\);[^}]*border-top: 1px solid var\(--hairline\); background: var\(--bg-2\)/);
    expect(rule(stage, '[data-layout="phone"] .d-call-stage__controls[data-bar]')).toMatch(/justify-content: center/);
    expect(rule(stage, '[data-layout="phone"] .d-call-stage__where')).toMatch(/display: none/);
  });

  it('the stage header slot is the top bar with the hairline at its foot', () => {
    expect(rule(stage, '.d-call-stage__bar')).toMatch(/height: var\(--bar-h\)/);
    expect(rule(stage, '.d-call-stage__bar')).toMatch(/border-bottom: 1px solid var\(--hairline\)/);
  });

  it('a tile is --stage-tile with a 1px --hairline edge and --r-xl corners', () => {
    expect(rule(tile, '.d-call-tile[data-stage]')).toMatch(/background: var\(--stage-tile\); border-radius: var\(--r-xl\); box-shadow: inset 0 0 0 1px var\(--hairline\);/);
  });
});
