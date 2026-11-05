import { isMobileAppOrigin, MOBILE_BUILD } from './mobile-build';

/** Desktop may select a base path, but can never redirect a worker's credentials to another origin.
 *  The mobile app's worker (a mobile build, running at capacitor://localhost or https://localhost) names an instance on
 *  another origin by design; dillad admits those two origins through http.mobile_origins. It gets the strict form the
 *  app's picker accepts (packages/mobile/src/instances.ts mobileInstance): an HTTPS origin with a DNS name. */
export function workerInstanceURL(input: unknown, origin: string): string {
  if (input === undefined) return origin;
  if (typeof input !== 'string' || input.length > 2048 || /[\s\\\u0000-\u001f]|%(?:2f|5c|00|2e)/i.test(input)) throw new Error('Invalid worker instance URL');
  const url = new URL(input);
  if (MOBILE_BUILD && isMobileAppOrigin(origin)) {
    // An IP literal is refused: IPv4 dotted or numeric forms parse to a dotted quad, IPv6 to a [bracketed] host.
    const ipLiteral = url.hostname.startsWith('[') || /^\d{1,3}(\.\d{1,3}){3}$/.test(url.hostname);
    // localhost and *.localhost name the device itself; https://localhost is the Android app's own origin.
    const local = url.hostname === 'localhost' || url.hostname.endsWith('.localhost');
    if (url.protocol !== 'https:' || url.pathname !== '/' || url.username || url.password || url.search || url.hash
      || !url.hostname || url.hostname.endsWith('.') || ipLiteral || local) throw new Error('Invalid mobile instance URL');
    return url.origin;
  }
  if (url.origin !== origin || url.username || url.password || url.search || url.hash) throw new Error('Worker instance must have the same origin');
  if (url.pathname === '/assets' || url.pathname.startsWith('/assets/')) throw new Error('The installed client reserves /assets/');
  return url.href.replace(/\/$/, '');
}
