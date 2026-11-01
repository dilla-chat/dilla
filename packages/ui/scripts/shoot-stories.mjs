// Renders Storybook stories to PNGs for review (L-UI-15, F16).
//
//   node packages/ui/scripts/shoot-stories.mjs <port> <out-dir> <story-id-prefix>…
//
// Serves packages/ui/storybook-static on 127.0.0.1:<port>, opens every story of
// index.json whose id starts with one of the prefixes in each theme with
// Playwright Chromium at 1280×800, and writes <out-dir>/<story id>.<theme>.png.
// Exits 1 when a prefix matched no story or a story page logged an error.
//
// A story whose parameters say `phoneViewport: true` is visited at 360×740 by the
// browser checker (scripts/check-stories.mjs), and its play asserts that: such a story
// is shot only in a pass at that viewport and skipped, by name, in any other.
import http from 'node:http';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const THEMES = ['signal', 'light', 'high-contrast'];
export const VIEWPORT = { width: 1280, height: 800 };
/** The viewport a story declaring `phoneViewport: true` is visited at, as in .storybook/test-runner.ts. */
export const PHONE_VIEWPORT = { width: 360, height: 740 };

/** The viewport a story's parameters declare, or null when it runs at whatever viewport the pass uses. */
export function declaredViewport(parameters) {
  return parameters?.phoneViewport === true ? PHONE_VIEWPORT : null;
}

/** Whether a story with these parameters is shot in a pass at this viewport. */
export function shootsAt(parameters, viewport) {
  const declared = declaredViewport(parameters);
  return declared === null || (declared.width === viewport.width && declared.height === viewport.height);
}

export function parseViewport(arg) {
  if (arg === undefined) return VIEWPORT;
  const match = /^--viewport=([1-9]\d*)x([1-9]\d*)$/.exec(arg);
  if (!match) throw new Error('invalid viewport');
  const width = Number(match[1]);
  const height = Number(match[2]);
  if (!Number.isSafeInteger(width) || !Number.isSafeInteger(height)) throw new Error('invalid viewport');
  return { width, height };
}

export function splitViewportArg(args) {
  const first = args.findIndex(arg => arg.startsWith('--viewport='));
  if (first !== -1 && first !== args.length - 1) throw new Error('viewport must be final');
  return { prefixes: first === -1 ? args : args.slice(0, -1), viewport: parseViewport(first === -1 ? undefined : args[first]) };
}

/** The story ids (never docs entries) that start with one of the prefixes, and the prefixes that matched none. */
export function selectStories(index, prefixes) {
  const ids = Object.values(index.entries)
    .filter(e => e.type === 'story' && prefixes.some(p => e.id.startsWith(p)))
    .map(e => e.id);
  const sorted = [...ids].sort();
  const unmatched = prefixes.filter(p => !sorted.some(id => id.startsWith(p)));
  return { ids: sorted, unmatched };
}

export function storyUrl(port, id, theme) {
  return `http://127.0.0.1:${port}/iframe.html?id=${encodeURIComponent(id)}&globals=theme:${theme}&viewMode=story`;
}

export function outFile(outDir, id, theme) {
  return path.join(outDir, `${id}.${theme}.png`);
}

const TYPES = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.map': 'application/json',
  '.svg': 'image/svg+xml',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.wasm': 'application/wasm',
};

export function contentType(urlPath) {
  const ext = path.extname(urlPath).toLowerCase();
  return Object.hasOwn(TYPES, ext) ? TYPES[ext] : 'application/octet-stream';
}

/** The absolute file a request path names under root, or null when it is malformed or leaves root. */
export function resolveInRoot(root, urlPath) {
  const q = urlPath.indexOf('?');
  const raw = q === -1 ? urlPath : urlPath.slice(0, q);
  let decoded;
  try {
    decoded = decodeURIComponent(raw);
  } catch (e) {
    if (e instanceof URIError) return null;
    throw e;
  }
  if (decoded === '/') return path.join(root, 'index.html');
  const p = path.resolve(root, '.' + decoded);
  if (p === root || p.startsWith(root + path.sep)) return p;
  return null;
}

/** Resolve an existing request file and refuse symlinks that leave the Storybook output. */
export async function resolveExistingInRoot(root, urlPath) {
  const candidate = resolveInRoot(root, urlPath);
  if (candidate === null) return null;
  try {
    const [realRoot, realFile] = await Promise.all([fsp.realpath(root), fsp.realpath(candidate)]);
    return realFile.startsWith(realRoot + path.sep) ? realFile : null;
  } catch {
    return null;
  }
}

function notFound(res) {
  res.writeHead(404, { 'content-length': '0' });
  res.end();
}

function startServer(root, port) {
  const server = http.createServer(async (req, res) => {
    if (req.method !== 'GET' && req.method !== 'HEAD') {
      res.writeHead(405, { 'content-length': '0', allow: 'GET, HEAD' });
      res.end();
      return;
    }
    const file = await resolveExistingInRoot(root, req.url ?? '/');
    if (file === null) { notFound(res); return; }
    let body;
    try {
      const st = await fsp.stat(file);
      if (!st.isFile()) { notFound(res); return; }
      body = await fsp.readFile(file);
    } catch {
      notFound(res);
      return;
    }
    res.writeHead(200, { 'content-type': contentType(file), 'content-length': String(body.length) });
    res.end(req.method === 'HEAD' ? undefined : body);
  });
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => {
      server.off('error', reject);
      resolve(server);
    });
  });
}

export async function shoot({ port, outDir, prefixes, root, viewport = VIEWPORT }) {
  const indexPath = path.join(root, 'index.json');
  if (!fs.existsSync(indexPath)) {
    throw new Error('shoot-stories: ' + root + '/index.json is missing: run build-storybook first');
  }
  const index = JSON.parse(await fsp.readFile(indexPath, 'utf8'));
  const { ids, unmatched } = selectStories(index, prefixes);
  const consoleErrors = [];
  const skipped = [];
  let written = 0;
  const server = await startServer(root, port);
  let browser;
  try {
    const { chromium } = await import('playwright');
    browser = await chromium.launch({ headless: true });
    const context = await browser.newContext({ viewport, deviceScaleFactor: 1 });
    try {
      const parameters = await storyParameters(context, port, ids);
      fs.mkdirSync(outDir, { recursive: true });
      for (const id of ids) {
        if (!shootsAt(parameters.get(id), viewport)) { skipped.push(id); continue; }
        for (const theme of THEMES) {
          const page = await context.newPage();
          try {
            page.on('console', m => { if (m.type() === 'error') consoleErrors.push(`${id} ${theme}: ${m.text()}`); });
            page.on('pageerror', e => consoleErrors.push(`${id} ${theme}: ${e.message}`));
            await page.goto(storyUrl(port, id, theme), { waitUntil: 'load', timeout: 60_000 });
            await page.waitForSelector('#storybook-root > *', { state: 'attached', timeout: 60_000 });
            await page.evaluate(() => document.fonts.ready.then(() => true));
            await page.screenshot({ path: outFile(outDir, id, theme) });
          } finally {
            await page.close();
          }
          written += 1;
        }
      }
    } finally {
      await context.close();
    }
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(() => resolve()));
  }
  return { written, consoleErrors, unmatched, skipped };
}

/** Each story's parameters, read from the Storybook preview the way the test runner's getStoryContext does. */
export async function storyParameters(context, port, ids) {
  const page = await context.newPage();
  try {
    await page.goto(`http://127.0.0.1:${port}/iframe.html`, { waitUntil: 'load', timeout: 60_000 });
    await page.waitForFunction(() => globalThis.__STORYBOOK_PREVIEW__?.storyStoreValue !== undefined, null, { timeout: 60_000 });
    const entries = await page.evaluate(async storyIds => {
      const store = globalThis.__STORYBOOK_PREVIEW__.storyStoreValue;
      const out = [];
      for (const storyId of storyIds) {
        const story = await store.loadStory({ storyId });
        out.push([storyId, { phoneViewport: story.parameters?.phoneViewport }]);
      }
      return out;
    }, ids);
    return new Map(entries);
  } finally {
    await page.close();
  }
}

async function main(argv) {
  const [portArg, outArg, ...rest] = argv;
  const { prefixes, viewport } = splitViewportArg(rest);
  const port = /^\d+$/.test(portArg ?? '') ? Number(portArg) : NaN;
  if (!Number.isInteger(port) || port < 1024 || port > 65535 || !outArg || prefixes.length === 0) {
    process.stderr.write('usage: shoot-stories.mjs <port> <out-dir> <story-id-prefix>…\n');
    return 2;
  }
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', 'storybook-static');
  const outDir = path.resolve(process.cwd(), outArg);
  const { written, consoleErrors, unmatched, skipped } = await shoot({ port, outDir, prefixes, root, viewport });
  for (const p of unmatched) process.stderr.write(`shoot-stories: no story matches "${p}"\n`);
  for (const id of skipped) {
    process.stdout.write(`shoot-stories: skipped ${id}: it declares phoneViewport, shot only at ${PHONE_VIEWPORT.width}x${PHONE_VIEWPORT.height}\n`);
  }
  for (const line of consoleErrors) process.stderr.write(line + '\n');
  process.stdout.write(`shoot-stories: wrote ${written} screenshots to ${outDir}\n`);
  return unmatched.length > 0 || consoleErrors.length > 0 ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2)).then(
    code => { process.exitCode = code; },
    err => { process.stderr.write(String(err && err.stack ? err.stack : err) + '\n'); process.exitCode = 1; },
  );
}
