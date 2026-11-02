/** LiveKit logs an aborted signal read even when room deletion races its own
 * intentional disconnect. Keep the log in the trace; classify it only after
 * both fixture leave and SDK disconnect have begun in this client. */
export function signalCloseGuard(): { beginLeave(): void; observe(text: string): boolean } {
  let fixtureLeaving = false, sdkDisconnecting = false;
  return {
    beginLeave() { fixtureLeaving = true; sdkDisconnecting = false; },
    observe(text) {
      if (/^connection state changed: \w+ -> (?:connecting|connected|reconnecting)(?:\s|$)/.test(text)) {
        fixtureLeaving = false; sdkDisconnecting = false;
      }
      if (fixtureLeaving && /^disconnect from room(?:\s|$)/.test(text)) sdkDisconnecting = true;
      return fixtureLeaving && sdkDisconnecting
        && /^error reading from signal stream(?:\s|$)/.test(text)
        && /\bWS closed unexpectedly with code 1006\b/.test(text);
    },
  };
}
