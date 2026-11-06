// Bot v2: which authenticated app routes go over the gateway socket (op 59 request, op 60 response) and which stay on HTTP.
// scripts/check-socket-routes.mjs holds this file to the Go registrations: every `m.Socket*` pattern is in SOCKET_ROUTES and
// every SOCKET_ROUTES entry is one; every Go HTTP /v1 pattern is in HTTP_ONLY_ROUTES, SOCKET_ROUTES or BOT_RUNTIME_ROUTES.
// Tasks 4-6 moved every authenticated app route from HTTP_ONLY_ROUTES to SOCKET_ROUTES and deleted its REST form; what is
// left in HTTP_ONLY_ROUTES is the spec's stays-on-HTTP list (before a session exists, large binary bodies, unauthenticated
// or public) and the rulings' additions (negotiate, the F allow-list, backup chunks and GIF media), plus the Bot runtime's own
// routes in BOT_RUNTIME_ROUTES until Task 14. internal/dillad TestHTTPOnlyListIsExactlyTheStaysOnHTTPList pins the list.
import type { HttpRequest } from './client';

/** One route: its method and its Go ServeMux path pattern ({name} one segment, {name...} the rest). */
export interface SocketRoute { method: HttpRequest['method']; pattern: string }

/** Routes the app sends as socket request frames. There is no HTTP fallback: the instance serves them only there. */
export const SOCKET_ROUTES: readonly SocketRoute[] = [
  { method: 'DELETE', pattern: '/v1/accounts/me' },
  { method: 'PATCH', pattern: '/v1/accounts/me' },
  { method: 'PATCH', pattern: '/v1/accounts/me/presence' },
  { method: 'POST', pattern: '/v1/accounts/me/presence/activity' },
  { method: 'GET', pattern: '/v1/admin/audit' },
  { method: 'DELETE', pattern: '/v1/admin/blobs/{blob_id}' },
  { method: 'GET', pattern: '/v1/admin/diagnostics' },
  { method: 'POST', pattern: '/v1/admin/users/{id}/disable' },
  { method: 'POST', pattern: '/v1/auth/factors/remove' },
  { method: 'POST', pattern: '/v1/auth/password' },
  { method: 'POST', pattern: '/v1/auth/recovery/codes' },
  { method: 'POST', pattern: '/v1/auth/stepup' },
  { method: 'POST', pattern: '/v1/auth/stepup/passkey/begin' },
  { method: 'POST', pattern: '/v1/auth/stepup/passkey/finish' },
  { method: 'DELETE', pattern: '/v1/calls/{call_id}' },
  { method: 'POST', pattern: '/v1/calls/{call_id}/decline' },
  { method: 'POST', pattern: '/v1/calls/{call_id}/rtt' },
  { method: 'DELETE', pattern: '/v1/calls/{call_id}/share' },
  { method: 'POST', pattern: '/v1/calls/{call_id}/share' },
  { method: 'POST', pattern: '/v1/calls/{call_id}/stats' },
  { method: 'DELETE', pattern: '/v1/channels/{id}' },
  { method: 'GET', pattern: '/v1/channels/{id}' },
  { method: 'PATCH', pattern: '/v1/channels/{id}' },
  { method: 'GET', pattern: '/v1/channels/{id}/call' },
  { method: 'POST', pattern: '/v1/channels/{id}/calls' },
  { method: 'GET', pattern: '/v1/channels/{id}/members' },
  { method: 'DELETE', pattern: '/v1/channels/{id}/members/{user_id}' },
  { method: 'PUT', pattern: '/v1/channels/{id}/members/{user_id}' },
  { method: 'GET', pattern: '/v1/channels/{id}/messages' },
  { method: 'POST', pattern: '/v1/channels/{id}/messages' },
  { method: 'DELETE', pattern: '/v1/channels/{id}/messages/{seq}' },
  { method: 'PATCH', pattern: '/v1/channels/{id}/messages/{seq}' },
  { method: 'DELETE', pattern: '/v1/channels/{id}/overwrites/{kind}/{target_id}' },
  { method: 'PUT', pattern: '/v1/channels/{id}/overwrites/{kind}/{target_id}' },
  { method: 'GET', pattern: '/v1/channels/{id}/permissions' },
  { method: 'PUT', pattern: '/v1/channels/{id}/read-state' },
  { method: 'GET', pattern: '/v1/channels/{id}/search' },
  { method: 'GET', pattern: '/v1/communities' },
  { method: 'POST', pattern: '/v1/communities' },
  { method: 'GET', pattern: '/v1/communities/{community_id}/presence' },
  { method: 'DELETE', pattern: '/v1/communities/{community}/bot' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot' },
  { method: 'PUT', pattern: '/v1/communities/{community}/bot' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/actors/{actor}/authority' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/actors/{actor}/channels/{channel}/authority' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/command-status' },
  { method: 'POST', pattern: '/v1/communities/{community}/bot/disable' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/events' },
  { method: 'PUT', pattern: '/v1/communities/{community}/bot/events/ack' },
  { method: 'POST', pattern: '/v1/communities/{community}/bot/join-role-grants' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/members/{member}/episode' },
  { method: 'PUT', pattern: '/v1/communities/{community}/bot/music-settings' },
  { method: 'PATCH', pattern: '/v1/communities/{community}/bot/profile' },
  { method: 'GET', pattern: '/v1/communities/{community}/bot/receipts/{operation}' },
  { method: 'POST', pattern: '/v1/communities/{community}/bot/receipts/{operation}/resolve' },
  { method: 'POST', pattern: '/v1/communities/{community}/bot/restart' },
  { method: 'DELETE', pattern: '/v1/communities/{id}' },
  { method: 'GET', pattern: '/v1/communities/{id}' },
  { method: 'PATCH', pattern: '/v1/communities/{id}' },
  { method: 'GET', pattern: '/v1/communities/{id}/afk' },
  { method: 'POST', pattern: '/v1/communities/{id}/afk/activity' },
  { method: 'PUT', pattern: '/v1/communities/{id}/afk/away' },
  { method: 'GET', pattern: '/v1/communities/{id}/afk/settings' },
  { method: 'PUT', pattern: '/v1/communities/{id}/afk/settings' },
  { method: 'GET', pattern: '/v1/communities/{id}/bans' },
  { method: 'DELETE', pattern: '/v1/communities/{id}/bans/{user_id}' },
  { method: 'PUT', pattern: '/v1/communities/{id}/bans/{user_id}' },
  { method: 'GET', pattern: '/v1/communities/{id}/bot-join-role' },
  { method: 'PUT', pattern: '/v1/communities/{id}/bot-join-role' },
  { method: 'GET', pattern: '/v1/communities/{id}/calls' },
  { method: 'GET', pattern: '/v1/communities/{id}/channels' },
  { method: 'POST', pattern: '/v1/communities/{id}/channels' },
  { method: 'GET', pattern: '/v1/communities/{id}/gifs' },
  { method: 'DELETE', pattern: '/v1/communities/{id}/gifs/key' },
  { method: 'PUT', pattern: '/v1/communities/{id}/gifs/key' },
  { method: 'PUT', pattern: '/v1/communities/{id}/gifs/rating' },
  { method: 'GET', pattern: '/v1/communities/{id}/gifs/search' },
  { method: 'GET', pattern: '/v1/communities/{id}/invites' },
  { method: 'POST', pattern: '/v1/communities/{id}/invites' },
  { method: 'POST', pattern: '/v1/communities/{id}/join' },
  { method: 'POST', pattern: '/v1/communities/{id}/leave' },
  { method: 'GET', pattern: '/v1/communities/{id}/members' },
  { method: 'DELETE', pattern: '/v1/communities/{id}/members/{user_id}' },
  { method: 'GET', pattern: '/v1/communities/{id}/members/{user_id}' },
  { method: 'DELETE', pattern: '/v1/communities/{id}/members/{user_id}/roles/{role_id}' },
  { method: 'PUT', pattern: '/v1/communities/{id}/members/{user_id}/roles/{role_id}' },
  { method: 'DELETE', pattern: '/v1/communities/{id}/members/{user_id}/timeout' },
  { method: 'GET', pattern: '/v1/communities/{id}/members/{user_id}/timeout' },
  { method: 'PUT', pattern: '/v1/communities/{id}/members/{user_id}/timeout' },
  { method: 'POST', pattern: '/v1/communities/{id}/members/{user_id}/warning' },
  { method: 'GET', pattern: '/v1/communities/{id}/overwrites' },
  { method: 'POST', pattern: '/v1/communities/{id}/owner' },
  { method: 'GET', pattern: '/v1/communities/{id}/permissions' },
  { method: 'PUT', pattern: '/v1/communities/{id}/role-panels' },
  { method: 'POST', pattern: '/v1/communities/{id}/role-panels/{channel_id}/{message_seq}/reaction' },
  { method: 'GET', pattern: '/v1/communities/{id}/roles' },
  { method: 'POST', pattern: '/v1/communities/{id}/roles' },
  { method: 'GET', pattern: '/v1/communities/{id}/self-roles' },
  { method: 'PUT', pattern: '/v1/communities/{id}/self-roles/policy' },
  { method: 'PUT', pattern: '/v1/communities/{id}/self-roles/{role_id}' },
  { method: 'GET', pattern: '/v1/devices' },
  { method: 'POST', pattern: '/v1/devices' },
  { method: 'DELETE', pattern: '/v1/devices/{device_id}' },
  { method: 'PATCH', pattern: '/v1/devices/{device_id}' },
  { method: 'GET', pattern: '/v1/devices/{device_id}/keypackage' },
  { method: 'GET', pattern: '/v1/dms' },
  { method: 'POST', pattern: '/v1/dms' },
  { method: 'POST', pattern: '/v1/groups' },
  { method: 'POST', pattern: '/v1/groups/{group}/automod-grants/arm' },
  { method: 'POST', pattern: '/v1/groups/{group}/bot/messages' },
  { method: 'GET', pattern: '/v1/groups/{group}/moderation/removals' },
  { method: 'POST', pattern: '/v1/groups/{group}/moderation/removals' },
  { method: 'POST', pattern: '/v1/groups/{id}/commit' },
  { method: 'POST', pattern: '/v1/groups/{id}/cursor' },
  { method: 'POST', pattern: '/v1/groups/{id}/fork-report' },
  { method: 'GET', pattern: '/v1/groups/{id}/handshakes' },
  { method: 'GET', pattern: '/v1/groups/{id}/heal' },
  { method: 'POST', pattern: '/v1/groups/{id}/heal' },
  { method: 'GET', pattern: '/v1/groups/{id}/heal-state' },
  { method: 'GET', pattern: '/v1/groups/{id}/info' },
  { method: 'GET', pattern: '/v1/groups/{id}/interaction-admission-context' },
  { method: 'POST', pattern: '/v1/groups/{id}/message' },
  { method: 'GET', pattern: '/v1/groups/{id}/messages' },
  { method: 'DELETE', pattern: '/v1/groups/{id}/messages/{seq}' },
  { method: 'POST', pattern: '/v1/groups/{id}/proposal' },
  { method: 'GET', pattern: '/v1/groups/{id}/proposals' },
  { method: 'POST', pattern: '/v1/groups/{id}/resync' },
  { method: 'GET', pattern: '/v1/groups/{id}/tree' },
  { method: 'DELETE', pattern: '/v1/invites/{id}' },
  { method: 'GET', pattern: '/v1/reports' },
  { method: 'POST', pattern: '/v1/reports' },
  { method: 'PATCH', pattern: '/v1/reports/{id}' },
  { method: 'DELETE', pattern: '/v1/roles/{id}' },
  { method: 'PATCH', pattern: '/v1/roles/{id}' },
  { method: 'GET', pattern: '/v1/welcomes' },
  { method: 'DELETE', pattern: '/v1/welcomes/{welcome_id}' },
];

/** Routes that stay on HTTP (before a session exists, large binary bodies, unauthenticated or public). */
export const HTTP_ONLY_ROUTES: readonly SocketRoute[] = [
  { method: 'POST', pattern: '/v1/accounts' },
  { method: 'GET', pattern: '/v1/accounts/me' },
  { method: 'GET', pattern: '/v1/auth/factors' },
  { method: 'GET', pattern: '/v1/auth/oidc/callback' },
  { method: 'GET', pattern: '/v1/auth/oidc/start' },
  { method: 'POST', pattern: '/v1/auth/passkey/login/begin' },
  { method: 'POST', pattern: '/v1/auth/passkey/login/finish' },
  { method: 'POST', pattern: '/v1/auth/passkey/register/begin' },
  { method: 'POST', pattern: '/v1/auth/passkey/register/finish' },
  { method: 'POST', pattern: '/v1/auth/password/login' },
  { method: 'POST', pattern: '/v1/auth/recovery/verify' },
  { method: 'POST', pattern: '/v1/auth/totp/confirm' },
  { method: 'POST', pattern: '/v1/auth/totp/enroll' },
  { method: 'POST', pattern: '/v1/auth/totp/verify' },
  { method: 'GET', pattern: '/v1/backups' },
  { method: 'GET', pattern: '/v1/backups/{kind}/{chunk_seq}' },
  { method: 'PUT', pattern: '/v1/backups/{kind}/{chunk_seq}' },
  { method: 'DELETE', pattern: '/v1/backups/{kind}/{chunk_seq}' },
  { method: 'GET', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'PUT', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'DELETE', pattern: '/v1/channels/{id}/blobs/{blob_id}' },
  { method: 'POST', pattern: '/v1/channels/{id}/blobs/{blob_id}/confirm' },
  { method: 'GET', pattern: '/v1/communities/{id}/gifs/media/{gif_id}' },
  { method: 'POST', pattern: '/v1/devices/{device_id}/sessions' },
  { method: 'DELETE', pattern: '/v1/devices/{device_id}/sessions' },
  { method: 'POST', pattern: '/v1/devices/{device_id}/sessions/challenge' },
  { method: 'POST', pattern: '/v1/gateway/ticket' },
  { method: 'GET', pattern: '/v1/instance' },
  { method: 'GET', pattern: '/v1/instance/limits' },
  { method: 'GET', pattern: '/v1/keypackages' },
  { method: 'POST', pattern: '/v1/keypackages' },
  { method: 'POST', pattern: '/v1/sessions/negotiate' },
  { method: 'GET', pattern: '/v1/users/{user_id}/device-list' },
  { method: 'PUT', pattern: '/v1/users/{user_id}/device-list' },
];

/** Ruling C1: routes the worker calls with a session while no gateway carries requests, and so stay on HTTP: sign-up,
 *  sign-in and enrolment; enterReady's steps (a)-(f) before (g) creates the gateway; the factor-setup phase of an owed
 *  account (enterFactorSetup returns before (g), and onFactorRequired drops the gateway); the calls made after quiesce()
 *  stops it (sign-out's self-revoking list, forgetting this browser); and the ticket that opens it. Each is also in
 *  HTTP_ONLY_ROUTES, and check-socket-routes fails if one moves to SOCKET_ROUTES. */
export const PRE_GATEWAY_ROUTES: readonly SocketRoute[] = [
  { method: 'GET', pattern: '/v1/instance' }, // (b) the instance signer pin check, and every mint of a ticket
  { method: 'PUT', pattern: '/v1/users/{user_id}/device-list' }, // (c) publishDeviceList; sign-out's self-revoking list after quiesce
  { method: 'GET', pattern: '/v1/users/{user_id}/device-list' }, // (c) a 409's served head; (f) refreshOwnDeviceList and its history pages
  { method: 'GET', pattern: '/v1/auth/factors' }, // the factor gate between (d) and (e)
  { method: 'GET', pattern: '/v1/instance/limits' }, // a sign-up's first KeyPackage top-up
  { method: 'GET', pattern: '/v1/keypackages' }, // the top-up reads this device's count
  { method: 'POST', pattern: '/v1/keypackages' }, // the top-up publishes, before any join
  { method: 'GET', pattern: '/v1/backups' }, // (e) ensureBackups lists the objects
  { method: 'GET', pattern: '/v1/backups/{kind}/{chunk_seq}' }, // (e) ensureBackups and repairBackupState read an object
  { method: 'PUT', pattern: '/v1/backups/{kind}/{chunk_seq}' }, // (e) ensureBackups and repairBackupState store one
  { method: 'GET', pattern: '/v1/accounts/me' }, // sign-up reads its own account before ready
  { method: 'POST', pattern: '/v1/gateway/ticket' }, // the ticket that opens the socket
  { method: 'POST', pattern: '/v1/devices/{device_id}/sessions' }, // (b) session.ensure() renewing, (d) session.establish(), enrolment's pending session
  { method: 'DELETE', pattern: '/v1/devices/{device_id}/sessions' }, // forgetBrowser ends the sessions after quiesce (also from factor-setup and revoked)
  { method: 'POST', pattern: '/v1/auth/totp/enroll' }, // factor-setup: an owed account enrols an app
  { method: 'POST', pattern: '/v1/auth/totp/confirm' }, // factor-setup: confirms the app's first code
  { method: 'POST', pattern: '/v1/auth/passkey/register/begin' }, // factor-setup: starts a passkey registration
  { method: 'POST', pattern: '/v1/auth/passkey/register/finish' }, // factor-setup: finishes it
  { method: 'POST', pattern: '/v1/devices/{device_id}/sessions/challenge' }, // (d) the unauthenticated challenge before session.establish()
];

/** The Bot runtime's own HTTP routes (F24), kept apart from the app's lists. */
export const BOT_RUNTIME_ROUTES: readonly SocketRoute[] = [
  { method: 'GET', pattern: '/v1/bot-runtime/installations' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/automod-grants/redeem' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/claim' },
  { method: 'PUT', pattern: '/v1/bot-runtime/installations/{installation}/credential' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/enroll' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/groups' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/groups/{operation}' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/interaction-admissions' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/interaction-admissions/context' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/interaction-admissions/{request}/group-absence' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/cursor' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/handshakes' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/heal' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/info' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/interaction-groups/{group}/messages' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/keypackages' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/keypackages' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/keypackages/{operation}' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/moderation/actions' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/moderation/removals/{request}' },
  { method: 'GET', pattern: '/v1/bot-runtime/installations/{installation}/moderation/warnings/{request}' },
  { method: 'POST', pattern: '/v1/bot-runtime/installations/{installation}/ready' },
  { method: 'PUT', pattern: '/v1/bot-runtime/installations/{installation}/status' },
];

interface Compiled { key: string; method: string; segments: readonly string[] }

const COMPILED = new WeakMap<readonly SocketRoute[], readonly Compiled[]>();

function compiled(routes: readonly SocketRoute[]): readonly Compiled[] {
  let table = COMPILED.get(routes);
  if (table === undefined) {
    table = routes.map((r) => ({ key: `${r.method} ${r.pattern}`, method: r.method, segments: r.pattern.split('/').slice(1) }));
    COMPILED.set(routes, table);
  }
  return table;
}

/** Go ServeMux matching of one path pattern against a path's segments. */
function matches(segments: readonly string[], parts: readonly string[]): boolean {
  for (let i = 0; i < segments.length; i++) {
    const segment = segments[i];
    if (segment.startsWith('{') && segment.endsWith('...}')) return i < parts.length;
    if (i >= parts.length) return false;
    if (segment === '{$}') return i === parts.length - 1 && parts[i] === '';
    if (segment.startsWith('{') && segment.endsWith('}')) {
      if (parts[i] === '') return false;
      continue;
    }
    if (segment !== parts[i]) return false;
  }
  return segments.length === parts.length;
}

/** The `METHOD /pattern` key of the socket route a request matches, or null when the request goes over HTTP. The query
 *  string is not part of the match. */
export function socketRouteFor(method: string, path: string, routes: readonly SocketRoute[] = SOCKET_ROUTES): string | null {
  const table = compiled(routes);
  if (table.length === 0) return null;
  const bare = path.split('?', 1)[0];
  if (!bare.startsWith('/')) return null;
  const parts = bare.split('/').slice(1);
  for (const route of table) if (route.method === method && matches(route.segments, parts)) return route.key;
  return null;
}
