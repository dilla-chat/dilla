import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { PERM_ALL } from '../http/routes';
import {
  COMMUNITY_BITS, PERM, PERM_BY_KEY, PERM_GROUPS, ROLE_PERM_GROUPS, channelBitsOf, isPrivate, resolveChannel, resolveCommunity,
  type PermKey, type PermOverwrite,
} from './perm';

interface Vector {
  name: string; owner: string; everyone: string; user: string; community: number; channel: number;
  roles: { id: string; position: number; allow: number; deny: number }[]; held: string[];
  overwrites: { kind: 0 | 1; target: string; allow: number; deny: number }[];
}
const file = JSON.parse(readFileSync(new URL('../../../../protocol/vectors/permissions.json', import.meta.url), 'utf8')) as
  { bits: Record<string, number>; cases: Vector[] };

const camel = (s: string): string => s.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());
const NAMES: Record<string, string> = { manage_community: 'manageCommunity', screen_share: 'screenShare' };

describe('PERM', () => {
  it('pins all 26 values of internal/api/perm.go', () => {
    expect(PERM).toEqual({
      viewChannel: 1, sendMessages: 2, manageMessages: 4, pinMessages: 8, attachFiles: 16, addReactions: 32, readHistory: 64,
      connect: 128, speak: 256, video: 512, screenShare: 1024, createInvite: 2048, kickMembers: 4096, banMembers: 8192,
      manageChannels: 16384, manageRoles: 32768, manageCommunity: 65536, viewAuditLog: 131072, mentionEveryone: 262144,
      bypassSlowmode: 524288, administrator: 1048576, muteMembers: 2097152, moveMembers: 4194304, manageNicknames: 8388608,
      moderateMembers: 16777216, controlMusic: 33554432,
    });
  });
  it('equals the bits map of the vector file', () => {
    const fromFile = Object.fromEntries(Object.entries(file.bits).map(([k, v]) => [NAMES[k] ?? camel(k), v]));
    expect(fromFile).toEqual(PERM);
  });
  it('COMMUNITY_BITS is the seven community-scoped bits', () => {
    expect(COMMUNITY_BITS).toBe(4096 | 8192 | 65536 | 32768 | 131072 | 1048576 | 8388608 | 16777216);
  });
  it('PERM_GROUPS covers all 26 keys exactly once, and PERM_BY_KEY holds 26 distinct bits', () => {
    const keys = PERM_GROUPS.flatMap((g) => g.keys);
    expect(keys).toHaveLength(26);
    expect(new Set(keys)).toEqual(new Set(Object.keys(PERM_BY_KEY) as PermKey[]));
    expect(new Set(Object.values(PERM_BY_KEY)).size).toBe(26);
    expect(Object.values(PERM_BY_KEY).reduce((a, b) => a | b, 0)).toBe(PERM_ALL);
  });
  it('ROLE_PERM_GROUPS is the prototype\'s General, Moderation, Voice and Music and covers all 26 keys exactly once', () => {
    expect(ROLE_PERM_GROUPS.map((g) => g.id)).toEqual(['general', 'moderation', 'voice', 'music']);
    const keys = ROLE_PERM_GROUPS.flatMap((g) => g.keys);
    expect(keys).toHaveLength(26);
    expect(new Set(keys)).toEqual(new Set(Object.keys(PERM_BY_KEY) as PermKey[]));
    // The prototype's rows lead each group in its order; Control music is alone under Music.
    expect(ROLE_PERM_GROUPS[0].keys.slice(0, 4)).toEqual(['view', 'send', 'pin', 'everyone']);
    expect(ROLE_PERM_GROUPS[1].keys.slice(0, 3)).toEqual(['manageChannels', 'kick', 'ban']);
    expect(ROLE_PERM_GROUPS[2].keys.slice(0, 3)).toEqual(['connect', 'speak', 'screen']);
    expect(ROLE_PERM_GROUPS[3].keys).toEqual(['music']);
  });
  it('includes moderate_members in the members group and community mask', () => {
    expect(Object.values(PERM)).toContain(1 << 24);
    expect(PERM_GROUPS.find((g) => g.id === 'members')?.keys).toContain('moderate');
    expect(COMMUNITY_BITS & (1 << 24)).toBe(1 << 24);
  });
});

describe('the resolver against protocol/vectors/permissions.json', () => {
  it('has cases', () => {
    expect(file.cases.length).toBeGreaterThan(0);
  });
  for (const c of file.cases) {
    const input = { owner: c.owner, everyoneId: c.everyone, held: c.held,
      roles: c.roles.map((r) => ({ id: r.id, position: r.position, allow: r.allow, deny: r.deny })) };
    const overwrites: PermOverwrite[] = c.overwrites.map((o) => ({ kind: o.kind, targetId: o.target, allow: o.allow, deny: o.deny }));
    it(`${c.name}: community`, () => {
      expect(resolveCommunity(input, c.user)).toBe(c.community);
    });
    it(`${c.name}: channel`, () => {
      expect(resolveChannel(input, overwrites, c.user)).toBe(c.channel);
    });
  }
});

describe('resolver pins beyond the vectors', () => {
  it('equal_position_tie_by_id: reversing the roles array gives the same answers (the id breaks the tie)', () => {
    const c = file.cases.find((v) => v.name === 'equal_position_tie_by_id') as Vector;
    const overwrites: PermOverwrite[] = c.overwrites.map((o) => ({ kind: o.kind, targetId: o.target, allow: o.allow, deny: o.deny }));
    const roles = c.roles.map((r) => ({ id: r.id, position: r.position, allow: r.allow, deny: r.deny }));
    const input = { owner: c.owner, everyoneId: c.everyone, held: c.held, roles };
    const reversed = { ...input, roles: roles.slice().reverse() };
    expect(resolveCommunity(reversed, c.user)).toBe(c.community);
    expect(resolveChannel(reversed, overwrites, c.user)).toBe(c.channel);
  });
  it('a base without view stays 0 even when a member overwrite allows view (the base 0 short-circuit)', () => {
    const input = { owner: 'aa', everyoneId: 'ee', roles: [{ id: 'ee', position: 0, allow: PERM.sendMessages, deny: 0 }], held: [] as string[] };
    expect(resolveCommunity(input, 'bb')).toBe(0);
    expect(resolveChannel(input, [{ kind: 1, targetId: 'bb', allow: PERM.viewChannel, deny: 0 }], 'bb')).toBe(0);
  });
});

describe('channelBitsOf', () => {
  const s = { owner: 'aa', everyoneId: 'ee', roles: [{ id: 'ee', position: 0, allow: 3, deny: 0 }], members: [{ userId: 'bb', roleIds: [] }],
    overwrites: [] as PermOverwrite[] };
  it('is null for a non-member and everything for the owner', () => {
    expect(channelBitsOf(s, 'cc')).toBeNull();
    expect(channelBitsOf(s, 'aa')).toBe(PERM_ALL);
  });
  it('resolves a member through the overwrites', () => {
    expect(channelBitsOf(s, 'bb')).toBe(3);
    expect(channelBitsOf({ ...s, overwrites: [{ kind: 0, targetId: 'ee', allow: 0, deny: 1 }] }, 'bb')).toBe(0);
  });
});

describe('the AFK grant mirror (R35)', () => {
  const noView = { owner: 'aa', everyoneId: 'ee', roles: [{ id: 'ee', position: 0, allow: PERM.sendMessages, deny: PERM.viewChannel }],
    members: [{ userId: 'bb', roleIds: [] }], overwrites: [] as PermOverwrite[] };
  it('gives a member exactly view and connect in the AFK channel, and nothing without the flag', () => {
    expect(channelBitsOf(noView, 'bb')).toBe(0);
    expect(channelBitsOf({ ...noView, afk: true }, 'bb')).toBe(PERM.viewChannel | PERM.connect);
    expect(resolveChannel({ owner: 'aa', everyoneId: 'ee', roles: noView.roles, held: [] }, [], 'bb', true)).toBe(PERM.viewChannel | PERM.connect);
  });
  it('keeps the owner at every bit, and a non-member unknown', () => {
    expect(channelBitsOf({ ...noView, afk: true }, 'aa')).toBe(PERM_ALL);
    expect(channelBitsOf({ ...noView, afk: true }, 'cc')).toBeNull();
  });
});

describe('isPrivate', () => {
  it('is true for an @everyone deny of view only', () => {
    expect(isPrivate([{ kind: 0, targetId: 'ee', allow: 0, deny: PERM.viewChannel }], 'ee')).toBe(true);
    expect(isPrivate([{ kind: 0, targetId: 'ee', allow: 0, deny: PERM.sendMessages }], 'ee')).toBe(false);
    expect(isPrivate([{ kind: 0, targetId: 'rr', allow: 0, deny: PERM.viewChannel }], 'ee')).toBe(false);
    expect(isPrivate([{ kind: 1, targetId: 'ee', allow: 0, deny: PERM.viewChannel }], 'ee')).toBe(false);
  });
});
