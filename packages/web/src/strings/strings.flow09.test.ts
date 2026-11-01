// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
// @ts-expect-error the copy lint is a plain .mjs script without types
import { FORBIDDEN } from '../../../../scripts/check-ui-copy.mjs';
import { en } from './en.ts';

describe('flow 09 (v0.1.0-web3.5, roles and private channels)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/09-roles-and-private-channels.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-09 copy key equals en.ts byte for byte, and every new key is in the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
    expect(pairs.length).toBe(185); // Task 10c fix round 1: perm.group.moderation
    const listed = new Set(pairs.map(([key]) => key));
    const added = Object.keys(en).filter(k => /^(roles|perm|chperm|mention|serverNotify)\./.test(k));
    expect(added.filter(k => !listed.has(k))).toEqual([]);
  });
  it('sections: States, Keyboard map, Focus rules and Announcements have content, and States has 20 or more rows', () => {
    const body = (title: string) => {
      const start = doc.indexOf(`## ${title}\n`);
      expect(start, title).toBeGreaterThanOrEqual(0);
      const rest = doc.slice(start + title.length + 4);
      const next = rest.indexOf('\n## ');
      return (next < 0 ? rest : rest.slice(0, next)).trim();
    };
    for (const title of ['States', 'Keyboard map', 'Focus rules', 'Announcements']) expect(body(title).length, title).toBeGreaterThan(0);
    const rows = body('States').split('\n').filter(l => l.startsWith('|') && !/^\|[-| ]+\|$/.test(l));
    expect(rows.length - 1).toBeGreaterThanOrEqual(20);
  });
  const LOWER: readonly string[] = [
    'roles.tab', 'roles.new', 'roles.edit', 'roles.up',
    'roles.down', 'roles.delete', 'roles.locked', 'roles.menu.assign',
    'roles.editor.hoist', 'roles.editor.mentionable', 'roles.color.none', 'roles.color.red',
    'roles.color.orange', 'roles.color.amber', 'roles.color.green', 'roles.color.teal',
    'roles.color.cyan', 'roles.color.blue', 'roles.color.indigo', 'roles.color.purple',
    'roles.color.pink', 'roles.color.brown', 'roles.color.grey', 'roles.color.custom',
    'perm.view', 'perm.send', 'perm.manageMessages', 'perm.pin',
    'perm.attach', 'perm.react', 'perm.history', 'perm.connect',
    'perm.speak', 'perm.video', 'perm.screen', 'perm.invite',
    'perm.kick', 'perm.ban', 'perm.manageChannels', 'perm.manageRoles',
    'perm.manageServer', 'perm.auditLog', 'perm.everyone', 'perm.slowmode',
    'perm.admin', 'perm.mute', 'perm.move', 'perm.nicknames',
    'perm.allow', 'perm.inherit', 'perm.deny', 'chperm.private',
    'chperm.add', 'chperm.remove', 'chperm.privateTag', 'chperm.member',
    'chperm.create.private', 'serverNotify.mode.default', 'serverNotify.mode.all', 'serverNotify.mode.mentions',
    'serverNotify.mode.nothing', 'serverNotify.mute', 'serverNotify.suppressEveryone',
  ];
  it('chrome: 63 lower-case keys are pinned', () => { expect(LOWER.length).toBe(63); });
// Signal task 10 fix round 1 (Jonas, prototype wins): settings chrome follows the prototype's sentence case.
  it('chrome: the pinned chrome keys keep their case (lower case, or the settings sentence case of Signal task 10)', () => {
    const table = en as Readonly<Record<string, string>>;
    for (const key of LOWER) {
      const value = table[key];
      expect(value, key).toBe(/^(perm|serverNotify)\./.test(key) && !/^perm\.group\./.test(key) ? value.charAt(0).toUpperCase() + value.slice(1).toLowerCase() : value.toLowerCase());
    }
  });
});
