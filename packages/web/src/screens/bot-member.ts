// Who is this community's Bot: one rule for the whole web client. The answer comes from the server's member row
// (account kind 1), never from a sender's credential, which its owner signs (Task S1).
import type { MemberSummary } from '@dilla/client-core';

/** The server's account kind of a managed Bot. */
export const BOT_ACCOUNT_KIND = 1;

/** The member row of `userId` when the server lists it as a Bot account; undefined otherwise. */
export function communityBotMember(members: readonly MemberSummary[] | undefined, userId: string | null | undefined): MemberSummary | undefined {
  if (members === undefined || userId === null || userId === undefined) return undefined;
  return members.find(m => m.userId === userId && m.kind === BOT_ACCOUNT_KIND);
}

/** Whether `userId` is a Bot account by the server's member list. */
export function isCommunityBot(members: readonly MemberSummary[], userId: string | null): boolean {
  return communityBotMember(members, userId) !== undefined;
}
