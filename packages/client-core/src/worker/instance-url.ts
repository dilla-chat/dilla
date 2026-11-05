/** The mobile shell's own origins (iOS, Android): its bundled client reaches a chosen instance across origins. */
const MOBILE_APP_ORIGINS = new Set(['capacitor://localhost', 'https://localhost']);

/** Desktop HTTP/gateway configuration must stay inside the complete installed same-origin instance base.
 *  The mobile app's worker (origin capacitor://localhost or https://localhost) may instead name any HTTPS instance;
 *  the server admits those two origins through http.mobile_origins. */
export function workerInstanceURL(input: unknown, origin: string): string {
  if (input === undefined) return origin;
  if (typeof input !== 'string' || input.length > 2048 || /[\s\\\u0000-\u001f]|%(?:2f|5c|00|2e)/i.test(input)) throw new Error('Invalid worker instance URL');
  const url = new URL(input);
  const crossOriginAllowed = MOBILE_APP_ORIGINS.has(origin) && url.protocol === 'https:';
  if ((url.origin !== origin && !crossOriginAllowed) || url.username || url.password || url.search || url.hash) throw new Error('Worker instance must have the same origin');
  if (url.pathname === '/assets' || url.pathname.startsWith('/assets/')) throw new Error('The installed client reserves /assets/');
  return url.href.replace(/\/$/, '');
}
