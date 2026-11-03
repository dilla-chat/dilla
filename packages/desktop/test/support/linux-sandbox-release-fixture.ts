import { createHash, generateKeyPairSync, sign } from 'node:crypto';
export function sandboxReleaseFixture(dates: { publishedAt?: string; expiresAt?: string } = {}) {
 const hash=(b:Buffer)=>createHash('sha512').update(b).digest('base64');
 function elf(tag:number) { const b=Buffer.alloc(129); Buffer.from([127,69,76,70,2,1,1]).copy(b); b.writeUInt16LE(2,16); b.writeUInt16LE(62,18); b.writeUInt32LE(1,20); b.writeBigUInt64LE(0x400080n,24); b.writeBigUInt64LE(64n,32); b.writeUInt16LE(64,52); b.writeUInt16LE(56,54); b.writeUInt16LE(1,56); b.writeUInt32LE(1,64); b.writeUInt32LE(5,68); b.writeBigUInt64LE(0x400000n,80); b.writeBigUInt64LE(0x400000n,88); b.writeBigUInt64LE(129n,96); b.writeBigUInt64LE(129n,104); b.writeBigUInt64LE(4096n,112); b[128]=tag; return b; }
 const pair=generateKeyPairSync('ed25519'), maintainer=elf(1), helper=elf(2), publicKey=pair.publicKey.export({format:'der',type:'spki'}).subarray(-32).toString('base64');
 const publishedAt=dates.publishedAt??'2026-11-01T00:00:00Z', expiresAt=dates.expiresAt??'2027-01-01T00:00:00Z', generation=1;
 const config={goVersion:'go1.27.3',generation,publishedAt,expiresAt,publicKeys:[publicKey],helper:{version:'fixture',size:helper.length,sha256:createHash('sha256').update(helper).digest('hex'),sha512:hash(helper)}};
 const manifest=Buffer.from(JSON.stringify({format:1,kind:'dilla-linux-sandbox',generation,platform:'linux',arch:'x64',compatibilityEpoch:1,sandboxApi:1,publishedAt,expiresAt,maintainer:{size:maintainer.length,sha512:hash(maintainer)},helper:{size:helper.length,sha512:hash(helper)}}));
 const header=Buffer.alloc(32); header.write('DLSBOX1\0'); header.writeUInt32LE(manifest.length,8); header.writeBigUInt64LE(BigInt(maintainer.length),16); header.writeBigUInt64LE(BigInt(helper.length),24);
 const capsule=Buffer.concat([header,manifest,maintainer,helper]), signature=Buffer.from(sign(null,Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),manifest]),pair.privateKey).toString('base64')+'\n');
 return {pair,config,manifest,capsule,signature,hash};
}

/** Synthetic signed-feed records for tests which do not exercise sidecar bytes. */
export function sandboxAssetRecords(version:string) {
 const base=`dilla-${version}-linux-x64.sandbox`, sha512=Buffer.alloc(64).toString('base64');
 return [{name:base,size:100,sha512},{name:base+'.json',size:20,sha512},{name:base+'.sig',size:89,sha512}];
}
