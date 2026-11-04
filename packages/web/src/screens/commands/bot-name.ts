import { privateResultCopy } from './copy.ts';

/** Resolve this installation's Bot, never another Bot in the community. */
export function installationBotName(members: readonly { userId: string; display?: string; username?: string }[] | undefined, botUserId: string | null): string {
  const member = botUserId === null ? undefined : members?.find(row => row.userId === botUserId);
  return member?.display || member?.username || privateResultCopy.author.bot;
}
