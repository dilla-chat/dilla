// @vitest-environment node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

// R9 (web3.8): a landscape phone gets the phone layout, so every phone media query in packages/ui carries the same
// second clause, and the container rules that cannot see the viewport get a [data-layout="phone"] twin (R9, C-7).
// The web half (app.css) is pinned in packages/web's layout.test.ts.
const SRC = fileURLToPath(new URL('..', import.meta.url));
const PHONE_MEDIA = '@media (max-width: 47.99rem), (pointer: coarse) and (max-height: 31.99rem)';
const PHONE_ATTR = '[data-layout="phone"]';

function walk(dir: string, out: string[]): string[] {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (e.name.endsWith('.css')) out.push(p);
  }
  return out;
}
const files = walk(SRC, []).map(file => ({ file: path.relative(SRC, file), css: fs.readFileSync(file, 'utf8') }));

/** The at-rule preludes (everything from `@` to `{`) that mention 47.99rem. */
function preludes(css: string, kind: 'media' | 'container'): string[] {
  return [...css.matchAll(new RegExp(`@${kind}[^{]*\\{`, 'g'))].map(m => m[0].slice(0, -1).trim()).filter(p => p.includes('47.99rem'));
}

/** The body of the block opened by the `{` at `open` (balanced braces). */
function block(css: string, open: number): string {
  let depth = 0;
  for (let i = open; i < css.length; i += 1) {
    if (css[i] === '{') depth += 1;
    else if (css[i] === '}' && (depth -= 1) === 0) return css.slice(open + 1, i);
  }
  throw new Error('unbalanced braces');
}

/** The flat rules `selector { body }` of a CSS fragment, comments removed. */
function rules(css: string): Array<{ selector: string; body: string }> {
  const bare = css.replace(/\/\*[\s\S]*?\*\//g, '');
  return [...bare.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map(m => ({ selector: m[1].trim(), body: m[2].trim() }));
}

describe('R9: the phone media query', () => {
  it('every @media rule with 47.99rem in packages/ui reads the full phone query', () => {
    const found = files.flatMap(f => preludes(f.css, 'media').map(p => ({ file: f.file, prelude: p })));
    expect(found.length).toBe(8); // Task 10: SettingsRow and VoiceSettings left the tier (prototype sizes on a phone). SettingsFrame, and since Signal task 8 CallStage, CallTile and RingBanner, get phone geometry from the shell's data-layout.
    for (const { file, prelude } of found) expect(`${file}: ${prelude}`).toBe(`${file}: ${PHONE_MEDIA}`);
  });

  it('the eight rules R9 names are all there', () => {
    const named = ['AttachmentCard/AttachmentCard.css', 'MediaPlayer/MediaPlayer.css', 'CallControls/CallControls.css', 'styles/base.css'];
    for (const n of named) expect(files.find(f => f.file === n)?.css.includes(PHONE_MEDIA), n).toBe(true);
    expect(files.find(f => f.file === 'styles/base.css')?.css.split(PHONE_MEDIA).length).toBe(2);
  });
});

describe('R9: the @container twins', () => {
  const containers = files.flatMap(f => {
    const out: Array<{ file: string; css: string; body: ReturnType<typeof rules> }> = [];
    for (const m of f.css.matchAll(/@container d-shell \(max-width: 47\.99rem\) \{/g)) {
      out.push({ file: f.file, css: f.css, body: rules(block(f.css, m.index + m[0].length - 1)) });
    }
    return out;
  });

  // Task 7: MessageRow.css lost its two blocks (the toolbar floats and the thread chip is 24px at every width, as in the prototype).
  it('finds the one @container d-shell block (base.css)', () => {
    expect(containers.map(c => c.file).sort()).toEqual(['styles/base.css']);
    expect(containers.every(c => c.body.length > 0)).toBe(true);
  });

  it('every rule in them has a [data-layout="phone"] twin with identical declarations', () => {
    for (const c of containers) {
      // Outside every @container: the file with its container blocks cut out.
      const flat = rules(c.css.replace(/@(container|media)[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}/g, ''));
      for (const r of c.body) {
        const twin = flat.find(t => t.selector.includes(PHONE_ATTR) && t.body === r.body
          && t.selector.replace(PHONE_ATTR, '').trim() === r.selector);
        expect(twin, `${c.file}: ${r.selector}`).toBeDefined();
      }
    }
  });
});

describe('m6: dialogs follow the visual viewport on a phone', () => {
  const rule = (file: string, selector: string): string => {
    const css = files.find(f => f.file === file)!.css;
    const at = css.indexOf(PHONE_MEDIA);
    expect(at, `${file} has the phone media`).toBeGreaterThanOrEqual(0);
    return rules(block(css, css.indexOf('{', at))).find(r => r.selector === selector)?.body ?? '';
  };
  it.each([['Dialog/Dialog.css', '.d-dialog:modal'], ['Lightbox/Lightbox.css', '.d-lightbox:modal']])('%s: %s', (file, selector) => {
    const body = rule(file, selector);
    expect(body).toContain('top: var(--d-vvtop, 0px)');
    expect(body).toContain('max-height: calc(var(--d-vvh, 100dvh) - 2rem)');
  });
});
