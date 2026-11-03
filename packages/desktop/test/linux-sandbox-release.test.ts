import { test } from 'node:test';
import { strict as assert } from 'node:assert';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { deflateSync } from 'node:zlib';
import { verifyLinuxSandboxRelease } from '../src/linux-sandbox-release.ts';

const now = Date.parse('2026-11-02T12:00:00Z');
const hash = (b: Buffer) => createHash('sha512').update(b).digest('base64');
function elf(tag: number) {
 const b = Buffer.alloc(129); Buffer.from([127,69,76,70,2,1,1]).copy(b); b.writeUInt16LE(2,16); b.writeUInt16LE(62,18); b.writeUInt32LE(1,20); b.writeBigUInt64LE(0x400080n,24); b.writeBigUInt64LE(64n,32); b.writeUInt16LE(64,52); b.writeUInt16LE(56,54); b.writeUInt16LE(1,56); b.writeUInt32LE(1,64); b.writeUInt32LE(5,68); b.writeBigUInt64LE(0x400000n,80); b.writeBigUInt64LE(0x400000n,88); b.writeBigUInt64LE(129n,96); b.writeBigUInt64LE(129n,104); b.writeBigUInt64LE(4096n,112); b[128]=tag; return b;
}
function fixture(changes: Record<string, unknown> = {}, rewrite: (json: string) => string = json => json, maintainer: Buffer = elf(1)) {
 const {publicKey,privateKey}=generateKeyPairSync('ed25519'), helper=elf(2);
 const m={format:1,kind:'dilla-linux-sandbox',generation:3,platform:'linux',arch:'x64',compatibilityEpoch:1,sandboxApi:1,publishedAt:'2026-11-02T11:00:00Z',expiresAt:'2026-11-03T12:00:00Z',maintainer:{size:maintainer.length,sha512:hash(maintainer)},helper:{size:helper.length,sha512:hash(helper)},...changes};
 const manifest=Buffer.from(rewrite(JSON.stringify(m))), header=Buffer.alloc(32); header.write('DLSBOX1\0'); header.writeUInt32LE(manifest.length,8); header.writeBigUInt64LE(BigInt(maintainer.length),16); header.writeBigUInt64LE(BigInt(helper.length),24);
 const capsule=Buffer.concat([header,manifest,maintainer,helper]); const signature=sign(null,Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),manifest]),privateKey).toString('base64')+'\n';
 const wrongPurpose=sign(null,manifest,privateKey).toString('base64'); const keys=[publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64')]; return {capsule,manifest,signature,keys,now,minimumGeneration:3,wrongPurpose};
}
test('authenticates Go-compatible unsigned capsule with a dedicated detached signature',()=>{ assert.equal(verifyLinuxSandboxRelease(fixture()).generation,3); });
test('rejects wrong purpose, mutations, expiry and generation rollback',()=>{
 const f=fixture(); assert.throws(()=>verifyLinuxSandboxRelease({...f,signature:f.wrongPurpose}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,manifest:Buffer.concat([f.manifest,Buffer.from(' ')] )}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,capsule:Buffer.concat([f.capsule,Buffer.from([0])])}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,now:Date.parse('2026-11-04T00:00:00Z')}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,minimumGeneration:4}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,keys:fixture().keys}));
 assert.throws(()=>verifyLinuxSandboxRelease({...f,signature:f.signature.trim() + ' '}));
 const mutated = Buffer.from(f.capsule); mutated[mutated.length - 1] ^= 1; assert.throws(()=>verifyLinuxSandboxRelease({...f,capsule:mutated}));
 const signedFrame = Buffer.from(f.capsule); signedFrame.writeUInt32LE(64,12); assert.throws(()=>verifyLinuxSandboxRelease({...f,capsule:signedFrame}));
});
test('rejects duplicate keys and noncanonical numbers despite valid signatures',()=>{
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({}, json => json.replace('\"generation\":3', '\"generation\":3,\"generation\":3'))));
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({}, json => json.replace('\"generation\":3', '\"generation\":3e0'))));
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({publishedAt:'2026-02-30T11:00:00Z'})));
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({publishedAt:'2026-11-02T12:06:00Z'})));
});

test('matches resident Go ELF acceptance for section and program table edge cases', () => {
 const base=elf(1), cases: Array<[string,Buffer]> = [['minimal',base]];
 function change(name:string, edit:(b:Buffer)=>void, length=129) { const b=Buffer.alloc(length); base.copy(b); edit(b); cases.push([name,b]); }
 change('out of bounds section offset',b=>{b.writeBigUInt64LE(99999n,40); b.writeUInt16LE(64,58); b.writeUInt16LE(1,60);});
 change('count with zero section offset',b=>{b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);});
 change('short section entry',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(63,58);b.writeUInt16LE(1,60);},193);
 change('truncated section table',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(2,60);},256);
 change('valid section table',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);},193);
 change('section index outside count',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);b.writeUInt16LE(1,62);},193);
 change('extended count without table',b=>{b.writeBigUInt64LE(99999n,40);});
 change('extended count too small',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeBigUInt64LE(1n,129+32);},193);
 change('extended count with truncated table',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeBigUInt64LE(0xff00n,129+32);},193);
 change('extended phnum invalid',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);b.writeUInt16LE(0xffff,56);},193);
 change('truncated program table',b=>b.writeUInt16LE(2,56));
 change('larger program entries',b=>b.writeUInt16LE(64,54));
 change('negative signed section offset',b=>b.writeBigUInt64LE(0x8000000000000000n,40));
 const extendedProgramEnd=64+0xffff*56;
 change('valid extended program count',b=>{b.writeUInt16LE(0xffff,56);b.writeBigUInt64LE(BigInt(extendedProgramEnd),40);b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);b.writeUInt32LE(0xffff,extendedProgramEnd+44);},extendedProgramEnd+64);
 change('section string table wrong type',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(2,60);b.writeUInt16LE(1,62);},257);
 change('valid extended section count',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeBigUInt64LE(0xff00n,129+32);},129+0xff00*64);
 change('valid section names',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(2,60);b.writeUInt16LE(1,62);b.writeUInt32LE(1,193);b.writeUInt32LE(3,197);b.writeBigUInt64LE(257n,217);b.writeBigUInt64LE(5n,225);Buffer.from('\0foo\0').copy(b,257);},262);
 change('section name without terminator',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(2,60);b.writeUInt16LE(1,62);b.writeUInt32LE(1,193);b.writeUInt32LE(3,197);b.writeBigUInt64LE(257n,217);b.writeBigUInt64LE(4n,225);Buffer.from('\0foo').copy(b,257);},261);
 const compressed=deflateSync(Buffer.from('\0foo\0'));
 change('valid compressed section names',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(2,60);b.writeUInt16LE(1,62);b.writeUInt32LE(1,193);b.writeUInt32LE(3,197);b.writeBigUInt64LE(0x800n,201);b.writeBigUInt64LE(257n,217);b.writeBigUInt64LE(BigInt(24+compressed.length),225);b.writeUInt32LE(1,257);b.writeBigUInt64LE(5n,265);compressed.copy(b,281);},281+compressed.length);
 change('truncated compressed section header',b=>{b.writeBigUInt64LE(129n,40);b.writeUInt16LE(64,58);b.writeUInt16LE(1,60);b.writeBigUInt64LE(0x800n,137);b.writeBigUInt64LE(193n,153);b.writeBigUInt64LE(12n,161);},205);
 const oracle=spawnSync('go',['run','./test/support/linux-sandbox-elf-oracle'],{cwd: new URL('..',import.meta.url),input:JSON.stringify(cases.map(([,bytes])=>bytes.toString('base64'))),encoding:'utf8',timeout:120000,maxBuffer:1048576});
 assert.ifError(oracle.error); assert.equal(oracle.status,0,oracle.stderr); const expected=JSON.parse(oracle.stdout) as boolean[];
 for (const [i,[name,bytes]] of cases.entries()) { let actual=true; try {verifyLinuxSandboxRelease(fixture({}, x=>x, bytes));} catch {actual=false;} assert.equal(actual,expected[i],name); }
});

test('freshness uses nanosecond precision at skew, expiry and 93-day boundaries',()=>{
 const accepted=fixture({publishedAt:'2026-11-02T12:05:00Z',expiresAt:'2027-02-03T12:05:00Z'});
 assert.equal(verifyLinuxSandboxRelease(accepted).generation,3);
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({publishedAt:'2026-11-02T12:05:00.000000001Z',expiresAt:'2026-11-03T12:00:00Z'})));
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({publishedAt:'2026-11-02T12:05:00Z',expiresAt:'2027-02-03T12:05:00.000000001Z'})));
 assert.throws(()=>verifyLinuxSandboxRelease(fixture({publishedAt:'2026-11-02T11:00:00Z',expiresAt:'2026-11-02T12:00:00Z'})));
});

test('sandbox capsule signed by the active key verifies under an old launcher list and under the transition list, and a staged-only capsule verifies only under the transition list',()=>{
 const base=fixture(), stagedPair=generateKeyPairSync('ed25519'), stagedKey=stagedPair.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64');
 const oldList=base.keys, transitionList=[...base.keys,stagedKey];
 assert.equal(verifyLinuxSandboxRelease({...base,keys:oldList}).generation,3);
 assert.equal(verifyLinuxSandboxRelease({...base,keys:transitionList}).generation,3);
 const message=Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),base.manifest]);
 const stagedSignature=sign(null,message,stagedPair.privateKey).toString('base64')+'\n';
 assert.equal(verifyLinuxSandboxRelease({...base,signature:stagedSignature,keys:transitionList}).generation,3);
 assert.throws(()=>verifyLinuxSandboxRelease({...base,signature:stagedSignature,keys:oldList}));
 const stranger=sign(null,message,generateKeyPairSync('ed25519').privateKey).toString('base64')+'\n';
 assert.throws(()=>verifyLinuxSandboxRelease({...base,signature:stranger,keys:transitionList}));
});
