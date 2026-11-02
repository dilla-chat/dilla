// Content-free CI fixture only. No production root, key or runtime override.
import {readFileSync} from 'node:fs';
import {posix} from 'node:path';
const {isAbsolute,normalize}=posix;
export const MAINTENANCE_ROOT='/usr/lib/dilla-sandbox-maintenance-ci';
export const STAGING_PROGRAM=readFileSync(new URL('../fixtures/linux-sandbox-maintenance/root-stage.py',import.meta.url),'utf8');
const digest=x=>typeof x==='string'&&/^[A-Za-z0-9+/]{86}==$/.test(x)&&Buffer.from(x,'base64').toString('base64')===x;
export function maintenanceCommands({build,identity,hashes}){
 if(!isAbsolute(build)||normalize(build)!==build||!Array.isArray(identity)||identity.length!==3||!identity.slice(0,2).every(x=>typeof x==='string'&&/^[1-9][0-9]{0,19}$/.test(x))||!/^[0-9a-f]{40}$/.test(identity[2])||!Array.isArray(hashes)||hashes.length!==4||!hashes.every(digest))throw Error('Invalid maintenance inputs');
 const stage=role=>['/usr/bin/python3','-I','-S','-c',STAGING_PROGRAM,role,...identity];
 return [[...stage('init'),...hashes],stage('stage-native'),stage('stage-v1'),stage('stage-v2'),stage('stage-observer'),[...stage('verify'),...hashes],['/usr/bin/timeout','--signal=TERM','--kill-after=5s','300s',...stage('run')],['/usr/bin/timeout','--signal=TERM','--kill-after=5s','30s',...stage('cleanup')]];
}
export function verifyMaintenanceProof(stdout){
 const fail=()=>{throw Error('Invalid maintenance proof');};
 if(typeof stdout!=='string'||Buffer.byteLength(stdout)>65536)fail();
 const lines=stdout.split('\n').filter(x=>x.startsWith('DILLA_SANDBOX_MAINTENANCE_PROOF '));if(lines.length!==1)fail();
 const raw=lines[0].slice('DILLA_SANDBOX_MAINTENANCE_PROOF '.length);if(raw.length>4096||raw.includes('\\'))fail();
 let value;try{value=JSON.parse(raw);}catch{fail();}
 const expected={format:1,kind:'dilla-sandbox-maintenance-kernel',positive:0,demoted:65534,exchanged:65534,unlinked:65534,fresh:0,recoveries:6,mounts:2,lateInode:true,nativeUntouched:true,productionAbsent:true};
 if(!value||Array.isArray(value)||Object.keys(value).sort().join(',')!==Object.keys(expected).sort().join(','))fail();
 const keys=[...raw.matchAll(/"([^"\\]*)"\s*:/g)].map(x=>x[1]);if(keys.length!==Object.keys(expected).length||new Set(keys).size!==keys.length)fail();
 for(const key of Object.keys(expected))if(value[key]!==expected[key])fail();
 return expected;
}

// Every finalizer is attempted; cleanup/artifact failure never replaces the
// original test assertion. This helper performs no privileged operation itself.
export async function preserveMaintenanceFailure(primary,actions){
 let failure=primary;
 for(const action of actions){try{await action();}catch(error){failure??=error;}}
 return failure;
}
