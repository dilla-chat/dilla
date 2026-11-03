import { createHash, createPublicKey, verify } from 'node:crypto';
import { inflateSync, zstdDecompressSync } from 'node:zlib';

const MAX_MANIFEST = 65536, MAX_MAINTAINER = 32 * 1024 ** 2, MAX_HELPER = 8 * 1024 ** 2, MAX_CAPSULE = 48 * 1024 ** 2;
const domain = Buffer.from('Dilla Linux sandbox capsule v1\0');
const fail = (): never => { throw new Error('Invalid Linux sandbox release'); };
const digest = (bytes: Uint8Array) => createHash('sha512').update(bytes).digest('base64');
function base64(value: unknown, size: number): value is string {
 return typeof value === 'string' && Buffer.from(value, 'base64').length === size && Buffer.from(value, 'base64').toString('base64') === value;
}
function strictJSON(raw: Buffer): any {
 const text = new TextDecoder('utf-8', { fatal: true }).decode(raw), parsed = JSON.parse(text); let i = 0;
 const ws = () => { while (i < text.length && /[ \t\r\n]/.test(text[i])) i++; };
 const str = () => { const start = i++; while (i < text.length) { const c = text[i++]; if (c === '\\') i++; else if (c === '"') return JSON.parse(text.slice(start, i)) as string; } return fail(); };
 const value = (depth: number): void => {
  if (depth > 16) fail(); ws();
  if (text[i] === '{') { i++; ws(); const seen = new Set<string>(); if (text[i] === '}') { i++; return; } for (;;) { ws(); if (text[i] !== '"') fail(); const key = str(); if (seen.has(key)) fail(); seen.add(key); ws(); if (text[i++] !== ':') fail(); value(depth + 1); ws(); const c = text[i++]; if (c === '}') return; if (c !== ',') fail(); } }
  if (text[i] === '[') { i++; ws(); if (text[i] === ']') { i++; return; } for (;;) { value(depth + 1); ws(); const c = text[i++]; if (c === ']') return; if (c !== ',') fail(); } }
  if (text[i] === '"') { str(); return; }
  const start = i; while (i < text.length && !/[ \t\r\n,\]}]/.test(text[i])) i++; const token = text.slice(start, i);
  if (!['true', 'false', 'null'].includes(token) && (!/^(0|[1-9][0-9]*)$/.test(token) || !Number.isSafeInteger(Number(token)))) fail();
 };
 value(0); ws(); if (i !== text.length) fail(); return parsed;
}
function exact(value: any, keys: string[]): boolean { return !!value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === keys.length && keys.every(k => Object.hasOwn(value,k)); }
function validELF(b: Buffer): boolean {
 if (b.length < 64 || !b.subarray(0,7).equals(Buffer.from([127,69,76,70,2,1,1])) || ![2,3].includes(b.readUInt16LE(16)) || b.readUInt16LE(18) !== 62 || b.readUInt32LE(20) !== 1 || b.readUInt16LE(52) !== 64) return false;
 const off = b.readBigUInt64LE(32), size = b.readUInt16LE(54), entry = b.readBigUInt64LE(24);
 const shoff = b.readBigUInt64LE(40), shsize = b.readUInt16LE(58); let shcount = b.readUInt16LE(60), namesIndex = b.readUInt16LE(62), count = b.readUInt16LE(56);
 const maxInt64 = 0x7fffffffffffffffn, bounded = (start: bigint, length: bigint) => start <= BigInt(b.length) && length <= BigInt(b.length) - start;
 if (shoff > maxInt64 || off > maxInt64 || (shoff === 0n && shcount !== 0) || (shcount > 0 && namesIndex >= shcount) || (count > 0 && size < 56)) return false;
 if (shoff > 0n && (shcount === 0 || count === 0xffff)) {
  if (!bounded(shoff,64n)) return false; const first = Number(shoff), total = b.readBigUInt64LE(first+32), type = b.readUInt32LE(first+4), link = b.readUInt32LE(first+40), info = b.readUInt32LE(first+44);
  if (type !== 0) return false;
  if (shcount === 0) { if (total < 0xff00n || total > BigInt(Number.MAX_SAFE_INTEGER)) return false; shcount = Number(total); }
  if (count === 0xffff) { if (info < 0xffff) return false; count = info; }
  if (namesIndex === 0xffff) { if (link < 0xff00 || link >= shcount) return false; namesIndex = link; }
 }
 if (!bounded(off, BigInt(size)*BigInt(count)) || (shcount > 0 && shsize < 64) || !bounded(shoff,BigInt(shsize)*BigInt(shcount))) return false;
 // debug/elf parses every section and may read its compression header even
 // when the section is not otherwise used. Only the name table's contents
 // are read; bound decompression so untrusted metadata cannot exhaust memory.
 let namesTable: {type:number; flags:bigint; offset:bigint; length:bigint; decoded:bigint; compression:number} | undefined;
 for (let i=0; i<shcount; i++) {
  const p=Number(shoff)+i*shsize, offset=b.readBigUInt64LE(p+24), length=b.readBigUInt64LE(p+32), flags=b.readBigUInt64LE(p+8);
  if (offset > maxInt64 || length > maxInt64) return false;
  let decoded=length, compression=0;
  if ((flags & 0x800n) !== 0n) { if (length < 24n || !bounded(offset,24n)) return false; compression=b.readUInt32LE(Number(offset)); decoded=b.readBigUInt64LE(Number(offset)+8); }
  if (i === namesIndex) namesTable={type:b.readUInt32LE(p+4),flags,offset,length,decoded,compression};
 }
 if (shcount > 0 && namesIndex !== 0) {
  const table=namesTable; if (!table || table.type !== 3 || table.decoded > BigInt(MAX_CAPSULE) || !bounded(table.offset,table.length)) return false;
  let bytes=b.subarray(Number(table.offset),Number(table.offset+table.length));
  if ((table.flags & 0x800n) !== 0n) {
   if ((table.flags & 2n) !== 0n) return false;
   try { const payload=bytes.subarray(24); bytes=table.compression===1 ? inflateSync(payload,{maxOutputLength:MAX_CAPSULE}) : table.compression===2 ? zstdDecompressSync(payload,{maxOutputLength:MAX_CAPSULE}) : Buffer.alloc(0); } catch { return false; }
  }
  if (BigInt(bytes.length) < table.decoded) return false; bytes=bytes.subarray(0,Number(table.decoded));
  const lastNull=bytes.lastIndexOf(0);
  for (let i=0; i<shcount; i++) { const name=b.readUInt32LE(Number(shoff)+i*shsize); if (name > lastNull) return false; }
 }

 let executableEntry = false;
 for (let i = 0; i < count; i++) { const p = Number(off) + i * size, start = b.readBigUInt64LE(p+8), addr = b.readBigUInt64LE(p+16), filesz = b.readBigUInt64LE(p+32), memsz = b.readBigUInt64LE(p+40);
  if (start + filesz > BigInt(b.length) || memsz < filesz) return false;
  if (b.readUInt32LE(p) === 1 && (b.readUInt32LE(p+4) & 1) && filesz > 0n && entry >= addr && entry - addr < memsz) executableEntry = true;
 } return executableEntry;
}
export interface LinuxSandboxReleaseInput { capsule: Uint8Array; manifest: Uint8Array; signature: string; keys: readonly string[]; now: number; minimumGeneration: number }
/** Verifies release-side bytes only; this does not confer privileged authority. */
export function inspectLinuxSandboxCapsule(input: Omit<LinuxSandboxReleaseInput, 'signature' | 'keys'>): { generation: number; compatibilityEpoch: 1 } {
 const c = Buffer.from(input.capsule), external = Buffer.from(input.manifest);
 if (c.length < 32 || c.length > MAX_CAPSULE || !c.subarray(0,8).equals(Buffer.from('DLSBOX1\0'))) fail();
 const m = c.readUInt32LE(8), s = c.readUInt32LE(12), n = c.readBigUInt64LE(16), h = c.readBigUInt64LE(24);
 if (!m || m > MAX_MANIFEST || s !== 0 || n < 1n || n > BigInt(MAX_MAINTAINER) || h < 1n || h > BigInt(MAX_HELPER) || 32n+BigInt(m)+n+h !== BigInt(c.length)) fail();
 const manifest = c.subarray(32,32+m), maintainer = c.subarray(32+m,32+m+Number(n)), helper = c.subarray(32+m+Number(n));
 if (!manifest.equals(external)) fail();
 const v = strictJSON(manifest);
 if (!exact(v,['format','kind','generation','platform','arch','compatibilityEpoch','sandboxApi','publishedAt','expiresAt','maintainer','helper']) || v.format !== 1 || v.kind !== 'dilla-linux-sandbox' || v.platform !== 'linux' || v.arch !== 'x64' || v.compatibilityEpoch !== 1 || v.sandboxApi !== 1 || !Number.isSafeInteger(v.generation) || v.generation < 1 || !Number.isSafeInteger(input.minimumGeneration) || input.minimumGeneration < 1 || v.generation < input.minimumGeneration) fail();
 for (const [part, bytes] of [[v.maintainer,maintainer],[v.helper,helper]] as const) if (!exact(part,['size','sha512']) || part.size !== bytes.length || !base64(part.sha512,64) || digest(bytes) !== part.sha512 || !validELF(bytes)) fail();
 const utc = /^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z$/;
 const nanos = (value: unknown): bigint => {
  if (typeof value !== 'string') return fail(); const match = utc.exec(value); if (!match) return fail();
  const ms = Date.parse(match[1] + 'Z'); if (!Number.isFinite(ms) || new Date(ms).toISOString().slice(0,19) !== match[1]) return fail();
  return BigInt(ms) * 1000000n + BigInt((match[2] ?? '').padEnd(9,'0') || '0');
 };
 const published = nanos(v.publishedAt), expires = nanos(v.expiresAt);
 if (!Number.isSafeInteger(input.now)) fail(); const now = BigInt(input.now) * 1000000n;
 if (published > now + 300000000000n || expires <= now || expires <= published || expires-published > 93n*86400n*1000000000n) fail();
 return {generation:v.generation,compatibilityEpoch:1};
}
/** The inspection above is not authority. Verify only with installed dedicated keys. */
export function verifyLinuxSandboxRelease(input: LinuxSandboxReleaseInput): { generation: number; compatibilityEpoch: 1 } {
 const result = inspectLinuxSandboxCapsule(input);
 const manifest = Buffer.from(input.manifest);
 const sig = input.signature.endsWith('\n') ? input.signature.slice(0,-1) : input.signature;
 if (!base64(sig,64) || input.keys.length < 1 || input.keys.length > 8 || !input.keys.every(k => base64(k,32))) fail();
 const message = Buffer.concat([domain,manifest]);
 if (!input.keys.some(k => verify(null,message,createPublicKey({key:{kty:'OKP',crv:'Ed25519',x:Buffer.from(k,'base64').toString('base64url')},format:'jwk'}),Buffer.from(sig,'base64')))) fail();
 return result;
}
