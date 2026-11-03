// Fixture-only metadata: the caller supplies the same fixture key/config used
// to compile every capsule in the job. Never use production private keys.
import { createHash, sign } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { inspectLinuxSandboxCapsule, verifyLinuxSandboxRelease } from '../../src/linux-sandbox-release.ts';
export async function fixtureSandboxFeed(image, version, pair, configuration, now = Date.now()) {
 if (typeof image !== 'string' || !image.endsWith('.AppImage') || !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.test(version)) throw new Error('Invalid fixture target');
 const stem = image.slice(0,-'.AppImage'.length), capsule = await readFile(stem+'.sandbox'), manifest = await readFile(stem+'.sandbox.json');
 const {generation} = inspectLinuxSandboxCapsule({capsule,manifest,now,minimumGeneration:1});
 const publicKey=pair.publicKey.export({type:'spki',format:'der'}).subarray(-32).toString('base64');
 if (!Array.isArray(configuration.publicKeys) || configuration.publicKeys.length !== 1 || configuration.publicKeys[0] !== publicKey) throw new Error('Fixture signer differs from compiled fixture trust');
 const parsed=JSON.parse(manifest.toString('utf8')); if (generation !== configuration.generation || parsed.publishedAt !== configuration.publishedAt || parsed.expiresAt !== configuration.expiresAt || parsed.helper.size !== configuration.helper.size || parsed.helper.sha512 !== configuration.helper.sha512) throw new Error('Fixture component differs from compiled configuration');
 const signature=Buffer.from(sign(null,Buffer.concat([Buffer.from('Dilla Linux sandbox capsule v1\0'),manifest]),pair.privateKey).toString('base64')+'\n');
 verifyLinuxSandboxRelease({capsule,manifest,signature:signature.toString('utf8'),keys:configuration.publicKeys,now,minimumGeneration:generation});
 const base=`dilla-${version}-linux-x64.sandbox`, hash=bytes=>createHash('sha512').update(bytes).digest('base64');
 const assets=[[base,capsule],[base+'.json',manifest],[base+'.sig',signature]].map(([name,bytes])=>({name,size:bytes.length,sha512:hash(bytes)}));
 const linuxSandbox={compatibilityEpoch:1,minimumGeneration:generation,bundle:{url:`https://github.com/dilla-chat/dilla/releases/download/desktop-v${version}/${base}`,size:capsule.length,sha512:hash(capsule)}};
 return {linuxSandbox,assets,signature,fixturePublicKey:publicKey};
}
