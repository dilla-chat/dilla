import { crc32, deflateRawSync } from 'node:zlib';

/**
 * A minimal ZIP writer for tests: builds the archives upload-artifact produces
 * (stored or deflated entries, Unix "version made by", optional data
 * descriptors) and, through per-entry overrides, the malformed ones the
 * artifact reader must refuse. Test support only; nothing in a release job
 * imports it.
 *
 * Entry: { name, data, method = 8, mode = 0o100644, externalAttributes,
 *   versionMadeBy = 0x0314, flags = 0, size, crc, localName, descriptor = false }
 */
export function zip(entries, { comment = Buffer.alloc(0), trailing = Buffer.alloc(0), eocd = {} } = {}) {
 const locals = [], centrals = [];
 let offset = 0;
 for (const entry of entries) {
  const data = Buffer.from(entry.data ?? '');
  const method = entry.method ?? 8;
  const body = method === 8 ? deflateRawSync(data) : data;
  const name = Buffer.from(entry.name), localName = Buffer.from(entry.localName ?? entry.name);
  const crc = entry.crc ?? crc32(data), size = entry.size ?? data.length;
  const flags = (entry.flags ?? 0) | (entry.descriptor ? 0x8 : 0) | 0x800;
  const external = entry.externalAttributes ?? (((entry.mode ?? 0o100644) << 16) >>> 0);
  const local = Buffer.alloc(30);
  local.writeUInt32LE(0x04034b50, 0); local.writeUInt16LE(20, 4); local.writeUInt16LE(flags, 6); local.writeUInt16LE(method, 8);
  local.writeUInt16LE(0, 10); local.writeUInt16LE(0x21, 12);
  local.writeUInt32LE(entry.descriptor ? 0 : crc, 14); local.writeUInt32LE(entry.descriptor ? 0 : body.length, 18); local.writeUInt32LE(entry.descriptor ? 0 : size, 22);
  local.writeUInt16LE(localName.length, 26); local.writeUInt16LE(0, 28);
  const descriptor = Buffer.alloc(entry.descriptor ? 16 : 0);
  if (entry.descriptor) { descriptor.writeUInt32LE(0x08074b50, 0); descriptor.writeUInt32LE(crc, 4); descriptor.writeUInt32LE(body.length, 8); descriptor.writeUInt32LE(size, 12); }
  locals.push(local, localName, body, descriptor);
  const central = Buffer.alloc(46);
  central.writeUInt32LE(0x02014b50, 0); central.writeUInt16LE(entry.versionMadeBy ?? 0x0314, 4); central.writeUInt16LE(20, 6); central.writeUInt16LE(flags, 8);
  central.writeUInt16LE(method, 10); central.writeUInt16LE(0, 12); central.writeUInt16LE(0x21, 14);
  central.writeUInt32LE(crc, 16); central.writeUInt32LE(entry.compressedSize ?? body.length, 20); central.writeUInt32LE(size, 24);
  central.writeUInt16LE(name.length, 28); central.writeUInt16LE(0, 30); central.writeUInt16LE(0, 32); central.writeUInt16LE(0, 34); central.writeUInt16LE(0, 36);
  central.writeUInt32LE(external, 38); central.writeUInt32LE(entry.offset ?? offset, 42);
  centrals.push(central, name);
  offset += local.length + localName.length + body.length + descriptor.length;
 }
 const directory = Buffer.concat(centrals);
 const end = Buffer.alloc(22);
 end.writeUInt32LE(0x06054b50, 0); end.writeUInt16LE(eocd.disk ?? 0, 4); end.writeUInt16LE(0, 6);
 end.writeUInt16LE(eocd.entries ?? entries.length, 8); end.writeUInt16LE(eocd.entries ?? entries.length, 10);
 end.writeUInt32LE(directory.length, 12); end.writeUInt32LE(eocd.directoryOffset ?? offset, 16); end.writeUInt16LE(comment.length, 20);
 return Buffer.concat([...locals, directory, end, comment, trailing]);
}
