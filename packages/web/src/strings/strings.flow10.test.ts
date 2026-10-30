// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
// @ts-expect-error the copy lint is a plain .mjs script without types
import { FORBIDDEN } from '../../../../scripts/check-ui-copy.mjs';
import { en } from './en.ts';

describe('flow 10 (v0.1.0-web3.6, second factor)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/10-second-factor.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-10 copy key equals en.ts byte for byte, and every new key is in the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    expect(pairs.length).toBe(139); // Wave B task 3b: the sign-up's factor and code steps in the prototype's words
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
    const listed = new Set(pairs.map(([key]) => key));
    const added = Object.keys(en).filter(k => /^(factor|factorSignin|factorSettings)\./.test(k));
    expect(added.filter(k => !listed.has(k))).toEqual([]);
  });
  it('sections: States, Keyboard map, Focus rules and Announcements have content, and States has 22 or more rows', () => {
    const body = (title: string) => {
      const start = doc.indexOf(`## ${title}\n`);
      expect(start, title).toBeGreaterThanOrEqual(0);
      const rest = doc.slice(start + title.length + 4);
      const next = rest.indexOf('\n## ');
      return (next < 0 ? rest : rest.slice(0, next)).trim();
    };
    for (const title of ['States', 'Keyboard map', 'Focus rules', 'Announcements']) expect(body(title).length, title).toBeGreaterThan(0);
    const rows = body('States').split('\n').filter(l => l.startsWith('|') && !/^\|[-| ]+\|$/.test(l));
    expect(rows.length - 1).toBeGreaterThanOrEqual(22);
  });
  const LOWER: readonly string[] = [
    'factorSettings.nav',
    'factorSettings.totp.add',
    'factorSettings.totp.remove',
    'factorSettings.totp.removeLabel',
    'factorSettings.passkeys.add',
    'factorSettings.passkeys.remove',
    'factorSettings.passkeys.removeLabel',
    'factorSettings.codes.make',
  ];
// Signal task 10 fix round 1 (Jonas, prototype wins): settings chrome follows the prototype's sentence case. Wave B task 3b:
// so do the choice's cards ("Authenticator app", "Passkey"), the prototype's new-account step 4.
  it('chrome: the settings chrome keys are sentence case, the others lower case', () => {
    const table = en as Readonly<Record<string, string>>;
    expect(LOWER.length).toBe(8);
    for (const key of LOWER) expect(table[key], key).toMatch(key.startsWith('factorSettings.') ? /^[A-Z]/ : /^[a-z]/);
  });
});
