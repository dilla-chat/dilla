import { test } from 'node:test';
import assert from 'node:assert/strict';
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

const ts = (socket, httpOnly, bot = []) => [
  'export const SOCKET_ROUTES: readonly SocketRoute[] = [',
  ...socket.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' },`),
  '];',
  'export const HTTP_ONLY_ROUTES: readonly SocketRoute[] = [',
  ...httpOnly.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' },`),
  '];',
  'export const BOT_RUNTIME_ROUTES: readonly SocketRoute[] = [',
  ...bot.map(([method, pattern]) => `  { method: '${method}', pattern: '${pattern}' },`),
  '];',
].join('\n');

const SOCKET = [['POST', '/v1/groups/{id}/message'], ['GET', '/v1/accounts/me']];
const HTTP_ONLY = [['GET', '/v1/instance'], ['POST', '/v1/communities/{id}/join']];
const BOT = [['PUT', '/v1/communities/{community}/bot/music']];

test('reads Go registrations, literal and through a local pattern variable', () => {
  const routes = goRoutes(go);
  assert.deepEqual([...routes.http].sort(), ['GET /v1/instance', 'POST /v1/communities/{id}/join', 'PUT /v1/communities/{community}/bot/music']);
  assert.deepEqual([...routes.socket].sort(), ['GET /v1/accounts/me', 'POST /v1/groups/{id}/message']);
});

test('passes when the three lists cover the Go routes exactly', () => {
  assert.deepEqual(problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY, BOT))), []);
});

test('reports a Go Socket pattern missing from SOCKET_ROUTES', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET.slice(1), HTTP_ONLY, BOT)));
  assert.equal(found.length, 1);
  assert.match(found[0], /POST \/v1\/groups\/\{id\}\/message.*registered with Socket.*not in SOCKET_ROUTES/);
});

test('reports a SOCKET_ROUTES entry with no Go Socket registration', () => {
  const found = problems(goRoutes(go), manifest(ts([...SOCKET, ['DELETE', '/v1/welcomes/{id}']], HTTP_ONLY, BOT)));
  assert.equal(found.length, 1);
  assert.match(found[0], /DELETE \/v1\/welcomes\/\{id\}.*SOCKET_ROUTES.*no Go Socket registration/);
});

test('reports a Go HTTP /v1 pattern in neither HTTP_ONLY_ROUTES nor BOT_RUNTIME_ROUTES', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY.slice(1), BOT)));
  assert.equal(found.length, 1);
  assert.match(found[0], /GET \/v1\/instance.*HTTP.*not in HTTP_ONLY_ROUTES, SOCKET_ROUTES or BOT_RUNTIME_ROUTES/);
});

test('reports a Bot runtime route that is only on the Bot runtime list when it is missing there', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY, [])));
  assert.equal(found.length, 1);
  assert.match(found[0], /PUT \/v1\/communities\/\{community\}\/bot\/music/);
});

test('reports a stale HTTP-only or Bot runtime entry and an entry listed twice', () => {
  const stale = problems(goRoutes(go), manifest(ts(SOCKET, [...HTTP_ONLY, ['GET', '/v1/gone']], BOT)));
  assert.equal(stale.length, 1);
  assert.match(stale[0], /GET \/v1\/gone.*HTTP_ONLY_ROUTES.*no Go HTTP registration/);
  const twice = problems(goRoutes(go), manifest(ts(SOCKET, [...HTTP_ONLY, ['PUT', '/v1/communities/{community}/bot/music']], BOT)));
  assert.equal(twice.length, 1);
  assert.match(twice[0], /listed more than once/);
});

test('reports a Bot socket route the instance does not serve on the socket', () => {
  const found = problems(goRoutes(go), manifest(ts(SOCKET, HTTP_ONLY, BOT)), ['GET /v1/accounts/me', 'GET /v1/instance']);
  assert.equal(found.length, 1);
  assert.match(found[0], /GET \/v1\/instance.*Bot.*SocketRoutes/);
});

test('the real tree is in sync', () => {
  assert.deepEqual(check(), []);
});
