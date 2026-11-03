/** LiveKit logs an aborted signal read even when room deletion races its own
 * intentional disconnect. Keep the log in the trace; classify it only after
 * both an explicitly expected leave and SDK disconnect have begun in this client. */
export function signalCloseGuard(): {
  beginLeave(): void; expectSecurityLeave(assertLeave: () => Promise<void>): Promise<void>; observe(text: string): boolean;
} {
  let expectedLeaving = false, sdkDisconnecting = false;
  const beginLeave = () => { expectedLeaving = true; sdkDisconnecting = false; };
  return {
    beginLeave,
    async expectSecurityLeave(assertLeave) {
      beginLeave();
      try { await assertLeave(); }
      catch (error) { expectedLeaving = false; sdkDisconnecting = false; throw error; }
    },
    observe(text) {
      if (/^connection state changed: \w+ -> (?:connecting|connected|reconnecting)(?:\s|$)/.test(text)) {
        expectedLeaving = false; sdkDisconnecting = false;
      }
      if (expectedLeaving && /^disconnect from room(?:\s|$)/.test(text)) sdkDisconnecting = true;
      return expectedLeaving && sdkDisconnecting
        && /^error reading from signal stream(?:\s|$)/.test(text)
        && /\bWS closed unexpectedly with code 1006\b/.test(text);
    },
  };
}

/** SDK disconnect and reconnection lines are informational, but must reach the
 * same per-client guard before any error-only consumer filters the stream. */
export function observeSignalConsole(
  context: Pick<import('@playwright/test').BrowserContext, 'on'>,
  guard: ReturnType<typeof signalCloseGuard>,
  unexpectedError: (text: string) => void,
): void {
  context.on('console', message => {
    const expected = guard.observe(message.text());
    if (message.type() === 'error' && !expected) unexpectedError(message.text());
  });
}
