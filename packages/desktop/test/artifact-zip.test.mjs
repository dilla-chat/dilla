import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { ARTIFACT_BLOB_HOST, downloadArtifactZip, extractArtifactZip } from '../scripts/artifact-zip.mjs';
import { zip } from './support/zip-writer.mjs';

const NAMES = ['dilla-1.2.3-win-x64.exe', 'build-provenance-win-x64.json'];
const good = () => [{ name: NAMES[0], data: 'MZ installer bytes' }, { name: NAMES[1], data: '{"format":1}', method: 0 }];

async function setup() {
 const root = await mkdtemp(join(tmpdir(), 'dilla-artifact-zip-'));
 let n = 0;
 const extract = async (bytes, options = {}) => {
  const file = join(root, `${++n}.zip`), directory = join(root, `out-${n}`);
  await writeFile(file, bytes); await mkdir(directory);
  const result = await extractArtifactZip({ file, names: NAMES, directory, ...options });
  return { result, directory };
 };
 return { root, extract, cleanup: () => rm(root, { recursive: true, force: true }) };
}

test('an artifact zip with exactly the expected entries is read in-process, stored or deflated, with or without data descriptors', async () => {
 const s = await setup();
 try {
  for (const descriptor of [false, true]) {
   const { directory } = await s.extract(zip(good().map(entry => ({ ...entry, descriptor }))));
   assert.deepEqual((await readdir(directory)).sort(), [...NAMES].sort());
   assert.equal(await readFile(join(directory, NAMES[0]), 'utf8'), 'MZ installer bytes');
   assert.equal(await readFile(join(directory, NAMES[1]), 'utf8'), '{"format":1}');
  }
 } finally { await s.cleanup(); }
});

test('zip-slip names are refused: absolute paths, .., separators, NUL', async () => {
 const s = await setup();
 try {
  for (const name of ['/etc/passwd', '../dilla-1.2.3-win-x64.exe', 'x/../dilla-1.2.3-win-x64.exe', 'sub/dilla-1.2.3-win-x64.exe', '..\\dilla-1.2.3-win-x64.exe', 'C:\\x', 'dilla-1.2.3-win-x64.exe\0.txt', '..']) {
   const entries = good(); entries[0].name = name;
   await assert.rejects(s.extract(zip(entries)), /entry name/i, name);
  }
  // The local header may not name another file than the central directory.
  const entries = good(); entries[0].localName = '../../evil';
  await assert.rejects(s.extract(zip(entries)), /local header/i);
  assert.deepEqual(await readdir(s.root).then(list => list.filter(name => !/^(\d+\.zip|out-\d+)$/.test(name))), [], 'nothing was written outside the output directories');
 } finally { await s.cleanup(); }
});

test('duplicate, unexpected, missing and directory entries are refused', async () => {
 const s = await setup();
 try {
  await assert.rejects(s.extract(zip([...good(), { name: NAMES[0], data: 'second copy' }])), /duplicate|exact/i);
  await assert.rejects(s.extract(zip([...good(), { name: 'extra.sh', data: 'planted' }])), /unexpected|exact/i);
  await assert.rejects(s.extract(zip(good().slice(0, 1))), /missing|exact/i);
  await assert.rejects(s.extract(zip([good()[0], { name: NAMES[1] + '/', data: '', method: 0, mode: 0o040755 }])), /entry name|directory/i);
 } finally { await s.cleanup(); }
});

test('symlink and special entries are refused', async () => {
 const s = await setup();
 try {
  for (const mode of [0o120777, 0o020644, 0o060644, 0o010644, 0o140644, 0o040755]) {
   const entries = good(); entries[0].mode = mode;
   await assert.rejects(s.extract(zip(entries)), /regular file/i, mode.toString(8));
  }
  const entries = good(); entries[0].versionMadeBy = 0x0014; entries[0].externalAttributes = 0x10; // MS-DOS directory
  await assert.rejects(s.extract(zip(entries)), /regular file/i);
 } finally { await s.cleanup(); }
});

test('oversized entries, a lying size, a bad CRC and a bounded total are enforced', async () => {
 const s = await setup();
 try {
  await assert.rejects(s.extract(zip(good()), { maxEntry: 10 }), /too large/i);
  await assert.rejects(s.extract(zip(good()), { maxTotal: 20 }), /too large/i);
  // A deflate bomb that declares a small size is cut off at the declared size.
  const bomb = good(); bomb[0].data = Buffer.alloc(1024 * 1024); bomb[0].size = 100;
  await assert.rejects(s.extract(zip(bomb)), /size/i);
  const short = good(); short[0].size = 1000;
  await assert.rejects(s.extract(zip(short)), /size/i);
  const crc = good(); crc[0].crc = 12345;
  await assert.rejects(s.extract(zip(crc)), /crc/i);
 } finally { await s.cleanup(); }
});

test('encrypted entries, unknown methods, zip64 markers, multi-disk archives and trailing bytes are refused', async () => {
 const s = await setup();
 try {
  const encrypted = good(); encrypted[0].flags = 0x1;
  await assert.rejects(s.extract(zip(encrypted)), /encrypted|flags/i);
  const method = good(); method[0].method = 12; method[0].data = 'x';
  await assert.rejects(s.extract(zip(method)), /method/i);
  await assert.rejects(s.extract(zip(good(), { eocd: { entries: 0xffff } })), /zip64|entries/i);
  await assert.rejects(s.extract(zip(good(), { eocd: { disk: 1 } })), /disk/i);
  await assert.rejects(s.extract(zip(good(), { trailing: Buffer.from('appended') })), /end of central directory|trailing/i);
  await assert.rejects(s.extract(Buffer.from('not a zip')), /end of central directory/i);
  const overlap = good(); overlap[1].offset = 0;
  await assert.rejects(s.extract(zip(overlap)), /overlap|local header/i);
 } finally { await s.cleanup(); }
});

test('an existing output file is never overwritten', async () => {
 const s = await setup();
 try {
  const file = join(s.root, 'a.zip'), directory = join(s.root, 'out'); await mkdir(directory);
  await writeFile(file, zip(good())); await writeFile(join(directory, NAMES[1]), 'pre-existing');
  await assert.rejects(extractArtifactZip({ file, names: NAMES, directory }), /EEXIST|exists/);
  assert.equal(await readFile(join(directory, NAMES[1]), 'utf8'), 'pre-existing');
 } finally { await s.cleanup(); }
});

/** The REST endpoint answers 302 to the blob host; the blob host serves the bytes. */
function artifactHost(blobs, { location } = {}) {
 const log = [];
 const fetchImpl = async (input, init = {}) => {
  const url = new URL(String(input)); log.push({ url: url.href, redirect: init.redirect, authorization: init.headers?.Authorization });
  const match = /^\/repos\/dilla-chat\/dilla\/actions\/artifacts\/(\d+)\/zip$/.exec(url.pathname);
  if (url.origin === 'https://api.github.com' && match && blobs.has(Number(match[1]))) {
   return new Response(null, { status: 302, headers: { Location: location?.(match[1]) ?? `https://productionresultssa7.blob.core.windows.net/actions-results/${match[1]}.zip?sig=x` } });
  }
  const id = /^\/actions-results\/(\d+)\.zip$/.exec(url.pathname)?.[1];
  if (ARTIFACT_BLOB_HOST.test(url.hostname) && id && blobs.has(Number(id))) return new Response(blobs.get(Number(id)), { status: 200 });
  return new Response('Not Found', { status: 404 });
 };
 return { fetchImpl, log };
}

test('an artifact zip is fetched as an opaque blob and kept only when its sha256 is the gate’s digest', async () => {
 const s = await setup();
 try {
  const bytes = zip(good()), digest = 'sha256:' + createHash('sha256').update(bytes).digest('hex');
  const host = artifactHost(new Map([[41, bytes]]));
  const file = join(s.root, 'blob.zip');
  await downloadArtifactZip({ token: 't', id: 41, digest, file, fetchImpl: host.fetchImpl });
  assert.deepEqual(await readFile(file), bytes);
  assert.equal(host.log[0].redirect, 'manual');
  assert.equal(host.log[0].authorization, 'Bearer t');
  assert.equal(host.log[1].redirect, 'error');
  assert.equal(host.log[1].authorization, undefined, 'the token never goes to the blob host');
  await assert.rejects(downloadArtifactZip({ token: 't', id: 41, digest: 'sha256:' + 'f'.repeat(64), file: join(s.root, 'other.zip'), fetchImpl: host.fetchImpl }), /digest/i);
  await assert.rejects(downloadArtifactZip({ token: 't', id: 41, digest, file, fetchImpl: host.fetchImpl }), /EEXIST|exists/, 'never overwrites');
  const elsewhere = artifactHost(new Map([[41, bytes]]), { location: id => `https://evil.example/actions-results/${id}.zip` });
  await assert.rejects(downloadArtifactZip({ token: 't', id: 41, digest, file: join(s.root, 'evil.zip'), fetchImpl: elsewhere.fetchImpl }), /host/i);
  const plain = artifactHost(new Map([[41, bytes]]), { location: id => `http://productionresultssa7.blob.core.windows.net/actions-results/${id}.zip` });
  await assert.rejects(downloadArtifactZip({ token: 't', id: 41, digest, file: join(s.root, 'http.zip'), fetchImpl: plain.fetchImpl }), /host/i);
  await assert.rejects(downloadArtifactZip({ token: 't', id: 41, digest, file: join(s.root, 'big.zip'), fetchImpl: host.fetchImpl, maxBytes: 10 }), /too large/i);
  await assert.rejects(downloadArtifactZip({ token: 't', id: '41/../x', digest, file: join(s.root, 'id.zip'), fetchImpl: host.fetchImpl }), /artifact id/i);
 } finally { await s.cleanup(); }
});
