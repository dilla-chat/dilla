// Dumps every HTTP route of packages/client-core (http/routes.ts and negotiate-session.ts) as JSON, read from the
// TypeScript source with the compiler's parser (nothing is executed):
//   [{ name, method, path, request, response, auth, ok, transport }]
//   name      the Routes method, snake_case
//   path      the template: each caller-supplied piece is `{name}`, in call order; a trailing `?k={v}` group is optional
//   request   'none' (no body) | 'cbor' (application/cbor) | 'octet' (application/octet-stream)
//   response  'none' (the answer is not read) | 'body' (the body is read) | 'status' (the status is read too)
//   auth      false when the route is sent without the bearer
//   ok        the statuses the route lists in its `ok` option, sorted; [] when it uses the default 200/201/204
//   transport 'socket' when socket-routes.ts sends the route as a gateway request frame (the instance does not serve it
//             over HTTP), else 'http'
// The Rust test `route_table_matches_ts` runs this and compares it to `dilla_client::http::routes::TABLE`.
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

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

const HELPERS = {
  blobPath: '/v1/channels/{channel_id}/blobs/{blob_id}',
  rolePath: '/v1/communities/{community_id}/members/{user_id}/roles/{role_id}',
  overwritePath: '/v1/channels/{channel_id}/overwrites/{kind}/{target_id}',
};
const SLOT_CALLS = new Set(['id', 'digest', 'count', 'limit']);

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
    if (callee === 'callPath') return '/v1/calls/{call_id}' + (node.arguments[1] ? evalPath(node.arguments[1], scope) : '');
    if (HELPERS[callee]) return HELPERS[callee];
    return slot(node);
  }
  throw new Error('cannot evaluate path piece: ' + node.getText());
}
function slot(node) {
  if (ts.isCallExpression(node)) {
    const callee = node.expression.getText();
    const first = node.arguments[0];
    if (SLOT_CALLS.has(callee) && ts.isStringLiteral(first)) return `{${snake(first.text)}}`;
    if (callee === 'kindOf') return '{kind}';
    if (callee === 'toHex' || callee === 'encodeURIComponent') return `{${snake(first.getText())}}`;
    throw new Error('unknown slot call: ' + node.getText());
  }
  if (ts.isIdentifier(node)) return constants.has(node.text) ? constants.get(node.text) : `{${snake(node.text)}}`;
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

function options(obj) {
  const body = prop(obj, 'body');
  const ct = valueOf(prop(obj, 'contentType'));
  const auth = valueOf(prop(obj, 'auth'));
  const ok = valueOf(prop(obj, 'ok'));
  return {
    request: body === undefined ? 'none' : ct && ct.text === 'application/octet-stream' ? 'octet' : 'cbor',
    auth: !(auth && auth.kind === ts.SyntaxKind.FalseKeyword),
    ok: ok && ts.isArrayLiteralExpression(ok) ? ok.elements.map((e) => Number(e.text)).sort((a, b) => a - b) : [],
  };
}

const routes = new Map();
function record(entry) {
  const key = `${entry.method} ${entry.path}`;
  const old = routes.get(key);
  if (old) {
    if (JSON.stringify({ ...old, name: 0 }) !== JSON.stringify({ ...entry, name: 0 })) {
      throw new Error(`route ${key} is used with two shapes: ${JSON.stringify(old)} vs ${JSON.stringify(entry)}`);
    }
    return;
  }
  routes.set(key, entry);
}

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
const socketFile = load('socket-routes.ts');
const findSocketTable = (n) => {
  if (ts.isVariableDeclaration(n) && n.name.getText() === 'SOCKET_ROUTES') {
    for (const e of n.initializer.elements) socketRoutes.push({ method: valueOf(prop(e, 'method')).text, pattern: valueOf(prop(e, 'pattern')).text });
  }
  ts.forEachChild(n, findSocketTable);
};
findSocketTable(socketFile);
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
const out = [...routes.values()].sort((a, b) => (a.path + a.method).localeCompare(b.path + b.method));
process.stdout.write(JSON.stringify(out, null, 1) + '\n');
