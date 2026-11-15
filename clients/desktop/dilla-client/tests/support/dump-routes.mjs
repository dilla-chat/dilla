// Dumps every HTTP route of packages/client-core (http/routes.ts and negotiate-session.ts) as JSON, read from the
// TypeScript source with the compiler's parser (nothing is executed):
//   [{ name, method, path, request, response, auth, ok, maxBody, transport }]
//   name      the Routes method, snake_case
//   path      the template: each caller-supplied piece is `{name}`, in call order; a trailing `?k={v}` group is optional
//   request   'none' (no body) | 'cbor' (application/cbor) | 'octet' (application/octet-stream)
//   response  'none' (the answer is not read) | 'body' (the body is read) | 'status' (the status is read too)
//   auth      false when the route is sent without the bearer
//   maxBody   the `maxBodyBytes` bound on a success answer when it is a literal (or a product of literals), else null
//   ok        the statuses the route lists in its `ok` option, sorted; [] when it uses the default 200/201/204
//   transport 'socket' when socket-routes.ts sends the route as a gateway request frame (the instance does not serve it
//             over HTTP), else 'http'
//   slots     each `{name}` of the path before any `?`, in order, with the routes.ts check that built it:
//             'id' (id()), 'digest' (digest()), 'count' (count()), 'kind' (kindOf()), 'hex' (a bare toHex()),
//             'uri' (encodeURIComponent()), 'raw' (a value interpolated as it is: the method checks it itself)
//   names     every Routes method that sends this method and path (one route; `name` is the first of them). Two
//             methods collapse into one row only when their shapes are the same; different shapes throw.
// The Rust test `route_table_matches_ts` runs this and compares it to `dilla_client::http::routes::TABLE`. The module
// also exports `SLOT_CHECKS`, `slotCheck` and `collapse` (the slot validators and the collapse rule), which the test
// exercises directly (`--self-test`).
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..', '..');
const ts = createRequire(join(root, 'package.json'))('typescript');
const snake = (s) => s.replace(/([A-Z]+)([A-Z][a-z])/g, '$1_$2').replace(/([a-z0-9])([A-Z])/g, '$1_$2').toLowerCase();

function load(file) {
  const text = readFileSync(join(root, 'packages/client-core/src/http', file), 'utf8');
  return ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
}
const FILES = ['routes.ts', 'negotiate-session.ts'];
const constants = new Map();
for (const f of FILES) {
  for (const st of load(f).statements) {
    if (!ts.isVariableStatement(st)) continue;
    for (const d of st.declarationList.declarations) {
      if (ts.isIdentifier(d.name) && d.initializer && ts.isNumericLiteral(d.initializer)) constants.set(d.name.text, d.initializer.text);
    }
  }
}

// A slot is written `{name:check}` while a path is evaluated; `finish` splits the checks off.
const HELPERS = {
  blobPath: '/v1/channels/{channel_id:id}/blobs/{blob_id:digest}',
  rolePath: '/v1/communities/{community_id:id}/members/{user_id:id}/roles/{role_id:id}',
  overwritePath: '/v1/channels/{channel_id:id}/overwrites/{kind:kind}/{target_id:id}',
};
/** routes.ts's slot validators, by the function that builds the slot, and the check each one makes. */
export const SLOT_CHECKS = {
  id: 'id', // 16 bytes, written as 32 lowercase hex digits
  digest: 'digest', // 32 bytes, 64 lowercase hex digits
  count: 'count', // a bigint count, written in decimal
  limit: 'limit', // 1..max, a query value only
  kindOf: 'kind', // 0 or 1
  toHex: 'hex', // bytes written as hex with no length check
  encodeURIComponent: 'uri', // any text, one encoded segment
};
/** The check routes.ts makes for a slot built by calling `callee`, or null when it is not a slot validator. */
export function slotCheck(callee) {
  return Object.hasOwn(SLOT_CHECKS, callee) ? SLOT_CHECKS[callee] : null;
}
const NAMED_SLOT_CALLS = new Set(['id', 'digest', 'count', 'limit']);

function evalPath(node, scope) {
  if (ts.isParenthesizedExpression(node)) return evalPath(node.expression, scope);
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  if (ts.isTemplateExpression(node)) {
    return node.head.text + node.templateSpans.map((s) => slot(s.expression) + s.literal.text).join('');
  }
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.PlusToken) {
    return evalPath(node.left, scope) + evalPath(node.right, scope);
  }
  if (ts.isConditionalExpression(node)) return evalPath(node.whenTrue, scope); // the optional `?after=` group
  if (ts.isIdentifier(node)) {
    const init = scope.get(node.text);
    return init ? evalPath(init, scope) : slot(node);
  }
  if (ts.isCallExpression(node)) {
    const callee = node.expression.getText();
    if (callee === 'callPath') return '/v1/calls/{call_id:id}' + (node.arguments[1] ? evalPath(node.arguments[1], scope) : '');
    if (HELPERS[callee]) return HELPERS[callee];
    return slot(node);
  }
  throw new Error('cannot evaluate path piece: ' + node.getText());
}
function slot(node) {
  if (ts.isCallExpression(node)) {
    const callee = node.expression.getText();
    const check = slotCheck(callee);
    const first = node.arguments[0];
    if (check === null) throw new Error('unknown slot call: ' + node.getText());
    if (NAMED_SLOT_CALLS.has(callee) && ts.isStringLiteral(first)) return `{${snake(first.text)}:${check}}`;
    if (callee === 'kindOf') return `{kind:${check}}`;
    if (!NAMED_SLOT_CALLS.has(callee)) return `{${snake(first.getText())}:${check}}`;
    throw new Error('unknown slot call: ' + node.getText());
  }
  if (ts.isIdentifier(node)) return constants.has(node.text) ? constants.get(node.text) : `{${snake(node.text)}:raw}`;
  if (ts.isNumericLiteral(node)) return node.text;
  throw new Error('unknown slot: ' + node.getText());
}

const isProp = (p) => ts.isPropertyAssignment(p) || ts.isShorthandPropertyAssignment(p);
const prop = (obj, name) => obj.properties.find((p) => isProp(p) && p.name.getText() === name);
const valueOf = (p) => (p && ts.isPropertyAssignment(p) ? p.initializer : undefined);

function responseOf(call, fn) {
  const awaited = ts.isAwaitExpression(call.parent) ? call.parent : call;
  const holder = awaited.parent;
  if (ts.isExpressionStatement(holder)) return 'none';
  // `const response = await this.send(...)`: the status matters when the method reads response.status
  if (ts.isVariableDeclaration(holder) && ts.isIdentifier(holder.name)) {
    return fn.getText().includes(`${holder.name.text}.status`) ? 'status' : 'body';
  }
  return 'body';
}

function numberOf(node) {
  if (!node) return null;
  if (ts.isNumericLiteral(node)) return Number(node.text);
  if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.AsteriskToken) {
    const a = numberOf(node.left);
    const b = numberOf(node.right);
    return a === null || b === null ? null : a * b;
  }
  return null;
}

function options(obj) {
  const maxBody = numberOf(valueOf(prop(obj, 'maxBodyBytes')));
  const body = prop(obj, 'body');
  const ct = valueOf(prop(obj, 'contentType'));
  const auth = valueOf(prop(obj, 'auth'));
  const ok = valueOf(prop(obj, 'ok'));
  return {
    request: body === undefined ? 'none' : ct && ct.text === 'application/octet-stream' ? 'octet' : 'cbor',
    auth: !(auth && auth.kind === ts.SyntaxKind.FalseKeyword),
    maxBody,
    ok: ok && ts.isArrayLiteralExpression(ok) ? ok.elements.map((e) => Number(e.text)).sort((a, b) => a - b) : [],
  };
}

/** `entry` with its path's `{name:check}` slots split into `path` (`{name}`) and `slots` (those before any `?`). */
function finish(entry) {
  const bare = entry.path.split('?', 1)[0];
  const slots = [...bare.matchAll(/\{([a-z0-9_]+):([a-z]+)\}/g)].map((m) => ({ name: m[1], check: m[2] }));
  return { ...entry, path: entry.path.replace(/\{([a-z0-9_]+):[a-z]+\}/g, '{$1}'), slots };
}

/** The collapse rule: `entry` (one Routes method's request) joins `table`, keyed by method and path. A second method
 *  with the same method and path is the same route only when everything else it sends is the same (its name joins the
 *  row's `names`); anything else throws. */
export function collapse(table, entry) {
  const key = `${entry.method} ${entry.path}`;
  const old = table.get(key);
  const shape = (e) => JSON.stringify({ ...e, name: 0, names: 0 });
  if (old) {
    if (shape(old) !== shape(entry)) {
      throw new Error(`route ${key} is used with two shapes: ${JSON.stringify(old)} vs ${JSON.stringify(entry)}`);
    }
    if (!old.names.includes(entry.name)) old.names.push(entry.name);
    return old;
  }
  const row = { ...entry, names: [entry.name] };
  table.set(key, row);
  return row;
}

function dump() {
  const routes = new Map();
  const record = (entry) => collapse(routes, finish(entry));
  for (const f of FILES) {
    const walk = (n) => {
      if ((ts.isMethodDeclaration(n) || ts.isFunctionDeclaration(n)) && n.name && n.body && !['body', 'send', 'request'].includes(n.name.text)) {
        const scope = new Map();
        const visit = (m) => {
          if (ts.isVariableDeclaration(m) && ts.isIdentifier(m.name) && m.initializer && m.parent.flags & ts.NodeFlags.Const) scope.set(m.name.text, m.initializer);
          if (ts.isCallExpression(m)) {
            const callee = m.expression.getText();
            const name = snake(n.name.text);
            if (callee === 'this.body' && ts.isStringLiteral(m.arguments[0])) {
              const opts = m.arguments[4] && ts.isObjectLiteralExpression(m.arguments[4]) ? options(m.arguments[4]) : options({ properties: [] });
              record({ name, method: m.arguments[0].text, path: evalPath(m.arguments[1], scope), ...opts, response: responseOf(m, n) });
            } else if ((callee === 'this.send' || callee === 'http.request') && m.arguments[0] && ts.isObjectLiteralExpression(m.arguments[0])) {
              const o = m.arguments[0];
              const method = valueOf(prop(o, 'method'));
              const path = valueOf(prop(o, 'path'));
              if (method && path) record({ name, method: method.text, path: evalPath(path, scope), ...options(o), response: responseOf(m, n) });
            }
          }
          ts.forEachChild(m, visit);
        };
        visit(n.body);
        return;
      }
      ts.forEachChild(n, walk);
    };
    walk(load(f));
  }
  // socket-routes.ts: Go ServeMux matching of SOCKET_ROUTES against the path template.
  const socketRoutes = [];
  const findSocketTable = (n) => {
    if (ts.isVariableDeclaration(n) && n.name.getText() === 'SOCKET_ROUTES') {
      for (const e of n.initializer.elements) socketRoutes.push({ method: valueOf(prop(e, 'method')).text, pattern: valueOf(prop(e, 'pattern')).text });
    }
    ts.forEachChild(n, findSocketTable);
  };
  findSocketTable(load('socket-routes.ts'));
  if (socketRoutes.length < 50) throw new Error('SOCKET_ROUTES was not read');
  const matches = (segments, parts) => {
    for (let i = 0; i < segments.length; i++) {
      const segment = segments[i];
      if (segment.startsWith('{') && segment.endsWith('...}')) return i < parts.length;
      if (i >= parts.length) return false;
      if (segment.startsWith('{') && segment.endsWith('}')) { if (parts[i] === '') return false; continue; }
      if (segment !== parts[i]) return false;
    }
    return segments.length === parts.length;
  };
  const transportOf = (method, path) => {
    const parts = path.split('?', 1)[0].split('/').slice(1);
    return socketRoutes.some((r) => r.method === method && matches(r.pattern.split('/').slice(1), parts)) ? 'socket' : 'http';
  };
  for (const e of routes.values()) e.transport = transportOf(e.method, e.path);
  return [...routes.values()].sort((a, b) => (a.path + a.method).localeCompare(b.path + b.method));
}

/** The collapse rule and the slot validators, checked on their own (`node dump-routes.mjs --self-test`). */
function selfTest() {
  const fail = (what) => { throw new Error('self-test: ' + what); };
  const base = { method: 'GET', path: '/v1/x/{id}', request: 'none', response: 'body', auth: true, ok: [], maxBody: null, slots: [{ name: 'id', check: 'id' }] };
  const table = new Map();
  collapse(table, { ...base, name: 'first' });
  const row = collapse(table, { ...base, name: 'second' });
  if (table.size !== 1 || row.name !== 'first' || JSON.stringify(row.names) !== '["first","second"]') fail('one shape collapses into one row');
  collapse(table, { ...base, name: 'second' });
  if (row.names.length !== 2) fail('a method is named once');
  for (const differ of [{ auth: false }, { ok: [204] }, { maxBody: 1 }, { response: 'none' }, { slots: [{ name: 'id', check: 'hex' }] }]) {
    let threw = false;
    try { collapse(table, { ...base, ...differ, name: 'other' }); } catch { threw = true; }
    if (!threw) fail('a second shape throws: ' + JSON.stringify(differ));
  }
  collapse(table, { ...base, method: 'DELETE', name: 'other' });
  if (table.size !== 2) fail('another method is another route');
  for (const [callee, check] of [['id', 'id'], ['digest', 'digest'], ['count', 'count'], ['limit', 'limit'], ['kindOf', 'kind'], ['toHex', 'hex'], ['encodeURIComponent', 'uri']]) {
    if (slotCheck(callee) !== check) fail(`${callee} is ${check}`);
  }
  if (slotCheck('toString') !== null || slotCheck('String') !== null) fail('other calls are no validator');
  if (JSON.stringify(finish({ path: '/v1/a/{a_id:id}/b/{n:count}?after={after:raw}' }))
    !== JSON.stringify({ path: '/v1/a/{a_id}/b/{n}?after={after}', slots: [{ name: 'a_id', check: 'id' }, { name: 'n', check: 'count' }] })) {
    fail('finish splits the checks off');
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  if (process.argv.includes('--self-test')) {
    selfTest();
    process.stdout.write('ok\n');
  } else {
    process.stdout.write(JSON.stringify(dump(), null, 1) + '\n');
  }
}

