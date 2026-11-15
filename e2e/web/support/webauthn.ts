// A CDP virtual authenticator for the passkey flows (Chromium only). The host must run with -webauthn-origin so the
// relying party matches the page's hostname (HostConfig.webauthnOrigin).
import type { CDPSession, Page } from '@playwright/test';

export interface VirtualAuthenticator { cdp: CDPSession; authenticatorId: string }

export async function addAuthenticator(page: Page): Promise<VirtualAuthenticator> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2', transport: 'internal', hasResidentKey: true, hasUserVerification: true,
      isUserVerified: true, automaticPresenceSimulation: true,
    },
  });
  return { cdp, authenticatorId };
}

/** Copies every credential of `from` onto `to`, so a second browser holds the same passkey. */
export async function copyCredentials(from: VirtualAuthenticator, to: VirtualAuthenticator): Promise<number> {
  const { credentials } = await from.cdp.send('WebAuthn.getCredentials', { authenticatorId: from.authenticatorId });
  for (const credential of credentials) {
    await to.cdp.send('WebAuthn.addCredential', { authenticatorId: to.authenticatorId, credential });
  }
  return credentials.length;
}
