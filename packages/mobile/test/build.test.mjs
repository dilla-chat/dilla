import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const CSP = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self'; font-src 'self'; worker-src 'self'; img-src 'self' blob:; media-src blob:; connect-src 'self' https: wss:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'";

// A stand-in for packages/web/dist with the shape vite writes, so the test needs neither the wasm toolchain nor a web build.
let out = '';
before(() => {
  const work = mkdtempSync(join(tmpdir(), 'dilla-mobile-build-'));
  const client = join(work, 'web-dist');
  mkdirSync(join(client, 'assets'), { recursive: true });
  writeFileSync(join(client, 'index.html'), `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>dilla</title>
    <script type="module" crossorigin src="/assets/index-AAAA.js"></script>
    <link rel="modulepreload" crossorigin href="/assets/stats-BBBB.js">
  </head>
  <body><div id="root"></div></body>
</html>
`);
  writeFileSync(join(client, 'assets/index-AAAA.js'), 'console.log("entry")\n');
  writeFileSync(join(client, 'assets/stats-BBBB.js'), 'export {}\n');
  writeFileSync(join(client, 'dilla-manifest.json'), '{"v":1,"files":[]}\n');
  out = join(work, 'dist');
  execFileSync(process.execPath, [join(root, 'scripts/build.mjs'), `--client-dir=${client}`, `--out-dir=${out}`], { stdio: 'pipe' });
});

function files(dir) {
  return readdirSync(dir, { recursive: true, withFileTypes: true }).filter(entry => entry.isFile())
    .map(entry => relative(dir, join(entry.parentPath, entry.name)).split('\\').join('/'));
}

test('index.html carries the mobile CSP and loads mobile-bridge.js before the first module script', () => {
  const html = readFileSync(join(out, 'index.html'), 'utf8');
  const metas = [...html.matchAll(/<meta http-equiv="Content-Security-Policy" content="([^"]*)">/g)];
  assert.equal(metas.length, 1);
  assert.equal(metas[0][1], CSP);
  assert.doesNotMatch(CSP, /frame-ancestors/);
  const bridge = html.indexOf('<script src="/mobile-bridge.js"></script>');
  const module = html.search(/<script[^>]*type="module"/);
  assert.ok(bridge > 0 && module > 0 && bridge < module, 'mobile-bridge.js must load before the module entry');
  assert.ok(html.indexOf('Content-Security-Policy') < bridge, 'the CSP must precede every script');
  assert.ok(html.indexOf('<meta charset') < html.indexOf('Content-Security-Policy'));
});

test('the picker page ships with the same CSP and only its own external script', () => {
  const html = readFileSync(join(out, 'picker.html'), 'utf8');
  assert.ok(html.includes(`<meta http-equiv="Content-Security-Policy" content="${CSP}">`));
  assert.match(html, /<script src="\/picker.js"><\/script>/);
});

test('no shipped page has an inline script or inline style the CSP would block', () => {
  for (const name of files(out).filter(name => name.endsWith('.html'))) {
    const html = readFileSync(join(out, name), 'utf8');
    for (const tag of html.match(/<script\b[^>]*>/g) ?? []) assert.match(tag, /\bsrc="/, `${name}: ${tag}`);
    assert.doesNotMatch(html, /<style\b|\sstyle="/, name);
  }
});

test('the bridge and picker bundles are built as classic scripts', () => {
  for (const name of ['mobile-bridge.js', 'picker.js']) {
    const js = readFileSync(join(out, name), 'utf8');
    assert.ok(js.length > 100, name);
    assert.doesNotMatch(js, /^\s*(import|export)\s/m, `${name} must not be an ES module`);
  }
  assert.match(readFileSync(join(out, 'mobile-bridge.js'), 'utf8'), /dillaMobile/);
});

test('every file under dist is in dilla-manifest.json with a matching sha256', () => {
  const manifest = JSON.parse(readFileSync(join(out, 'dilla-manifest.json'), 'utf8'));
  assert.equal(manifest.v, 1);
  const listed = new Map(manifest.files.map(file => [file.path, file]));
  const present = files(out).filter(name => name !== 'dilla-manifest.json');
  assert.deepEqual([...listed.keys()].sort(), present.sort());
  for (const name of present) {
    const bytes = readFileSync(join(out, name));
    assert.equal(listed.get(name).sha256, createHash('sha256').update(bytes).digest('hex'), name);
    assert.equal(listed.get(name).size, bytes.byteLength, name);
  }
  for (const name of ['index.html', 'picker.html', 'picker.css', 'mobile-bridge.js', 'picker.js', 'assets/index-AAAA.js']) assert.ok(listed.has(name), name);
});

test('capacitor.config.ts bundles the client: no server.url, no mixed content, no cleartext', () => {
  const config = readFileSync(join(root, 'capacitor.config.ts'), 'utf8');
  assert.doesNotMatch(config, /\burl\s*:/);
  assert.doesNotMatch(config, /cleartext\s*:\s*true/);
  assert.match(config, /appId:\s*'chat\.dilla\.app'/);
  assert.match(config, /appName:\s*'Dilla'/);
  assert.match(config, /webDir:\s*'dist'/);
  assert.match(config, /allowMixedContent:\s*false/);
});
