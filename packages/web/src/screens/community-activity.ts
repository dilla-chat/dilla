import type { AccountState, CoreClient } from '@dilla/client-core';

// Session-local visits belong to one client and signed-in account; a replacement account inherits no ordering.
const visits = new WeakMap<CoreClient, Map<string, readonly string[]>>();
const NONE: readonly string[] = [];

export function communityActivityOwner(account: AccountState | undefined): string | null {
  return account?.instance && account.user ? `${account.instance.id}/${account.user.id}` : null;
}

export function rememberCommunityActivity(client: CoreClient, owner: string | null, communityId: string): void {
  if (owner === null) return;
  let accounts = visits.get(client);
  if (accounts === undefined) { accounts = new Map(); visits.set(client, accounts); }
  const previous = accounts.get(owner) ?? NONE;
  accounts.set(owner, [communityId, ...previous.filter(id => id !== communityId)]);
}

export function recentCommunities(client: CoreClient, owner: string | null): readonly string[] {
  return owner === null ? NONE : visits.get(client)?.get(owner) ?? NONE;
}
