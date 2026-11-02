// Content-free CI fixture only. No production root, key or runtime override.
import {readFileSync} from 'node:fs';
import {createHash} from 'node:crypto';
import {getSystemErrorMap} from 'node:util';
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

// Audited output boundary: native_root_linux_test.go has only fixed assertions,
// syscall/standard errors and fixed content-free success/crash records. Never
// copy arbitrary child stdout, environment, paths, capsule bytes or key material.
// Unknown text is retained only as a content-free hash/count, visibly marked.
export const AUDITED_NATIVE_REASONS=new Set(["EOF", "cleanup directory identity changed", "cleanup identity changed", "cleanup late pathname change", "closed, foreign or malformed lifecycle state", "compiled fixture input mismatch", "configuration preflight must be unprivileged", "context canceled", "context deadline exceeded", "credential execution did not match expected kernel state", "cross-device staging did not reach identity rejection", "expected owned actor pipe", "expected owned pipe", "file already closed", "file type does not support deadline", "fixed crash point not reached", "fixture root not empty", "foreign cleanup root", "foreign or unverified CI ownership", "holder opened other inode", "i/o timeout", "incompatible Linux sandbox executable", "install recovery lost admitted generation", "invalid Linux sandbox capsule framing", "invalid Linux sandbox manifest", "invalid Linux sandbox protected state", "invalid lifecycle digest", "invalid protected nonce", "kernel interruption matrix incomplete", "late guard deleted substitute", "late inode substitution advanced receipt", "late inode substitution did not HOLD", "lifecycle lease must be read-only", "linux sandbox acceptance time rejected", "linux sandbox component is not authenticated", "linux sandbox generation policy rejected", "linux sandbox prerequisite is not compatible", "missing compile-distinct native configuration", "missing fixed actor role", "missing lifecycle admission", "missing lifecycle exclusion", "native input changed", "native sibling changed", "native sibling was modified", "oversized credential response", "oversized native namespace", "owned actor did not die by SIGKILL", "owned actor did not reach exact crash boundary", "owned holder cleanup timed out", "production namespace must remain absent", "removal recovery lost floor", "root lifecycle fixture requires disposable CI", "same-device bind mount did not reach identity rejection", "sandbox namespace entry absent", "short write", "unconfigured disposable root fixture", "unexpected EOF", "unexpected native namespace child", "unexpected privileged fixture leaf", "unknown fixed actor role", "unprivileged holder credentials invalid", "unsafe fixed native input", "verified native ELF changed", "wrong lifecycle lease descriptor"]);
const systemReasons=new Set([...getSystemErrorMap().values()].map(value=>value[1]));
function auditedReason(text){
 if(text.length>256||!/^[\x20-\x7e]+$/.test(text))return undefined;
 if(AUDITED_NATIVE_REASONS.has(text)||systemReasons.has(text))return text;
 if(/^(?:exit status [0-9]{1,3}|signal: (?:killed|terminated|aborted|segmentation fault|interrupt))$/.test(text))return text;
 for(const prefix of ['fixed crash point not reached','same-device bind mount did not reach identity rejection','cross-device staging did not reach identity rejection'])if(text.startsWith(prefix+' ')){
  const reason=text.slice(prefix.length+1);if(reason==='<nil>')return text;const safe=auditedReason(reason);if(safe)return prefix+' '+safe;
 }
 // PathError is useful for its operation and errno, not its pathname. These
 // are the only fixture-owned path/pipe forms reachable in the audited test.
 const path=/^(open|read|write|close|stat|lstat|fork\/exec|waitid) (?:\/(?:usr\/lib|run)\/dilla-sandbox-maintenance-ci(?:\/[A-Za-z0-9._-]+)*|\|[0-9]{1,3}): (.+)$/.exec(text);
 if(path&&(AUDITED_NATIVE_REASONS.has(path[2])||systemReasons.has(path[2])))return path[1]+' <fixed-fixture-path>: '+path[2];
 return undefined;
}
export function maintenanceCommandFailure(result){
 const limit=65536,entries=[],unknown=createHash('sha256');let remaining=limit,unknownLines=0,truncated=false;
 for(const stream of ['stdout','stderr']){
  const value=typeof result[stream]==='string'?Buffer.from(result[stream]):Buffer.alloc(0);if(value.length>remaining)truncated=true;
  const selected=value.subarray(0,remaining);remaining-=selected.length;
  for(const line of selected.toString('utf8').split('\n')){
   if(!line.trim())continue;
   let entry;
   const assertion=/^\s*native_root_linux_test\.go:([1-9][0-9]{0,4}): (.{1,4096})$/.exec(line);
   if(assertion){const reason=auditedReason(assertion[2]);if(reason)entry={kind:'assertion',line:Number(assertion[1]),reason};else entry={kind:'unrecognized-assertion',line:Number(assertion[1])};}
   const test=/^(?:=== RUN   |--- (FAIL|PASS): )(Test(?:DisposableRoot(?:Transaction|Actor|Cleanup)|NativeCompiledConfiguration)(?:\/crash-(?:demoted|helper|receipt|remove|initial|terminal))?)(?: \([0-9.]+s\))?$/.exec(line.trim());
   if(test)entry={kind:'test',test:test[2],outcome:test[1]?.toLowerCase()??'run'};
   if(line==='Dilla CI maintenance staging refused unsafe or busy lifecycle.')entry={kind:'staging',reason:line};
   const namespace=/^unshare: unshare failed: ([A-Za-z ]{1,100})$/.exec(line);if(namespace&&systemReasons.has(namespace[1].toLowerCase()))entry={kind:'namespace',reason:'unshare failed: '+namespace[1]};
   if(entry&&entries.length<128)entries.push(entry);else if(entry)truncated=true;
   if(!entry||entry.kind==='unrecognized-assertion'){unknownLines++;unknown.update(line);unknown.update('\n');}
  }
 }
 const signals=new Set(['SIGTERM','SIGKILL','SIGABRT','SIGSEGV','SIGINT','SIGPIPE','SIGBUS','SIGILL','SIGTRAP']);
 const codes=new Set(['ETIMEDOUT','ENOBUFS','ENOENT','EACCES','EPIPE','ENOMEM']);
 const diagnostic={format:1,exitCode:Number.isInteger(result.status)&&result.status>=0&&result.status<=255?result.status:null,signal:signals.has(result.signal)?result.signal:null,spawnError:result.error?(codes.has(result.error.code)?result.error.code:'other'):null,entries,unknownLines,unknownSha256:unknown.digest('hex'),truncated};
 const error=Error('Maintenance fixture command failed ('+(diagnostic.exitCode??'unavailable')+')');error.maintenanceDiagnostic=diagnostic;return error;
}

// Same recorder used by the real wrapper and unprivileged failure transport
// tests. The primary error is retained by identity, including safe diagnostics.
export function captureMaintenanceFailure(proof,error){
 proof.primaryFailure=true;if(error?.maintenanceDiagnostic)proof.diagnostic=error.maintenanceDiagnostic;return error;
}
