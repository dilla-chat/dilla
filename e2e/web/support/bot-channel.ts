import type { Page } from '@playwright/test';
import { arr, decode } from '../../../packages/client-core/src/cbor/index';
import { socketRequestOnce } from './socket-requests';

/**
 * Bot v2 Task 17: the Bot posts only into Bot channels, so a spec whose Bot posts into a channel marks it first. The mark
 * is a socket route (PUT …/channels/{channel}/bot-channel), sent as the person whose page this is.
 *
 * `watchBearer` remembers the session bearer of the page's authenticated requests (the app sends it on its HTTP calls, such
 * as the gateway ticket); call it as soon as the page exists. `markBotChannel` then marks the channel named `name`.
 */
export function watchBearer(page: Page): () => string {
  let bearer = '';
  page.context().on('request', (r) => {
    if (!r.url().includes('/v1/')) return;
    void r.allHeaders().then((h) => { const a = h.authorization; if (a?.startsWith('Bearer ') === true) bearer = a; }).catch(() => undefined);
  });
  // Ordinary native request reports on the desktop (Electron) lane omit headers. Observing the factor gate's read (an
  // HTTP route, sent before the socket exists) through a pass-through route makes its headers visible, as afk.spec does.
  void page.context().route('**/v1/auth/factors', (route) => route.continue());
  return () => bearer;
}

const hex = (b: unknown): string => {
  if (!(b instanceof Uint8Array)) throw new Error('not a byte string');
  return [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
};

export async function markBotChannel(page: Page, origin: string, bearerOf: () => string, name: string): Promise<void> {
  const bearer = bearerOf();
  if (!bearer.startsWith('Bearer ')) throw new Error('no session bearer seen on the page yet');
  const community = /\/c\/([0-9a-f]{32})\//.exec(new URL(page.url()).pathname)?.[1];
  if (community === undefined) throw new Error('the page is not in a community');
  const list = await socketRequestOnce(origin, bearer, 'GET', `/v1/communities/${community}/channels`);
  if (list.status !== 200) throw new Error(`GET channels = ${String(list.status)}`);
  const channel = arr(decode(list.body)).map((v) => arr(v)).find((row) => String(row[5]) === name);
  if (channel === undefined) throw new Error(`no channel named ${name}`);
  const mark = await socketRequestOnce(origin, bearer, 'PUT', `/v1/communities/${community}/channels/${hex(channel[0])}/bot-channel`);
  if (mark.status !== 204 && mark.status !== 200) throw new Error(`mark ${name} as a Bot channel = ${String(mark.status)}`);
}
