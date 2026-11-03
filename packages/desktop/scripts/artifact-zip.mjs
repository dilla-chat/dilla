import { createHash } from 'node:crypto';
import { createReadStream, createWriteStream } from 'node:fs';
import { open } from 'node:fs/promises';
import { join } from 'node:path';
import { Readable, Transform } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { crc32, createInflateRaw } from 'node:zlib';

/**
 * The sign job's only way in for CI-built bytes. It never runs an action or a
 * tool that extracts an archive another job produced. Instead it fetches each
 * artifact zip as an opaque blob from the REST API, by the artifact id the gate
 * recorded, and keeps it only if its sha256 equals the digest the gate
 * recorded for that id. It then reads the zip here, in this process, with
 * node: built-ins only (no npm package, because the sign job runs no
 * `npm ci`). It accepts exactly the expected entry names (plain file names),
 * as regular, unencrypted, stored or deflated entries within fixed size
 * bounds. Each entry is written with an exclusive create under the name taken
 * from the caller's fixed list, never from the zip.
 */
const API = 'https://api.github.com/repos/dilla-chat/dilla/actions/artifacts/';
/** Where GitHub's artifact downloads redirect (Actions results storage). */
export const ARTIFACT_BLOB_HOST = /^productionresultssa\d{1,3}\.blob\.core\.windows\.net$/;
const DIGEST = /^sha256:[a-f0-9]{64}$/;
const ID = /^[1-9]\d*$/;
const GiB = 1024 ** 3;
export const ZIP_LIMITS = Object.freeze({ maxBytes: 2 * GiB, maxEntry: GiB, maxTotal: 2 * GiB });
const MAX_ENTRIES = 64, MAX_DIRECTORY = 1024 ** 2;
const EOCD = 0x06054b50, CENTRAL = 0x02014b50, LOCAL = 0x04034b50;

/** Streams one artifact zip from the REST API's redirect to `file` (exclusive create); its sha256 must be `digest`. */
export async function downloadArtifactZip({ token, id, digest, file, fetchImpl = fetch, maxBytes = ZIP_LIMITS.maxBytes }) {
 if (!ID.test(String(id)) || !Number.isSafeInteger(Number(id))) throw new Error('Invalid artifact id');
 if (typeof digest !== 'string' || !DIGEST.test(digest)) throw new Error('Invalid artifact digest');
 if (typeof token !== 'string' || !token) throw new Error('The artifact download requires the workflow’s scoped GitHub token');
 const first = await fetchImpl(`${API}${id}/zip`, {
  headers: { Authorization: `Bearer ${token}`, Accept: 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28' },
  redirect: 'manual', signal: AbortSignal.timeout(60000),
 });
 await first.body?.cancel();
 if (first.status !== 302) throw new Error(`The artifact ${id} download did not redirect to its blob (${first.status})`);
 let location;
 try { location = new URL(first.headers.get('location')); } catch { throw new Error(`The artifact ${id} download redirected to an invalid URL`); }
 if (location.protocol !== 'https:' || !ARTIFACT_BLOB_HOST.test(location.hostname) || location.port !== '' || location.username || location.password) {
  throw new Error(`The artifact ${id} download redirected to an unexpected host`);
 }
 // The blob URL is pre-signed: the token never leaves api.github.com.
 const response = await fetchImpl(location.href, { redirect: 'error', signal: AbortSignal.timeout(30 * 60000) });
 if (!response.ok || !response.body) throw new Error(`The artifact ${id} blob download failed (${response.status})`);
 const hash = createHash('sha256');
 let size = 0;
 const meter = new Transform({
  transform(chunk, _encoding, callback) {
   size += chunk.length;
   if (size > maxBytes) return callback(new Error(`The artifact ${id} zip is too large`));
   hash.update(chunk); callback(null, chunk);
  },
 });
 await pipeline(Readable.fromWeb(response.body), meter, createWriteStream(file, { flags: 'wx', mode: 0o600 }));
 if ('sha256:' + hash.digest('hex') !== digest) throw new Error(`The artifact ${id} zip does not match the digest the gate recorded`);
 return { size };
}

async function readAt(handle, length, position) {
 const buffer = Buffer.alloc(length);
 const { bytesRead } = await handle.read(buffer, 0, length, position);
 if (bytesRead !== length) throw new Error('The artifact zip is truncated');
 return buffer;
}

const UTF8 = new TextDecoder('utf-8', { fatal: true });
/** A plain file name: no separator, drive, parent, NUL or control character. */
function entryName(raw) {
 let name;
 try { name = UTF8.decode(raw); } catch { throw new Error('Refused artifact zip entry name (not UTF-8)'); }
 if (!name || name === '.' || name === '..' || /[/\\:\0]/.test(name) || /[\u0000-\u001f\u007f]/.test(name) || name.startsWith('..')) {
  throw new Error('Refused artifact zip entry name (absolute, parent, nested or special path): ' + JSON.stringify(name));
 }
 return name;
}

/** The central directory: every entry checked before anything is written. */
async function centralDirectory(handle, names, { maxEntry, maxTotal }) {
 const { size } = await handle.stat();
 const tailLength = Math.min(size, 22 + 0xffff);
 const tail = await readAt(handle, tailLength, size - tailLength);
 let at = -1;
 for (let i = tail.length - 22; i >= 0; i--) if (tail.readUInt32LE(i) === EOCD && i + 22 + tail.readUInt16LE(i + 20) === tail.length) { at = i; break; }
 if (at < 0) throw new Error('Not an artifact zip: no end of central directory record ends the file (trailing data?)');
 const end = size - tailLength + at;
 const disk = tail.readUInt16LE(at + 4), directoryDisk = tail.readUInt16LE(at + 6), onDisk = tail.readUInt16LE(at + 8), total = tail.readUInt16LE(at + 10);
 const directorySize = tail.readUInt32LE(at + 12), directoryOffset = tail.readUInt32LE(at + 16);
 if (disk !== 0 || directoryDisk !== 0 || onDisk !== total) throw new Error('Multi-disk artifact zips are refused');
 if (total === 0xffff || directorySize === 0xffffffff || directoryOffset === 0xffffffff) throw new Error('Zip64 artifact zips are refused');
 if (total > MAX_ENTRIES || directorySize > MAX_DIRECTORY) throw new Error('The artifact zip has too many entries');
 if (directoryOffset + directorySize !== end) throw new Error('The artifact zip central directory does not end at its end record');
 const directory = await readAt(handle, directorySize, directoryOffset);
 const entries = [];
 let p = 0;
 for (let i = 0; i < total; i++) {
  if (p + 46 > directory.length || directory.readUInt32LE(p) !== CENTRAL) throw new Error('Malformed artifact zip central directory');
  const madeBy = directory.readUInt16LE(p + 4), flags = directory.readUInt16LE(p + 8), method = directory.readUInt16LE(p + 10);
  const crc = directory.readUInt32LE(p + 16), compressed = directory.readUInt32LE(p + 20), uncompressed = directory.readUInt32LE(p + 24);
  const nameLength = directory.readUInt16LE(p + 28), extraLength = directory.readUInt16LE(p + 30), commentLength = directory.readUInt16LE(p + 32);
  const startDisk = directory.readUInt16LE(p + 34), external = directory.readUInt32LE(p + 38), local = directory.readUInt32LE(p + 42);
  if (p + 46 + nameLength + extraLength + commentLength > directory.length) throw new Error('Malformed artifact zip central directory');
  const raw = Buffer.from(directory.subarray(p + 46, p + 46 + nameLength));
  p += 46 + nameLength + extraLength + commentLength;
  const name = entryName(raw);
  if (flags & 0x2041) throw new Error('Encrypted artifact zip entries are refused (flags): ' + name);
  if (method !== 0 && method !== 8) throw new Error('Unsupported artifact zip compression method: ' + name);
  if (startDisk !== 0) throw new Error('Multi-disk artifact zips are refused');
  if (compressed === 0xffffffff || uncompressed === 0xffffffff || local === 0xffffffff) throw new Error('Zip64 artifact zip entries are refused: ' + name);
  // Unix hosts keep the mode in the high half: only a plain file (or no type at all) is accepted; never a DOS directory.
  const type = (external >>> 16) & 0o170000;
  if ((madeBy >> 8 === 3 && type !== 0 && type !== 0o100000) || (external & 0x10)) throw new Error('Artifact zip entries must be regular files: ' + name);
  if ((method === 0 && compressed !== uncompressed) || (method === 8 && compressed === 0)) throw new Error('Inconsistent artifact zip entry size: ' + name);
  entries.push({ name, raw, flags, method, crc, compressed, uncompressed, local });
 }
 if (p !== directory.length) throw new Error('Malformed artifact zip central directory');

 const seen = new Set();
 for (const entry of entries) {
  if (seen.has(entry.name)) throw new Error('Duplicate artifact zip entry: ' + entry.name);
  if (!names.includes(entry.name)) throw new Error('Unexpected artifact zip entry: ' + entry.name);
  seen.add(entry.name);
 }
 for (const name of names) if (!seen.has(name)) throw new Error('Missing artifact zip entry: ' + name);
 let sum = 0;
 for (const entry of entries) {
  if (entry.uncompressed > maxEntry) throw new Error('Artifact zip entry is too large: ' + entry.name);
  sum += entry.uncompressed;
  if (sum > maxTotal) throw new Error('The artifact zip is too large in total');
 }

 // Local headers: the same name and method, data inside the archive, no two entries sharing bytes.
 let previous = 0;
 for (const entry of [...entries].sort((a, b) => a.local - b.local)) {
  if (entry.local < previous) throw new Error('Artifact zip entries overlap: ' + entry.name);
  if (entry.local + 30 > directoryOffset) throw new Error('Artifact zip local header outside the archive: ' + entry.name);
  const header = await readAt(handle, 30, entry.local);
  if (header.readUInt32LE(0) !== LOCAL) throw new Error('Malformed artifact zip local header: ' + entry.name);
  const nameLength = header.readUInt16LE(26), extraLength = header.readUInt16LE(28);
  if (header.readUInt16LE(8) !== entry.method || entry.local + 30 + nameLength > directoryOffset
   || !(await readAt(handle, nameLength, entry.local + 30)).equals(entry.raw)) throw new Error('The artifact zip local header names another file or method than its directory: ' + entry.name);
  entry.start = entry.local + 30 + nameLength + extraLength;
  previous = entry.start + entry.compressed;
  if (previous > directoryOffset) throw new Error('Artifact zip entry data outside the archive: ' + entry.name);
 }
 return new Map(entries.map(entry => [entry.name, entry]));
}

/**
 * Writes exactly `names` from the zip at `file` into `directory`, each with an
 * exclusive create, after validating the whole archive. Each entry's size and
 * CRC-32 are checked as it is inflated, and inflation stops at the declared size.
 */
export async function extractArtifactZip({ file, names, directory, maxEntry = ZIP_LIMITS.maxEntry, maxTotal = ZIP_LIMITS.maxTotal }) {
 if (!Array.isArray(names) || !names.length) throw new Error('No expected artifact entries');
 for (const name of names) entryName(Buffer.from(name));
 const handle = await open(file, 'r');
 let entries;
 try { entries = await centralDirectory(handle, names, { maxEntry, maxTotal }); } finally { await handle.close(); }
 for (const name of names) {
  const entry = entries.get(name);
  let written = 0, crc = 0;
  const meter = new Transform({
   transform(chunk, _encoding, callback) {
    written += chunk.length;
    if (written > entry.uncompressed) return callback(new Error('Artifact zip entry is larger than its declared size: ' + name));
    crc = crc32(chunk, crc); callback(null, chunk);
   },
  });
  const source = entry.compressed === 0 ? Readable.from([]) : createReadStream(file, { start: entry.start, end: entry.start + entry.compressed - 1 });
  // The output name is the caller's, from the fixed list; the zip's bytes only had to equal it.
  const sink = createWriteStream(join(directory, name), { flags: 'wx', mode: 0o600 });
  await pipeline(...(entry.method === 8 ? [source, createInflateRaw(), meter, sink] : [source, meter, sink]));
  if (written !== entry.uncompressed) throw new Error('Artifact zip entry does not have its declared size: ' + name);
  if ((crc >>> 0) !== entry.crc) throw new Error('Artifact zip entry CRC mismatch: ' + name);
 }
 return names;
}
