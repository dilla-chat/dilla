// @vitest-environment node
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, it, expect } from 'vitest';

const read = (name: string) => readFileSync(fileURLToPath(new URL(name, import.meta.url)), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
const rule = (css: string, sel: string) => css.match(new RegExp(`${sel.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')} \\{[^}]*\\}`))?.[0] ?? '';

// Ruling J: a form field label is sentence case, 11px, #8C939E (real --fg-2) - the prototype's, not the micro-label style.
describe('form field labels follow the prototype (CSS pin)', () => {
  it.each([
    ['../TextField/TextField.css', '.d-text-field__label'],
    ['../Select/Select.css', '.d-select__label'],
  ])('%s %s', (file, sel) => {
    const r = rule(read(file), sel);
    expect(r).toContain('font-size: var(--text-xs)');
    expect(r).toContain('color: var(--fg-2)');
    expect(r).not.toMatch(/uppercase|letter-spacing/);
  });
});

// Ruling I: the on knob is white, from --knob.
describe('toggle on-knob', () => {
  it('uses --knob', () => {
    expect(read('../Toggle/Toggle.css')).toMatch(/aria-checked="true"\] \.d-toggle__knob \{[^}]*background: var\(--knob\)/);
  });
});
