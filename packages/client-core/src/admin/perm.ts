// The permission resolver of internal/api/perm.go, ported line for line. Checked against protocol/vectors/permissions.json.
// The client's answer is advisory: the instance decides every request again.
import { PERM_ALL } from '../http/routes';

/** The 26 permission bits of internal/api/perm.go (Perm*), as numbers (bit 25 fits in 32). */
export const PERM = {
  viewChannel: 1 << 0, sendMessages: 1 << 1, manageMessages: 1 << 2, pinMessages: 1 << 3, attachFiles: 1 << 4,
  addReactions: 1 << 5, readHistory: 1 << 6, connect: 1 << 7, speak: 1 << 8, video: 1 << 9, screenShare: 1 << 10,
  createInvite: 1 << 11, kickMembers: 1 << 12, banMembers: 1 << 13, manageChannels: 1 << 14, manageRoles: 1 << 15,
  manageCommunity: 1 << 16, viewAuditLog: 1 << 17, mentionEveryone: 1 << 18, bypassSlowmode: 1 << 19,
  administrator: 1 << 20, muteMembers: 1 << 21, moveMembers: 1 << 22, manageNicknames: 1 << 23, moderateMembers: 1 << 24,
  controlMusic: 1 << 25,
} as const;

/** The bits a channel overwrite can neither grant nor remove (perm.go communityBits). */
export const COMMUNITY_BITS = PERM.kickMembers | PERM.banMembers | PERM.manageCommunity | PERM.manageRoles
  | PERM.viewAuditLog | PERM.administrator | PERM.manageNicknames | PERM.moderateMembers;

/** The bit of each copy key (`perm.<key>`). */
export const PERM_BY_KEY = {
  view: PERM.viewChannel, send: PERM.sendMessages, manageMessages: PERM.manageMessages, pin: PERM.pinMessages,
  attach: PERM.attachFiles, react: PERM.addReactions, history: PERM.readHistory, connect: PERM.connect, speak: PERM.speak,
  video: PERM.video, screen: PERM.screenShare, invite: PERM.createInvite, kick: PERM.kickMembers, ban: PERM.banMembers,
  manageChannels: PERM.manageChannels, manageRoles: PERM.manageRoles, manageServer: PERM.manageCommunity,
  auditLog: PERM.viewAuditLog, everyone: PERM.mentionEveryone, slowmode: PERM.bypassSlowmode, admin: PERM.administrator,
  mute: PERM.muteMembers, move: PERM.moveMembers, nicknames: PERM.manageNicknames, moderate: PERM.moderateMembers,
  music: PERM.controlMusic,
} as const;
export type PermKey = keyof typeof PERM_BY_KEY;

export const PERM_GROUPS: readonly { id: 'general' | 'members' | 'text' | 'voice' | 'music'; keys: readonly PermKey[] }[] = [
  { id: 'general', keys: ['view', 'manageChannels', 'manageRoles', 'manageServer', 'invite', 'auditLog', 'admin'] },
  { id: 'members', keys: ['kick', 'ban', 'nicknames', 'moderate'] },
  { id: 'text', keys: ['send', 'attach', 'react', 'history', 'pin', 'everyone', 'manageMessages', 'slowmode'] },
  { id: 'voice', keys: ['connect', 'speak', 'video', 'screen', 'mute', 'move'] },
  // control_music (Task 10c): the prototype's own "Music" group under Voice.
  { id: 'music', keys: ['music'] },
];

/**
 * The role editor's groups are the prototype's four headings, General, Moderation, Voice and Music (Ruling AS), with
 * every other dilla permission folded into the first three. Each group starts with the prototype's rows in its order,
 * and Control music is under Music. Channel permissions keep PERM_GROUPS.
 */
export const ROLE_PERM_GROUPS: readonly { id: 'general' | 'moderation' | 'voice' | 'music'; keys: readonly PermKey[] }[] = [
  { id: 'general', keys: ['view', 'send', 'pin', 'everyone', 'attach', 'react', 'history', 'invite'] },
  { id: 'moderation', keys: ['manageChannels', 'kick', 'ban', 'manageMessages', 'moderate', 'nicknames', 'slowmode', 'manageRoles', 'manageServer', 'auditLog', 'admin'] },
  { id: 'voice', keys: ['connect', 'speak', 'screen', 'video', 'mute', 'move'] },
  { id: 'music', keys: ['music'] },
];

export interface PermRole { id: string; position: number; allow: number; deny: number }
export interface PermOverwrite { kind: 0 | 1; targetId: string; allow: number; deny: number }
export interface PermMember { userId: string; roleIds: readonly string[] }
export interface PermInput { owner: string; everyoneId: string; roles: readonly PermRole[]; held: readonly string[] }

/** Position, then id (lower-case hex compares as the bytes do). */
function byPositionThenId(a: PermRole, b: PermRole): number {
  return a.position - b.position || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
}

/** Deny first, then allow: an allow on the same bit wins (perm.go `&^= deny`, `|= allow`). */
function apply(bits: number, allow: number, deny: number): number {
  return ((bits & ~(deny & PERM_ALL)) | (allow & PERM_ALL)) & PERM_ALL;
}

function finish(bits: number): number {
  if ((bits & PERM.administrator) !== 0) return PERM_ALL;
  if ((bits & PERM.viewChannel) === 0) return 0;
  return bits;
}

function heldRoles(input: PermInput): PermRole[] {
  const held = new Set(input.held);
  return input.roles.filter((r) => r.id === input.everyoneId || held.has(r.id)).sort(byPositionThenId);
}

/** perm.go Resolve: the owner short-circuit, @everyone, then each held role in ascending position. */
export function resolveCommunity(input: PermInput, userId: string): number {
  if (userId === input.owner) return PERM_ALL;
  let bits = 0;
  for (const r of heldRoles(input)) bits = apply(bits, r.allow, r.deny);
  return finish(bits);
}

/** What resolveChannel needs of one user that no channel changes: the community bits and the held role ids in ascending
 *  position (@everyone first). Resolved once per user, it serves every channel (web3.5 C4 fix round 1). */
export interface ResolvedUser { userId: string; base: number; held: readonly string[] }

export function resolveUser(input: PermInput, userId: string): ResolvedUser {
  const held = heldRoles(input);
  if (userId === input.owner) return { userId, base: PERM_ALL, held: held.map((r) => r.id) };
  let bits = 0;
  for (const r of held) bits = apply(bits, r.allow, r.deny);
  return { userId, base: finish(bits), held: held.map((r) => r.id) };
}

/** One channel's overwrites by kind and target, each target's entries in list order. */
export interface OverwriteIndex { roles: ReadonlyMap<string, readonly PermOverwrite[]>; users: ReadonlyMap<string, readonly PermOverwrite[]> }

export function indexOverwrites(overwrites: readonly PermOverwrite[]): OverwriteIndex {
  const roles = new Map<string, PermOverwrite[]>();
  const users = new Map<string, PermOverwrite[]>();
  for (const o of overwrites) {
    const into = o.kind === 0 ? roles : o.kind === 1 ? users : null;
    if (into === null) continue;
    const list = into.get(o.targetId);
    if (list === undefined) into.set(o.targetId, [o]); else list.push(o);
  }
  return { roles, users };
}

/** perm.go ResolveChannel from a resolved user and an indexed channel. */
export function channelBitsFrom(u: ResolvedUser, index: OverwriteIndex): number {
  const base = u.base;
  if (base === PERM_ALL || base === 0) return base;
  let bits = base;
  for (const role of u.held) for (const o of index.roles.get(role) ?? []) bits = apply(bits, o.allow, o.deny);
  for (const o of index.users.get(u.userId) ?? []) bits = apply(bits, o.allow, o.deny);
  bits = (bits & ~COMMUNITY_BITS) | (base & COMMUNITY_BITS);
  return finish(bits);
}

/** perm.go ResolveChannel: the overwrites of the held roles in ascending role position, then the user's own, then the
 *  community-scoped bits restored from the base. An overwrite counts only for its own kind and target. */
export function resolveChannel(input: PermInput, overwrites: readonly PermOverwrite[], userId: string, afk?: boolean): number {
  const bits = channelBitsFrom(resolveUser(input, userId), indexOverwrites(overwrites));
  // R35: every member may view and connect to the built-in AFK channel; the owner and administrators keep PERM_ALL.
  return afk === true && bits !== PERM_ALL ? bits | PERM.viewChannel | PERM.connect : bits;
}

/** The bits `userId` holds in a channel, or null when the user is not in `members` (unknown: the caller keeps today's
 *  behaviour). The owner holds everything. */
export function channelBitsOf(s: { owner: string; everyoneId: string; roles: readonly PermRole[]; members: readonly PermMember[];
  overwrites: readonly PermOverwrite[]; afk?: boolean }, userId: string): number | null {
  if (userId === s.owner) return PERM_ALL;
  const member = s.members.find((m) => m.userId === userId);
  if (!member) return null;
  return resolveChannel({ owner: s.owner, everyoneId: s.everyoneId, roles: s.roles, held: member.roleIds }, s.overwrites, userId, s.afk);
}

/** A channel is private when @everyone's overwrite denies viewing it. */
export function isPrivate(overwrites: readonly PermOverwrite[], everyoneId: string): boolean {
  return overwrites.some((o) => o.kind === 0 && o.targetId === everyoneId && (o.deny & PERM.viewChannel) !== 0);
}
