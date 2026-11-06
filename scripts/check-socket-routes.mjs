import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Drift guard for the socket route manifest (Bot v2). packages/client-core/src/http/socket-routes.ts says which
 * authenticated routes the app sends over the gateway socket; the instance serves a moved route there only, so the two
 * must agree exactly. This fails when:
 *   - a Go `Socket*` registration is missing from SOCKET_ROUTES (the app would still use HTTP, which no longer serves it);
 *   - a SOCKET_ROUTES entry has no Go `Socket*` registration (the app would send a request nothing answers);
 *   - a Go HTTP /v1 registration is in none of HTTP_ONLY_ROUTES, SOCKET_ROUTES and BOT_RUNTIME_ROUTES (unclassified);
 *   - an HTTP_ONLY_ROUTES or BOT_RUNTIME_ROUTES entry has no Go HTTP registration (stale), or an entry is listed twice;
 *   - the Bot's SocketRoutes (internal/bot/client/socket_routes.go) names a route the instance does not serve on the socket.
 * Go registrations are read from source: string literals and local variables or constants assigned one, joined by +.
 * Every production /v1 registration is written that way; internal/dillad TestSocketRouteManifestMatchesTheMux checks the
 * same rules against the composed mux.
 */
const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const MANIFEST = 'packages/client-core/src/http/socket-routes.ts';
const BOT_ROUTES = 'internal/bot/client/socket_routes.go';
const GO_DIRS = ['internal', 'cmd'];
const LISTS = ['SOCKET_ROUTES', 'HTTP_ONLY_ROUTES', 'BOT_RUNTIME_ROUTES'];
const PATTERN = /^(GET|HEAD|POST|PUT|PATCH|DELETE) \/\S*$/;
const TERM = String.raw`(?:"[^"]*"|[A-Za-z_][A-Za-z0-9_]*)`;
const CALL = new RegExp(String.raw`\.(HandleFunc|Handle|SocketAuthedWithCap|SocketAuthed|SocketWithCap|Socket)\(\s*(${TERM}(?:\s*\+\s*${TERM})*)\s*,`, 'g');

/** The value of a pattern variable at a call: the last assignment of a string literal to it before the call. */
function resolve(source, name, at) {
  const assign = new RegExp(`(?:\\b${name}\\s*(?::=|=)|\\bconst\\s+${name}\\s*=)\\s*"([^"]*)"`, 'g');
  let value = null;
  for (const m of source.matchAll(assign)) if (m.index < at) value = m[1];
  return value;
}

/** A pattern argument: string literals and pattern variables joined by +, or null when a part cannot be read. */
function patternOf(source, expression, at) {
  let pattern = '';
  for (const term of expression.split('+').map((t) => t.trim())) {
    const value = term.startsWith('"') ? term.slice(1, -1) : resolve(source, term, at);
    if (value === null) return null;
    pattern += value;
  }
  return pattern;
}

/** Go registrations as `METHOD /path` keys: `http` holds the /v1 HTTP patterns, `socket` every socket pattern. */
export function goRoutes(files) {
  const http = new Set();
  const socket = new Set();
  for (const { source } of files) {
    for (const m of source.matchAll(CALL)) {
      const pattern = patternOf(source, m[2], m.index);
      if (pattern === null || !PATTERN.test(pattern)) continue;
      if (m[1].startsWith('Socket')) socket.add(pattern);
      else if (pattern.split(' ')[1].startsWith('/v1/')) http.add(pattern);
    }
  }
  return { http, socket };
}

/** The three lists of socket-routes.ts, each as `METHOD /pattern` keys in file order. */
export function manifest(source) {
  const lists = {};
  for (const name of LISTS) {
    const m = new RegExp(`export const ${name}: readonly SocketRoute\\[\\] = \\[([\\s\\S]*?)\\n\\];`).exec(source);
    if (!m) throw new Error(`${name} array literal not found`);
    lists[name] = [...m[1].matchAll(/\{\s*method:\s*'([A-Z]+)',\s*pattern:\s*'([^']+)'\s*\}/g)].map((e) => `${e[1]} ${e[2]}`);
  }
  return lists;
}

/** The Bot's SocketRoutes entries. */
export function botSocketRoutes(source) {
  const m = /var SocketRoutes = \[\]string\{([^}]*)\}/.exec(source);
  if (!m) throw new Error('SocketRoutes slice literal not found');
  return [...m[1].matchAll(/"([^"]*)"/g)].map((e) => e[1]);
}

export function problems(go, lists, bot = []) {
  const found = [];
  const socketList = new Set(lists.SOCKET_ROUTES);
  for (const key of go.socket) {
    if (!socketList.has(key)) found.push(`${key} is registered with Socket in Go but is not in SOCKET_ROUTES`);
  }
  for (const key of lists.SOCKET_ROUTES) {
    if (!go.socket.has(key)) found.push(`${key} is in SOCKET_ROUTES but has no Go Socket registration`);
  }
  const classified = new Set([...lists.SOCKET_ROUTES, ...lists.HTTP_ONLY_ROUTES, ...lists.BOT_RUNTIME_ROUTES]);
  for (const key of go.http) {
    if (!classified.has(key)) found.push(`${key} is registered on HTTP in Go but is not in HTTP_ONLY_ROUTES, SOCKET_ROUTES or BOT_RUNTIME_ROUTES`);
  }
  for (const name of ['HTTP_ONLY_ROUTES', 'BOT_RUNTIME_ROUTES']) {
    for (const key of lists[name]) if (!go.http.has(key)) found.push(`${key} is in ${name} but has no Go HTTP registration`);
  }
  const seen = new Map();
  for (const name of LISTS) {
    for (const key of lists[name]) {
      if (seen.has(key)) found.push(`${key} is listed more than once (${seen.get(key)} and ${name})`);
      else seen.set(key, name);
    }
  }
  for (const key of bot) {
    if (!go.socket.has(key)) found.push(`${key} is in the Bot's SocketRoutes (${BOT_ROUTES}) but the instance does not serve it on the socket`);
  }
  return found;
}

function goFiles(base) {
  const files = [];
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) { if (entry.name !== 'testdata' && entry.name !== 'node_modules') walk(path); }
      else if (entry.name.endsWith('.go') && !entry.name.endsWith('_test.go')) {
        files.push({ path: relative(base, path), source: readFileSync(path, 'utf8') });
      }
    }
  };
  for (const dir of GO_DIRS) walk(join(base, dir));
  return files;
}

export function check(base = root) {
  const go = goRoutes(goFiles(base));
  if (go.http.size < 50) return [`only ${go.http.size} Go /v1 HTTP registrations were found; the scan no longer sees the routes`];
  return problems(go, manifest(readFileSync(join(base, MANIFEST), 'utf8')), botSocketRoutes(readFileSync(join(base, BOT_ROUTES), 'utf8')));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const found = check();
  if (found.length) { console.error(`${MANIFEST} does not match the Go routes:\n${found.join('\n')}`); process.exit(1); }
  console.log('socket routes: the manifest matches every Go registration');
}
