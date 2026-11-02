import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
// @ts-expect-error build tooling is JavaScript
import { normalizeWindowsCoffResource } from '../scripts/windows-coff-compat.mjs';
const original=await readFile(new URL('./fixtures/windows-coff/msvc-cvtres-smoke.syso.fixture',import.meta.url));

test('actual Microsoft cvtres object removes only unreferenced comp.id while preserving resources and relocation targets',()=>{
 const result=normalizeWindowsCoffResource(original),offset=original.readUInt32LE(8),count=original.readUInt32LE(12);
 assert.equal(count,18);assert.equal(result.readUInt32LE(12),17);
 const expected=Buffer.concat([original.subarray(0,offset),original.subarray(offset+18)]);expected.writeUInt32LE(17,12);
 for(let section=0;section<original.readUInt16LE(2);section++) {
  const p=20+section*40,rel=original.readUInt32LE(p+24),n=original.readUInt16LE(p+32);
  const raw=original.readUInt32LE(p+20),size=original.readUInt32LE(p+16);
  assert.deepEqual(result.subarray(raw,raw+size),original.subarray(raw,raw+size));
  for(let i=0;i<n;i++){
   const at=rel+i*10+4,index=original.readUInt32LE(at),newIndex=result.readUInt32LE(at);
   assert.deepEqual(result.subarray(offset+newIndex*18,offset+(newIndex+1)*18),original.subarray(offset+index*18,offset+(index+1)*18));
   expected.writeUInt32LE(index-1,at);
  }
 }
 assert.deepEqual(result,expected);
 assert.deepEqual(normalizeWindowsCoffResource(result),result);
 assert.ok(original.includes(Buffer.from('DillaSmoke','utf16le')));
 assert.ok(!original.includes(Buffer.from('PRIVATE KEY')) && !original.includes(Buffer.from('bootstrapConfig')));
});

test('unknown, referenced, malformed or ambiguous COFF symbols are rejected',()=>{
 const sym=original.readUInt32LE(8),rel=original.readUInt32LE(20+40+24);
 for(const mutate of [
  (b:Buffer)=>b.write('@otherXX',sym),
  (b:Buffer)=>{b[sym+16]=2;},
  (b:Buffer)=>b.write('@comp.id',sym+18),
  (b:Buffer)=>b.writeUInt32LE(0,rel+4),
  (b:Buffer)=>{b[sym+17]=1;},
  (b:Buffer)=>b.writeUInt32LE(999999,8),
  (b:Buffer)=>b.writeInt16LE(-1,sym+8*18+12),
  (b:Buffer)=>b.writeUInt16LE(0xffff,20+40+32),
  (b:Buffer)=>b.writeUInt32LE(b.readUInt32LE(20+20),20+40+20),
  (b:Buffer)=>b.writeUInt32LE(123,20+28),
  (b:Buffer)=>b.writeUInt32LE(b.readUInt32LE(20+36)|0x01000000,20+36),
  (b:Buffer)=>{b[sym+2*18+17]=2;},
 ]) {const bad=Buffer.from(original);mutate(bad);assert.throws(()=>normalizeWindowsCoffResource(bad));}
});

test('non-ASCII aliases of known absolute symbols are rejected byte for byte',()=>{
 const sym=original.readUInt32LE(8);
 for(const index of [0,1]) for(let byte=0;byte<8;byte++){
  const bad=Buffer.from(original);bad[sym+index*18+byte]|=0x80;
  assert.throws(()=>normalizeWindowsCoffResource(bad),/Unsupported Windows launcher COFF resource/,`symbol ${index} byte ${byte}`);
 }
});
