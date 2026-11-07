import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { check, goRoutes, manifest, problems } from './check-socket-routes.mjs';

const go = [{
  path: 'internal/api/a.go',
  source: [
    'func (d *Deps) Mount(m *server.Mux) {',
    '\tm.HandleFunc("GET /v1/instance", d.instance)',
    '\tm.Handle("POST /v1/communities/{id}/join", h)',
    '\tpath := "PUT /v1/communities/{community}/bot/music"',
    '\tm.Handle(path, botSession(path, h))',
    '\tm.SocketAuthed("POST /v1/groups/{id}/message", mw, h)',
    '\tm.Socket("GET /v1/accounts/me", h)',
    '\tm.Handle("GET /healthz", h)',
    '}',
  ].join('\n'),
}];

const ts = (socket, httpOnly, preGateway = []) => [
  'export const SOCKET_ROUTES: readonly SocketRoute[] = [',
  ...socket.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' },`),
  '];',
  'export const HTTP_ONLY_ROUTES: readonly SocketRoute[] = [',
  ...httpOnly.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' },`),
  '];',
  'export const PRE_GATEWAY_ROUTES: readonly SocketRoute[] = [',
  ...preGateway.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' }, // a reason`),
  '];',
].join('\n');

const SOCKET = [['POST', '/v1/groups/{id}/message'], ['GET', '/v1/accounts/me']];
const HTTP_ONLY = [['GET', '/v1/instance'], ['POST', '/v1/communities/{id}/join'], ['PUT', '/v1/communities/{community}/bot/music']];

test('reads Go registrations, literal and through a local pattern variable', () => {
  const routes = goRoutes(go);
  assert.deepEqual([...routes.http].sort(), ['GET /v1/instance', 'POST /v1/communities/{id}/join', 'PUT /v1/communities/{community}/bot/music']);
  assert.deepEqual([...routes.socket].sort(), ['GET /v1/accounts/me', 'POST /v1/groups/{id}/message']);
});

test('passes when the two lists cover the Go routes exactly', () => {
  assert.deepEqual(problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY))), []);
});

test('reports a Go Socket pattern missing from SOCKET_ROUTES', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET.slice(1), HTTP_ONLY)));
  assert.equal(found.length, 1);
  assert.match(found[0], /POST \/v1\/groups\/\{id\}\/message.*registered with Socket.*not in SOCKET_ROUTES/);
});

test('reports a SOCKET_ROUTES entry with no Go Socket registration', () => {
  const found = problems(goRoutes(go), manifest(ts([...SOCKET, ['DELETE', '/v1/welcomes/{id}']], HTTP_ONLY)));
  assert.equal(found.length, 1);
  assert.match(found[0], /DELETE \/v1\/welcomes\/\{id\}.*SOCKET_ROUTES.*no Go Socket registration/);
});

test('reports a Go HTTP /v1 pattern in neither HTTP_ONLY_ROUTES nor SOCKET_ROUTES', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY.slice(1))));
  assert.equal(found.length, 1);
  assert.match(found[0], /GET \/v1\/instance.*HTTP.*in neither HTTP_ONLY_ROUTES nor SOCKET_ROUTES/);
});

test('reports a stale HTTP-only entry and an entry listed twice', () => {
  const stale = problems(goRoutes(go), manifest(ts(SOCKET, [...HTTP_ONLY, ['GET', '/v1/gone']])));
  assert.equal(stale.length, 1);
  assert.match(stale[0], /GET \/v1\/gone.*HTTP_ONLY_ROUTES.*no Go HTTP registration/);
  const twice = problems(goRoutes(go), manifest(ts(SOCKET, [...HTTP_ONLY, ['PUT', '/v1/communities/{community}/bot/music']])));
  assert.equal(twice.length, 1);
  assert.match(twice[0], /listed more than once/);
});

test('reports a Bot socket route the instance does not serve on the socket', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY)), ['GET /v1/accounts/me', 'GET /v1/instance']);
  assert.equal(found.length, 1);
  assert.match(found[0], /GET \/v1\/instance.*Bot.*SocketRoutes/);
});

test('reports a pre-gateway route moved to SOCKET_ROUTES, and a stale pre-gateway entry', () => {
  const ok = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY, [['GET', '/v1/instance']])));
  assert.deepEqual(ok, []);
  const moved = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY, [['GET', '/v1/accounts/me']])));
  assert.equal(moved.length, 2);
  assert.match(moved[0], /GET \/v1\/accounts\/me.*PRE_GATEWAY_ROUTES.*must stay on HTTP.*SOCKET_ROUTES/);
  assert.match(moved[1], /GET \/v1\/accounts\/me.*PRE_GATEWAY_ROUTES.*no Go HTTP registration/);
});

// Fix round 2: the routes a session uses with no gateway (session establishment, the owed account's factor set-up, the
// calls after quiesce) are held to HTTP in the real manifest; moving any one to SOCKET_ROUTES fails the check.
const NO_GATEWAY = [
  'POST /v1/devices/{device_id}/sessions', 'DELETE /v1/devices/{device_id}/sessions',
  'POST /v1/auth/totp/enroll', 'POST /v1/auth/totp/confirm', 'POST /v1/auth/passkey/register/begin',
  'POST /v1/auth/passkey/register/finish', 'POST /v1/devices/{device_id}/sessions/challenge',
];

test('the real manifest holds every no-gateway route to HTTP, and moving one to SOCKET_ROUTES fails', () => {
  const source = readFileSync(new URL('../packages/client-core/src/http/socket-routes.ts', import.meta.url), 'utf8');
  const real = manifest(source);
  for (const key of NO_GATEWAY) assert.ok(real.PRE_GATEWAY_ROUTES.includes(key), `${key} is not in PRE_GATEWAY_ROUTES`);
  const go = goRoutes([{ path: 'x.go', source: NO_GATEWAY.map((k) => `m.Socket("${k}", h)`).join('\n') }]);
  for (const key of NO_GATEWAY) {
    const moved = { ...real, SOCKET_ROUTES: [key] };
    const found = problems({ http: new Set(real.PRE_GATEWAY_ROUTES), socket: new Set([key]) }, moved)
      .filter((p) => p.includes('must stay on HTTP'));
    assert.equal(found.length, 1, key);
    assert.ok(go.socket.has(key));
  }
});

test('classifies the public /hooks route like a /v1 one (Bot v2 task 13)', () => {
  const hooks = [{ path: 'internal/api/webhooks.go', source: 'm.Handle("POST /hooks/{webhook_id}/{secret}", h)' }];
  const routes = goRoutes([...go, ...hooks]);
  assert.ok(routes.http.has('POST /hooks/{webhook_id}/{secret}'));
  const unlisted = problems(routes, manifest(ts(SOCKET, HTTP_ONLY)));
  assert.equal(unlisted.length, 1);
  assert.match(unlisted[0], /POST \/hooks\/\{webhook_id\}\/\{secret\}.*in neither HTTP_ONLY_ROUTES nor SOCKET_ROUTES/);
  assert.deepEqual(problems(routes, manifest(ts(SOCKET, [...HTTP_ONLY, ['POST', '/hooks/{webhook_id}/{secret}']]))), []);
});

test('the real tree is in sync', () => {
  assert.deepEqual(check(), []);
});
