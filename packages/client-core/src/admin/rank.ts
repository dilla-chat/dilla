// Who may do what to whom in a server. The client's gate is advisory: the instance decides every request again.
import { OWNER_TOP } from '../http/routes';
import { PERM } from './perm';
import type { CommunityAccess, RoleSummary } from '../state/types';

/** What a member may do community-wide, from the resolved bits and rank of GET /v1/communities/{id}/permissions. The owner
 *  and an administrator hold every permission. */
export function accessOf(bits: bigint, top: number, owner: boolean): CommunityAccess {
  const all = owner || (bits & BigInt(PERM.administrator)) !== 0n;
  const has = (bit: number): boolean => all || (bits & BigInt(bit)) !== 0n;
  return { top, owner, manageCommunity: has(PERM.manageCommunity), viewAuditLog: has(PERM.viewAuditLog), manageChannels: has(PERM.manageChannels),
    kickMembers: has(PERM.kickMembers), banMembers: has(PERM.banMembers), moderateMembers: has(PERM.moderateMembers), manageMessages: has(PERM.manageMessages), createInvite: has(PERM.createInvite),
    manageRoles: has(PERM.manageRoles), loaded: true, everyoneId: '', overwritesLoaded: false };
}

/** The highest role position among `roleIds`; 0 with none. A role id the client has no row for counts as just under the
 *  owner, so a stale role list never lets anyone act on a member the client cannot rank. */
export function topOf(roleIds: readonly string[], roles: readonly RoleSummary[]): number {
  const position = new Map(roles.map((r) => [r.id, r.position]));
  let top = 0;
  for (const roleId of roleIds) top = Math.max(top, position.get(roleId) ?? OWNER_TOP - 1);
  return top;
}

/** Whether `me` may kick or ban `target`: never the owner or oneself; always for the owner; otherwise only from a strictly
 *  higher rank than the target's, and never with no rank at all. */
export function canModerate(args: { me: string; target: string; owner: string; myTop: number; targetTop: number }): boolean {
  const { me, target, owner, myTop, targetTop } = args;
  if (target === owner || target === me) return false;
  if (me === owner) return true;
  if (myTop === 0) return false;
  return targetTop < myTop;
}
