// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
// @ts-expect-error the copy lint is a plain .mjs script without types
import { FORBIDDEN } from '../../../../scripts/check-ui-copy.mjs';
import { en } from './en.ts';

describe('flow 14 (v0.1.0-web3.9, layout grammar, Discord layout and the built-in AFK channel)', () => {
  const doc = readFileSync(fileURLToPath(new URL('../../../../docs/design/flows/14-layout-grammar-and-afk.md', import.meta.url)), 'utf8');
  const copy = doc.slice(doc.indexOf('## Copy'));
  const pairs = [...copy.matchAll(/`([a-zA-Z][\w.]*)='([^`]*)'`/g)].map(m => [m[1], m[2]] as const);
  it('strings: every flow-14 copy key equals en.ts byte for byte, and every new key is in the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    expect(pairs.length).toBe(53);
    // M2 (Jonas): a call cut because AFK was chosen on another device says so, calmly.
    expect(table['afk.elsewhere']).toBe('You went AFK on another device.');
    // AW5/AW6 fix round 1: the AFK page's button, its away line, its empty roster, and a refused away mark.
    expect(table['afk.go']).toBe('go AFK');
    expect(table['afk.youAway']).toBe('You’re away. Join a voice channel, or send a message or type in this server, to come back.');
    expect(table['afk.empty']).toBe('Nobody is away.');
    expect(table['afk.failed']).toBe('Could not go AFK. Try again.');
    // AW6: the AFK channel hosts no call, so its stage line and the failed move's join are gone; `afk.away` is new.
    expect(table['afk.away']).toBe('away');
    expect(table['afk.stage.muted']).toBeUndefined();
    expect(table['afk.moved.join']).toBeUndefined();
    for (const [key, value] of pairs) expect(table[key], key).toBe(value);
    // The curly apostrophe U+2019, pinned by code point so a straight one goes red in en.ts and in the flow.
    expect(table['afk.channel.note'].codePointAt(18)).toBe(0x2019);
    expect(table['afk.channel.note']).toBe('This is the server’s AFK channel. Nobody is connected here; people who are away are listed.');
    expect(pairs.find(([key]) => key === 'layout.ptt.tag')?.[1]).toBe('PTT');
    expect(en['layout.ptt.tag']).toBe('PTT');
    expect(pairs.find(([key]) => key === 'afk.channel.note')?.[1]).toBe('This is the server’s AFK channel. Nobody is connected here; people who are away are listed.');
    for (const [key, value] of pairs) {
      for (const f of FORBIDDEN as { word: string; re: RegExp }[]) expect(f.re.test(value), `${key}: ${f.word}`).toBe(false);
    }
    const listed = new Set(pairs.map(([key]) => key));
    const added = Object.keys(en).filter(k => /^(afk|layout|home)\./.test(k));
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
    const keyboard = body('Keyboard map');
    expect(keyboard).toContain('`aria-expanded`');
    expect(keyboard).toContain('Ctrl+End');
    const states = body('States');
    expect(states).not.toContain('listen-only');
    expect(states).toContain('`afk.away`');
    expect(body('Announcements')).toContain('Moved to');
    const rows = states.split('\n').filter(l => l.startsWith('|') && !/^\|[-| ]+\|$/.test(l));
    expect(rows.length - 1).toBeGreaterThanOrEqual(20);
  });
  const LOWER: readonly string[] = [
    'afk.timeout.label',
    'afk.away',
    'layout.members.online',
    'layout.members.offline',
    'layout.log.jump',
    'layout.log.today',
    'layout.log.yesterday',
    'layout.header.more',
    'layout.category.add',
    'layout.rail.add',
    'layout.add.title',
    'layout.add.join',
    'layout.add.create',
  ];
  it('the presence sections are bare words: the member list draws the count', () => {
    const table = en as Readonly<Record<string, string>>;
    expect(table['layout.members.online']).toBe('online');
    expect(table['layout.members.offline']).toBe('offline');
    expect(pairs.find(([key]) => key === 'layout.members.online')?.[1]).toBe('online');
  });
  it('the profile card words are lower case and match the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    const want = { owner: 'owner', you: 'you', roles: 'roles', close: 'close', bot: 'bot', manageRoles: 'edit roles', formerMember: 'former member' };
    for (const [k, v] of Object.entries(want)) {
      expect(table[`layout.profile.${k}`]).toBe(v);
      expect(pairs.find(([key]) => key === `layout.profile.${k}`)?.[1]).toBe(v);
    }
  });
  it('the presence words of the member list and the card are lower case and match the flow', () => {
    const table = en as Readonly<Record<string, string>>;
    const want = { online: 'online', idle: 'idle', dnd: 'do not disturb', offline: 'offline', invisible: 'invisible' };
    for (const [k, v] of Object.entries(want)) {
      expect(table[`presence.${k}`]).toBe(v);
      expect(pairs.find(([key]) => key === `presence.${k}`)?.[1]).toBe(v);
    }
  });
  it('chrome: the lower-case chrome keys are lower case', () => {
    const table = en as Readonly<Record<string, string>>;
    // Ruling AA: home.tab is the prototype's "Home".
    expect(LOWER.length).toBe(13);
    for (const key of LOWER) expect(table[key], key).toBe(table[key].toLowerCase());
  });
});
