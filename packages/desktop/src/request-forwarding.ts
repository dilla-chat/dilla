import type { Session } from 'electron';

/** protocol.handle reconstructs a Request without the renderer's credentials,
 * redirect or cache modes. Restore the client-core data transport contract at
 * the native boundary; this hop must not add session cookies or follow redirects. */
export function fetchInstanceData(ses: Pick<Session, 'fetch'>, request: Request): Promise<Response> {
  return ses.fetch(request, { bypassCustomProtocolHandlers: true, credentials: 'omit', redirect: 'error', cache: 'no-store' });
}
