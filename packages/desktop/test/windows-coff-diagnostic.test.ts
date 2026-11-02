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
 assert.deepEqual(describeCoffResource(b),{machine:'0x8664',sections:[{name:'.rsrc',relocations:1}],negativeSymbols:[{index:0,name:'@comp.id',section:-1,storageClass:3,relocationReferences:1},{index:1,name:'[redacted]',section:-1,storageClass:3,relocationReferences:0}],truncated:false});
});

test('rejects truncated or unsupported resource objects without throwing', () => {
 assert.equal(describeCoffResource(Buffer.alloc(19)).error,'invalid COFF header');
 const b=Buffer.alloc(20);b.writeUInt16LE(0x8664,0);b.writeUInt16LE(1,2);
 assert.equal(describeCoffResource(b).error,'invalid COFF section table');
});
