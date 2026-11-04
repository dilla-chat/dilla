import { SETTINGS, type AccountState } from '@dilla/client-core';

/** Account/device scoping also keeps an old settings slice harmless during an identity change. */
export function dmGifConsentKey(account: AccountState | undefined): string | null {
  return account?.phase === 'ready' && account.instance && account.user && account.deviceId
    ? SETTINGS.dmGifs(account.instance.id, account.user.id, account.deviceId) : null;
}
