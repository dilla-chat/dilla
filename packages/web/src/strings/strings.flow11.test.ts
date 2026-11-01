// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
// @ts-expect-error the copy lint is a plain .mjs script without types
import { FORBIDDEN } from '../../../../scripts/check-ui-copy.mjs';
import { en } from './en.ts';

describe('flow 11 (v0.1.0-web3.7, slow mode, announcements and threads)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/11-slow-mode-announcements-threads.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-11 copy key equals en.ts byte for byte, and every new key is in the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    // Task 7 (Signal §5.5) adds thread.summary.last.
    expect(pairs.length).toBe(33);
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
    const listed = new Set(pairs.map(([key]) => key));
    const added = Object.keys(en).filter(k => /^(slow|announce|thread)\./.test(k));
    expect(added.filter(k => !listed.has(k))).toEqual([]);
  });
  it('sections: States, Keyboard map, Focus rules and Announcements have content, and States has 12 or more rows', () => {
    const body = (title: string) => {
      const start = doc.indexOf(`## ${title}\n`);
      expect(start, title).toBeGreaterThanOrEqual(0);
      const rest = doc.slice(start + title.length + 4);
      const next = rest.indexOf('\n## ');
      return (next < 0 ? rest : rest.slice(0, next)).trim();
    };
    for (const title of ['States', 'Keyboard map', 'Focus rules', 'Announcements']) expect(body(title).length, title).toBeGreaterThan(0);
    const rows = body('States').split('\n').filter(l => l.startsWith('|') && !/^\|[-| ]+\|$/.test(l));
    expect(rows.length - 1).toBeGreaterThanOrEqual(12);
  });
  const LOWER: readonly string[] = [
    'slow.off',
    'slow.badge',
    'announce.kind',
    'thread.open',
    'thread.close',
    // Task 9 fix round 1 (Ruling AL): the reply input reads the prototype's "Reply in thread".
    'thread.list',
  ];
  it('chrome: the lower-case chrome keys are lower case', () => {
    const table = en as Readonly<Record<string, string>>;
    expect(LOWER.length).toBe(6);
    for (const key of LOWER) expect(table[key], key).toBe(table[key].toLowerCase());
  });
});
