/** Desktop may select a base path, but can never redirect a worker's credentials to another origin. */
export function workerInstanceURL(input: unknown, origin: string): string {
  if (input === undefined) return origin;
  if (typeof input !== 'string' || input.length > 2048 || /[\s\\\u0000-\u001f]|%(?:2f|5c|00|2e)/i.test(input)) throw new Error('Invalid worker instance URL');
  const url = new URL(input);
  if (url.origin !== origin || url.username || url.password || url.search || url.hash) throw new Error('Worker instance must have the same origin');
  if (url.pathname === '/assets' || url.pathname.startsWith('/assets/')) throw new Error('The installed client reserves /assets/');
  return url.href.replace(/\/$/, '');
}
