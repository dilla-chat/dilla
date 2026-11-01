// Flow 08: the member list's pure rules. Grouping, tags and the per-member menu are decided here so the pane stays plain.
import { canModerate, topOf, type CommunityAccess, type MemberSummary, type RoleSummary, type PresenceStatus } from '@dilla/client-core';

export type MemberTag = 'owner' | 'you' | 'bot';
export interface MemberRow { userId: string; name: string; tags: MemberTag[]; /** The colour of the member's highest coloured role (any role, hoisted or not); null when none has one. */ color: number | null }
/** `role` is null for the group of everyone who holds no hoisted role. */
export interface MemberGroup { id: string; role: RoleSummary | null; members: MemberRow[] }

export const memberName = (m: MemberSummary): string => (m.display !== '' ? m.display : m.username);

/** The colour of the member's highest role that has one (by position); null when none has. */
function colourOf(member: MemberSummary, roles: readonly RoleSummary[]): number | null {
  let best: RoleSummary | null = null;
  for (const r of roles) {
    if (r.color !== 0 && member.roleIds.includes(r.id) && (best === null || r.position > best.position)) best = r;
  }
  return best?.color ?? null;
}

/**
 * Groups by each member's highest hoisted role (position, highest first); everyone else goes in the role-less group, then the
 * bots in their own group (the prototype's "Bots", task 9), then (with presence) the offline people. Names sort with
 * localeCompare. Each row is tagged owner, you or bot.
 */
export function memberGroups(members: readonly MemberSummary[], roles: readonly RoleSummary[], ownerId: string, selfId: string,
  presence?: Readonly<Record<string, PresenceStatus>>): MemberGroup[] {
  const hoisted = roles.filter(r => r.hoist).sort((a, b) => b.position - a.position);
  const byId = new Map(hoisted.map(r => [r.id, r]));
  const buckets = new Map<string, MemberRow[]>();
  const plain: MemberRow[] = [];
  const bots: MemberRow[] = [];
  const offline: MemberRow[] = [];
  for (const m of members) {
    let best: RoleSummary | null = null;
    for (const id of m.roleIds) {
      const r = byId.get(id);
      if (r !== undefined && (best === null || r.position > best.position)) best = r;
    }
    const tags: MemberTag[] = [];
    if (m.userId === ownerId) tags.push('owner');
    if (m.userId === selfId) tags.push('you');
    if (m.kind === 1) tags.push('bot');
    const row: MemberRow = { userId: m.userId, name: memberName(m), tags, color: colourOf(m, roles) };
    if (m.kind === 1) bots.push(row);
    else if (presence !== undefined && (presence[m.userId] ?? 'offline') === 'offline') offline.push(row);
    else if (best === null) plain.push(row);
    else {
      const list = buckets.get(best.id) ?? [];
      list.push(row);
      buckets.set(best.id, list);
    }
  }
  const byName = (a: MemberRow, b: MemberRow) => a.name.localeCompare(b.name);
  const groups: MemberGroup[] = hoisted.filter(r => buckets.has(r.id))
    .map(r => ({ id: r.id, role: r, members: (buckets.get(r.id) ?? []).sort(byName) }));
  groups.push({ id: 'members', role: null, members: plain.sort(byName) });
  groups.push({ id: 'bots', role: null, members: bots.sort(byName) });
  if (presence !== undefined) groups.push({ id: 'offline', role: null, members: offline.sort(byName) });
  return groups;
}

export type MenuAction = 'message' | 'compare' | 'roles' | 'warn' | 'timeout' | 'kick' | 'ban';
export interface MenuContext { selfId: string; ownerId: string; roles: readonly RoleSummary[] }

/**
 * The actions offered for `member`: message and compare safety numbers (neither on the viewer's own card), roles with
 * manageRoles, and kick and ban when the loaded permissions allow it and the target ranks below the caller (never on
 * the owner or on self). Advisory only: the instance decides again.
 */
export function menuFor(member: MemberSummary, access: CommunityAccess | undefined, ctx: MenuContext): MenuAction[] {
  const out: MenuAction[] = [];
  const self = member.userId === ctx.selfId;
  if (!self) out.push('message', 'compare');
  if (access !== undefined && access.loaded) {
    if (access.manageRoles) out.push('roles');
    const may = canModerate({ me: ctx.selfId, target: member.userId, owner: ctx.ownerId, myTop: access.top, targetTop: topOf(member.roleIds, ctx.roles) });
    if (may && access.moderateMembers) out.push('warn');
    if (may && access.moderateMembers) out.push('timeout');
    if (may && access.kickMembers) out.push('kick');
    if (may && access.banMembers) out.push('ban');
  }
  return out;
}
