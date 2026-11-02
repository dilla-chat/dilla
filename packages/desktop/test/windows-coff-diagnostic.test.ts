import { test } from 'node:test';
import assert from 'node:assert/strict';
// @ts-expect-error build tooling is JavaScript
import { describeCoffResource } from '../scripts/windows-coff-diagnostic.mjs';

test('reports negative COFF symbols and their relocation references without exposing arbitrary names', () => {
 // One section, two symbols and one relocation targeting the first symbol.
 const b=Buffer.alloc(20+40+10+36+4); b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);b.writeUInt32LE(70,8);b.writeUInt32LE(2,12);
 b.write('.rsrc',20);b.writeUInt32LE(60,44);b.writeUInt16LE(1,52);b.writeUInt32LE(0,64);b.writeUInt16LE(3,68);
 b.write('@comp.id',70);b.writeInt16LE(-1,82);b[86]=3;
 b.write('bad/path',88);b.writeInt16LE(-1,100);b[104]=3;b.writeUInt32LE(4,106);
 assert.deepEqual(describeCoffResource(b),{machine:'0x8664',sections:[{name:'.rsrc',relocations:1}],negativeSymbols:[{index:0,name:'@comp.id',section:-1,storageClass:3,relocationReferences:1},{index:1,name:'[redacted]',section:-1,storageClass:3,relocationReferences:0}],truncated:false,header:{fileLength:110,machine:'0x8664',sectionCount:1,symbolTableOffset:70,symbolCount:2,optionalHeaderLength:0,stringTableOffset:106,stringTableLength:4}});
});

test('rejects truncated or unsupported resource objects without throwing', () => {
 assert.equal(describeCoffResource(Buffer.alloc(19)).error,'invalid COFF header');
 const b=Buffer.alloc(20);b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);
 assert.equal(describeCoffResource(b).error,'invalid COFF section table');
});

test('Go-compatible empty string tables preserve inline negative-symbol evidence', () => {
 for (const length of [0,1,2,3,4]) {
  const b=Buffer.alloc(20+40+18+4);b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);b.writeUInt32LE(60,8);b.writeUInt32LE(1,12);
  b.write('.rsrc',20);b.write('@comp.id',60);b.writeInt16LE(-1,72);b[76]=3;b.writeUInt32LE(length,78);
  const out=describeCoffResource(b);
  assert.equal(out.negativeSymbols[0].name,'@comp.id');assert.equal(out.header.stringTableLength,length);assert.equal(out.header.stringTableOffset,78);assert.equal(out.header.fileLength,82);
 }
});

test('invalid string table retains bounded header and offset metadata', () => {
 const b=Buffer.alloc(82);b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);b.writeUInt32LE(60,8);b.writeUInt32LE(1,12);b.writeUInt32LE(99,78);
 const out=describeCoffResource(b);assert.equal(out.error,'invalid COFF string table');
 assert.deepEqual(out.header,{fileLength:82,machine:'0x8664',sectionCount:1,symbolTableOffset:60,symbolCount:1,optionalHeaderLength:0,stringTableOffset:78,stringTableLength:99});
});

test('a symbol-free COFF object has no string table', () => {
 const b=Buffer.alloc(60);b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);b.write('.rsrc',20);
 const out=describeCoffResource(b);assert.deepEqual(out.negativeSymbols,[]);assert.equal(out.header.stringTableOffset,null);assert.equal(out.header.stringTableLength,null);
});

test('link diagnostic observer cannot replace original error, even on invalid bytes or observer failure', async () => {
 const { mkdtemp, writeFile, rm } = await import('node:fs/promises');
 const { tmpdir } = await import('node:os'); const {join} = await import('node:path');
 // @ts-expect-error build tooling is JavaScript
 const { reportCoffLinkFailure } = await import('../scripts/windows-package.mjs');
 const dir=await mkdtemp(join(tmpdir(),'dilla-coff-test-'));const file=join(dir,'resource.syso');const original=Error('original Go failure');
 try {
  const bytes=Buffer.from('bad object');await writeFile(file,bytes);let seen;
  await assert.rejects(reportCoffLinkFailure(original,file,(value:Buffer)=>{seen=Buffer.from(value);value.fill(0);throw Error('observer failure');}),(error:unknown)=>error===original);
  assert.deepEqual(seen,bytes);
  await assert.rejects(reportCoffLinkFailure(original,join(dir,'missing')),(error:unknown)=>error===original);
 } finally { await rm(dir,{recursive:true,force:true}); }
});
