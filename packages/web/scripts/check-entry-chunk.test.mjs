// @vitest-environment node
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { ALLOWED_COPY, checkEntryChunk } from './check-entry-chunk.mjs';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'check-entry-chunk.mjs');
const WASM = new Uint8Array([0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00]);
const INDEX = '<!doctype html><html lang="en"><head>'
  + '<script type="module" crossorigin src="/assets/index-AbC123.js"></script>'
  + '<link rel="modulepreload" crossorigin href="/assets/vendor-Pre111.js">'
  + '<link rel="stylesheet" crossorigin href="/assets/index-Def456.css"></head>'
  + '<body><div id="root" class="d-root"></div></body></html>\n';

let dir;
function put(path, content) {
  const full = join(dir, path);
  mkdirSync(dirname(full), { recursive: true });
  writeFileSync(full, content);
}
function cleanDist() {
  put('index.html', INDEX);
  put('assets/index-AbC123.js', 'const w=new URL("/assets/dilla_core_wasm_bg-Jkl012.wasm",import.meta.url);import("./load-Mno345.js");\n');
  put('assets/vendor-Pre111.js', 'export const react=1;\n');
  put('assets/index-Def456.css', 'body{margin:0}\n');
  // The lazily loaded call chunk may name livekit and rnnoise; only the entry and what it preloads may not.
  put('assets/load-Mno345.js', 'export const livekit="livekit-client";const n="rnnoise";\n');
  put('assets/index-Lazy999.js', 'export const sdk="livekit";\n');
  put('assets/dilla_core_wasm_bg-Jkl012.wasm', WASM);
  put('assets/rnnoise-Qrs678.wasm', WASM);
}

beforeEach(() => { dir = mkdtempSync(join(tmpdir(), 'dilla-web-entry-')); });
afterEach(() => { rmSync(dir, { recursive: true, force: true }); });

describe('checkEntryChunk (L-WEB-51, decision 5)', () => {
  it('passes a clean dist', () => {
    cleanDist();
    expect(checkEntryChunk(dir)).toEqual([]);
  });
  it('fails an entry that contains livekit', () => {
    cleanDist();
    put('assets/index-AbC123.js', 'import{Room}from"livekit-client";\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/index-AbC123.js: the entry chunk contains livekit']);
  });
  it('fails an entry that contains rnnoise', () => {
    cleanDist();
    put('assets/index-AbC123.js', 'const u="/assets/rnnoise-Qrs678.wasm";\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/index-AbC123.js: the entry chunk contains rnnoise']);
  });
  it('passes an entry whose copy names the product RNNoise (the voice settings hint), but not its lower-case code', () => {
    cleanDist();
    put('assets/index-AbC123.js', 'const s={"hint":"Picks the best processor this device can run. Falls back to RNNoise."};\n');
    expect(checkEntryChunk(dir)).toEqual([]);
    put('assets/index-AbC123.js', 'const s={"hint":"Falls back to RNNoise."};m._rnnoise_create(0);\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/index-AbC123.js: the entry chunk contains rnnoise']);
  });

  // Task 11 (merge 9+10 minor): only the exact shipped copy is exempt; any other RNNoise, in any case, fails.
  it('fails an entry that names RNNoise anywhere but the exact shipped noise hint', () => {
    cleanDist();
    put('assets/index-AbC123.js', 'export class RNNoiseProcessor {}\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/index-AbC123.js: the entry chunk contains rnnoise']);
    put('assets/index-AbC123.js', 'const s={"hint":"Uses RNNoise."};\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/index-AbC123.js: the entry chunk contains rnnoise']);
  });

  it('exempts copy that is verbatim in the shipped strings', () => {
    const strings = readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../src/strings/signalSettings.ts'), 'utf8');
    for (const copy of ALLOWED_COPY) expect(strings).toContain(copy);
  });
  it('fails a chunk the entry preloads that contains livekit, matching any case', () => {
    cleanDist();
    put('assets/vendor-Pre111.js', 'class LiveKitRoom{}\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/vendor-Pre111.js: the entry chunk contains livekit']);
  });
  it('fails a dist without an entry script', () => {
    cleanDist();
    put('index.html', '<!doctype html><html><head></head><body></body></html>\n');
    expect(checkEntryChunk(dir)).toEqual(['index.html: no entry <script type="module" src>']);
  });
  it('fails an entry or preload src that maps to no file in dist (a changed base would otherwise scan nothing)', () => {
    cleanDist();
    put('index.html', INDEX.replace('src="/assets/index-AbC123.js"', 'src="/app/assets/index-AbC123.js"')
      .replace('href="/assets/vendor-Pre111.js"', 'href="https://cdn.test/assets/vendor-Pre111.js"'));
    expect(checkEntryChunk(dir)).toEqual([
      'index.html: entry app/assets/index-AbC123.js is not in dist',
      'index.html: entry https://cdn.test/assets/vendor-Pre111.js is not in dist',
    ]);
  });
  it('fails two core wasm files (a second core download)', () => {
    cleanDist();
    put('assets/dilla_core_wasm_bg-Zzz999.wasm', WASM);
    expect(checkEntryChunk(dir)).toEqual(['assets: expected exactly 1 dilla_core_wasm_bg-*.wasm, found 2']);
  });
  it('fails no RNNoise wasm and two RNNoise wasm files', () => {
    cleanDist();
    rmSync(join(dir, 'assets/rnnoise-Qrs678.wasm'));
    expect(checkEntryChunk(dir)).toEqual(['assets: expected exactly 1 rnnoise-*.wasm, found 0']);
    put('assets/rnnoise-Qrs678.wasm', WASM);
    put('assets/rnnoise-Two222.wasm', WASM);
    expect(checkEntryChunk(dir)).toEqual(['assets: expected exactly 1 rnnoise-*.wasm, found 2']);
  });
  it('fails a chunk that still names a core wasm path the bundler did not emit (an unresolved second URL)', () => {
    cleanDist();
    put('assets/load-Mno345.js', 'const u=new URL("../wasm/dilla_core_wasm_bg.wasm",import.meta.url).href;\n');
    expect(checkEntryChunk(dir)).toEqual(['assets/load-Mno345.js: names dilla_core_wasm_bg.wasm, which is not the one emitted core wasm']);
  });
  it('exits 1 with the problems on stderr, 0 on a clean dist, 2 without an argument', () => {
    cleanDist();
    expect(spawnSync(process.execPath, [SCRIPT, dir], { encoding: 'utf8' }).status).toBe(0);
    put('assets/index-AbC123.js', 'livekit\n');
    const bad = spawnSync(process.execPath, [SCRIPT, dir], { encoding: 'utf8' });
    expect(bad.status).toBe(1);
    expect(bad.stderr).toContain('the entry chunk contains livekit');
    expect(spawnSync(process.execPath, [SCRIPT], { encoding: 'utf8' }).status).toBe(2);
  });
});
