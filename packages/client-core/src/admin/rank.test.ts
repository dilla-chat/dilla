import { describe, expect, it } from 'vitest';
import { OWNER_TOP } from '../http/routes';
import type { RoleSummary } from '../state/types';
import { accessOf, canModerate, topOf } from './rank';
import { PERM } from './perm';

const role = (id: string, position: number): RoleSummary => ({ id, name: id, color: 0, position, hoist: false, mentionable: false, allow: 0, deny: 0, members: 0 });

describe('accessOf', () => {
  it('gives the owner everything, whatever the bits', () => {
    expect(accessOf(0n, OWNER_TOP, true)).toEqual({ top: OWNER_TOP, owner: true, manageCommunity: true, viewAuditLog: true, manageChannels: true,
      kickMembers: true, banMembers: true, moderateMembers: true, manageMessages: true, createInvite: true, manageRoles: true, loaded: true, everyoneId: '', overwritesLoaded: false });
  });
  it('gives an administrator everything', () => {
    const a = accessOf(BigInt(PERM.administrator), 3, false);
    expect(a).toEqual({ top: 3, owner: false, manageCommunity: true, viewAuditLog: true, manageChannels: true, kickMembers: true, banMembers: true,
      moderateMembers: true, manageMessages: true, createInvite: true, manageRoles: true, loaded: true, everyoneId: '', overwritesLoaded: false });
  });
  it('gives the default member nothing', () => {
    // view, send, read history, connect: bits 0, 1, 6, 7
    const bits = (1n << 0n) | (1n << 1n) | (1n << 6n) | (1n << 7n);
    expect(accessOf(bits, 0, false)).toEqual({ top: 0, owner: false, manageCommunity: false, viewAuditLog: false, manageChannels: false,
      kickMembers: false, banMembers: false, moderateMembers: false, manageMessages: false, createInvite: false, manageRoles: false, loaded: true, everyoneId: '', overwritesLoaded: false });
  });
  it('reads each permission from its own bit', () => {
    expect(accessOf(BigInt(PERM.kickMembers), 1, false)).toMatchObject({ kickMembers: true, banMembers: false, createInvite: false });
    expect(accessOf(BigInt(PERM.banMembers), 1, false)).toMatchObject({ kickMembers: false, banMembers: true });
    expect(accessOf(BigInt(PERM.manageChannels), 1, false)).toMatchObject({ manageChannels: true, manageCommunity: false });
    expect(accessOf(BigInt(PERM.manageCommunity), 1, false)).toMatchObject({ manageCommunity: true, manageRoles: false });
    expect(accessOf(BigInt(PERM.manageRoles), 1, false)).toMatchObject({ manageRoles: true, manageChannels: false });
    expect(accessOf(BigInt(PERM.createInvite), 1, false)).toMatchObject({ createInvite: true, kickMembers: false });
    expect(accessOf(BigInt(PERM.moderateMembers), 1, false)).toMatchObject({ moderateMembers: true });
    expect(accessOf(BigInt(PERM.manageMessages), 1, false)).toMatchObject({ manageMessages: true });
    expect(accessOf(BigInt(PERM.banMembers), 1, false)).toMatchObject({ moderateMembers: false });
    expect(accessOf(BigInt(PERM.viewAuditLog), 1, false)).toMatchObject({ viewAuditLog: true, manageCommunity: false });
    expect(accessOf(BigInt(PERM.manageCommunity), 1, false)).toMatchObject({ viewAuditLog: false });
  });
});

describe('topOf', () => {
  const roles = [role('a', 2), role('b', 7), role('c', 4)];
  it('is the highest position among the roles held', () => {
    expect(topOf(['a', 'c'], roles)).toBe(4);
    expect(topOf(['a', 'b', 'c'], roles)).toBe(7);
  });
  it('is 0 with no roles', () => {
    expect(topOf([], roles)).toBe(0);
  });
  it('counts an unknown role id as just under the owner, so nobody moderates through a role the client cannot see', () => {
    expect(topOf(['a', 'zz'], roles)).toBe(OWNER_TOP - 1);
  });
});

describe('canModerate', () => {
  const base = { me: 'me', target: 'them', owner: 'boss', myTop: 5, targetTop: 3 };
  it('is false for the owner as a target, even for the owner', () => {
    expect(canModerate({ ...base, target: 'boss' })).toBe(false);
    expect(canModerate({ ...base, me: 'boss', target: 'boss', myTop: OWNER_TOP })).toBe(false);
  });
  it('is false for oneself', () => {
    expect(canModerate({ ...base, target: 'me' })).toBe(false);
  });
  it('is true for the owner on anyone else, whatever the ranks', () => {
    expect(canModerate({ ...base, me: 'boss', myTop: 0, targetTop: 9 })).toBe(true);
  });
  it('is false with no rank', () => {
    expect(canModerate({ ...base, myTop: 0, targetTop: 0 })).toBe(false);
  });
  it('is false at an equal rank and true at a lower one', () => {
    expect(canModerate({ ...base, myTop: 5, targetTop: 5 })).toBe(false);
    expect(canModerate({ ...base, myTop: 5, targetTop: 6 })).toBe(false);
    expect(canModerate({ ...base, myTop: 5, targetTop: 4 })).toBe(true);
    expect(canModerate({ ...base, myTop: 1, targetTop: 0 })).toBe(true);
  });
  it('refuses a target whose role is unknown (topOf)', () => {
    const roles = [role('a', 2)];
    expect(canModerate({ ...base, myTop: topOf(['a'], roles), targetTop: topOf(['zz'], roles) })).toBe(false);
  });
});
