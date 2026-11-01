import { describe, expect, it } from 'vitest';
import type { CommunityAccess, MemberSummary, RoleSummary } from '@dilla/client-core';
import { memberGroups, menuFor } from './members-model.ts';

const id = (n: number) => n.toString(16).padStart(2, '0').repeat(16);
const OWNER = id(1), ME = id(2), BOB = id(3), CAL = id(4), BOT = id(5);
const MOD: RoleSummary = { id: id(10), name: 'mods', color: 0, position: 5, hoist: true, mentionable: false, allow: 0, deny: 0, members: 0 };
const CREW: RoleSummary = { id: id(11), name: 'crew', color: 0, position: 2, hoist: true, mentionable: false, allow: 0, deny: 0, members: 0 };
const QUIET: RoleSummary = { id: id(12), name: 'quiet', color: 0, position: 9, hoist: false, mentionable: false, allow: 0, deny: 0, members: 0 };
const ROLES = [CREW, MOD, QUIET];
const m = (userId: string, username: string, roleIds: string[] = [], kind: 0 | 1 = 0, display = ''): MemberSummary => ({ userId, username, display, kind, roleIds });
const MEMBERS = [m(OWNER, 'olof'), m(ME, 'ada', [CREW.id]), m(BOB, 'bob', [CREW.id, MOD.id]), m(CAL, 'carol', [QUIET.id], 0, 'Åsa'), m(BOT, 'robo', [], 1)];
const EVERYONE = 'ab'.repeat(16);
const access = (over: Partial<CommunityAccess> = {}): CommunityAccess => ({ top: 5, owner: false, manageCommunity: false, manageChannels: false,
  kickMembers: true, banMembers: true, createInvite: false, manageRoles: false, loaded: true, overwritesLoaded: true, everyoneId: EVERYONE, ...over });

describe('memberGroups', () => {
  it('moves offline members into one bottom group regardless of role', () => {
    const presence = { [OWNER]: 'offline', [ME]: 'online', [BOB]: 'dnd', [CAL]: 'idle', [BOT]: 'offline' } as const;
    const groups = memberGroups(MEMBERS, ROLES, OWNER, ME, presence);
    expect(groups.map(g => [g.id, g.members.map(r => r.userId)])).toEqual([
      [MOD.id, [BOB]], [CREW.id, [ME]], ['members', [CAL]], ['bots', [BOT]], ['offline', [OWNER]],
    ]);
  });
  it('groups by the highest hoisted role, highest first, and the rest last; an unhoisted role does not group', () => {
    const groups = memberGroups(MEMBERS, ROLES, OWNER, ME);
    expect(groups.map(g => [g.id, g.members.map(r => r.userId)])).toEqual([[MOD.id, [BOB]], [CREW.id, [ME]], ['members', [CAL, OWNER]], ['bots', [BOT]]]);
  });
  it('sorts names with localeCompare', () => {
    const rest = memberGroups(MEMBERS, ROLES, OWNER, ME).find(g => g.id === 'members')!;
    expect(rest.members.map(r => r.name)).toEqual(['Åsa', 'olof']);
  });
  it('tags the owner, you and bots', () => {
    const tags = Object.fromEntries(memberGroups(MEMBERS, ROLES, OWNER, ME).flatMap(g => g.members).map(r => [r.userId, r.tags]));
    expect(tags[OWNER]).toEqual(['owner']);
    expect(tags[ME]).toEqual(['you']);
    expect(tags[BOT]).toEqual(['bot']);
    expect(tags[BOB]).toEqual([]);
  });
});

describe('menuFor', () => {
  const ctx = { selfId: ME, ownerId: OWNER, roles: ROLES };
  const bob = MEMBERS[2];
  it('offers message, compare, kick and ban on a lower-ranked member', () => {
    expect(menuFor(m(CAL, 'carol', [CREW.id]), access(), ctx)).toEqual(['message', 'compare', 'kick', 'ban']);
  });
  it('has no message, compare, kick or ban on self', () => {
    expect(menuFor(MEMBERS[1], access(), ctx)).toEqual([]);
    expect(menuFor(MEMBERS[1], access({ manageRoles: true }), ctx)).toEqual(['roles']);
  });
  it('hides kick and ban on the owner and on a higher-ranked target', () => {
    expect(menuFor(MEMBERS[0], access(), ctx)).toEqual(['message', 'compare']);
    expect(menuFor(bob, access({ top: 2 }), ctx)).toEqual(['message', 'compare']);
    expect(menuFor(bob, access({ top: 5 }), ctx)).toEqual(['message', 'compare']);
  });
  it('follows each permission, and shows neither before permissions load', () => {
    const lower = m(CAL, 'carol');
    expect(menuFor(lower, access({ banMembers: false }), ctx)).toEqual(['message', 'compare', 'kick']);
    expect(menuFor(lower, access({ kickMembers: false }), ctx)).toEqual(['message', 'compare', 'ban']);
    expect(menuFor(lower, access({ loaded: false }), ctx)).toEqual(['message', 'compare']);
    expect(menuFor(lower, undefined, ctx)).toEqual(['message', 'compare']);
  });
  it('offers text timeout only with moderate_members and a lower-ranked target', () => {
    const lower = m(CAL, 'carol', [CREW.id]);
    expect(menuFor(lower, access({ moderateMembers: true }), ctx)).toEqual(['message', 'compare', 'warn', 'timeout', 'kick', 'ban']);
    expect(menuFor(lower, access({ moderateMembers: false }), ctx)).toEqual(['message', 'compare', 'kick', 'ban']);
    expect(menuFor(MEMBERS[0], access({ moderateMembers: true }), ctx)).toEqual(['message', 'compare']);
    expect(menuFor(bob, access({ moderateMembers: true, top: 5 }), ctx)).toEqual(['message', 'compare']);
  });
});

describe('role assignment in the menu and the group colour', () => {
  const ctx = { selfId: ME, ownerId: OWNER, roles: ROLES };
  it('adds roles after compare and before kick only with manageRoles', () => {
    const lower = m(CAL, 'carol');
    expect(menuFor(lower, access({ manageRoles: true }), ctx)).toEqual(['message', 'compare', 'roles', 'kick', 'ban']);
    expect(menuFor(lower, access({ manageRoles: false }), ctx)).toEqual(['message', 'compare', 'kick', 'ban']);
    expect(menuFor(lower, access({ manageRoles: true, loaded: false }), ctx)).toEqual(['message', 'compare']);
  });
  it('colours a member by the highest coloured role, 0 giving null', () => {
    const red: RoleSummary = { ...CREW, id: id(20), position: 3, color: 0xE5484D };
    const blue: RoleSummary = { ...QUIET, id: id(21), position: 4, color: 0x0090FF };
    const plain: RoleSummary = { ...MOD, id: id(22), position: 8, color: 0 };
    const rows = memberGroups([m(BOB, 'bob', [red.id, blue.id, plain.id]), m(CAL, 'carol', [plain.id])], [red, blue, plain], OWNER, ME)
      .flatMap(g => g.members);
    expect(rows.find(r => r.userId === BOB)?.color).toBe(0x0090FF);
    expect(rows.find(r => r.userId === CAL)?.color).toBeNull();
  });
});
