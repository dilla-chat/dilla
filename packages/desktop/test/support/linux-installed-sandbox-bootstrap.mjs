// Disposable GitHub CI initial prerequisite only. No production runtime bypass.
import {constants} from 'node:fs';
import {open,lstat,realpath,mkdtemp,mkdir,readdir,writeFile,rm} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import {spawnSync} from 'node:child_process';
import {isAbsolute,join,resolve,dirname} from 'node:path';
import {buildCapsule,parsePublicConfiguration} from '../../scripts/linux-sandbox-package.mjs';
import {inspectAppImage} from '../../scripts/inspect-linux-sandbox-package.mjs';

const root='/usr/lib/dilla/appimage';
const repository=resolve(import.meta.dirname,'../../../..');
const hash=b=>createHash('sha512').update(b).digest('base64');
const exact=(x,keys)=>x&&typeof x==='object'&&!Array.isArray(x)&&Object.keys(x).sort().join(',')===[...keys].sort().join(',');
const validHash=x=>typeof x==='string'&&/^[A-Za-z0-9+/]{86}==$/.test(x)&&Buffer.from(x,'base64').toString('base64')===x;
function invalid(){throw Error('Invalid installed sandbox prerequisite');}
export function validateHostedContext({env,platform,arch,uid}){
 if(platform!=='linux'||arch!=='x64'||!Number.isInteger(uid)||uid<=0||env.GITHUB_ACTIONS!=='true'||env.RUNNER_ENVIRONMENT!=='github-hosted'||env.GITHUB_REPOSITORY!=='jonasthim/dilla'||!['GITHUB_RUN_ID','GITHUB_RUN_ATTEMPT'].every(k=>typeof env[k]==='string'&&/^[1-9][0-9]{0,19}$/.test(env[k]))||typeof env.GITHUB_SHA!=='string'||!/^[0-9a-f]{40}$/.test(env.GITHUB_SHA))invalid();
 return {runId:env.GITHUB_RUN_ID,attempt:env.GITHUB_RUN_ATTEMPT,sha:env.GITHUB_SHA};
}
function gitOutput(checkout,args,{input,maxBuffer=4*1024**2}={}){
 const result=spawnSync(process.platform==='linux'?'/usr/bin/git':'git',['-c','core.fsmonitor=false','-c','core.untrackedCache=false',...args],{cwd:checkout,env:{PATH:process.env.PATH,LANG:'C',LC_ALL:'C',GIT_CONFIG_NOSYSTEM:'1',GIT_CONFIG_GLOBAL:process.platform==='win32'?'NUL':'/dev/null'},input,timeout:10000,maxBuffer});
 if(result.error||result.signal||result.status!==0)invalid();return result.stdout;
}
// Materialize exact committed blobs, not a checkout/archive influenced by ignored
// files, filters or .git/info/attributes. Only this private HEAD tree is compiled.
// The pinned Go SDK and go.mod/go.sum-authenticated module cache are external
// build dependencies; working-tree compiler and embed inputs never enter it.
export async function createSourceSnapshot(checkout,expectedSHA,destination){
 const source=await verifySourceCheckout(checkout,expectedSHA);
 if(!isAbsolute(destination)||await realpath(destination)!==destination||(await readdir(destination)).length!==0)invalid();
 const entries=gitOutput(checkout,['ls-tree','-rz','--full-tree',source.sourceCommit]).toString('utf8').split('\0').filter(Boolean).map(record=>{
  const match=/^(100644|100755) blob ([0-9a-f]{40})\t(.+)$/.exec(record);
  if(!match||match[3].split('/').some(part=>!part||part==='.'||part==='..'||/[\x00-\x1f\x7f\\]/.test(part)))invalid();
  return {mode:match[1]==='100755'?0o700:0o600,id:match[2],path:match[3]};
 });
 if(!entries.length||entries.length>16384)invalid();
 const raw=gitOutput(checkout,['cat-file','--batch'],{input:entries.map(entry=>entry.id+'\n').join(''),maxBuffer:128*1024**2});
 let offset=0;
 for(const entry of entries){
  const end=raw.indexOf(10,offset);if(end<offset||end-offset>100)invalid();
  const header=/^([0-9a-f]{40}) blob ([0-9]+)$/.exec(raw.subarray(offset,end).toString('ascii'));if(!header||header[1]!==entry.id)invalid();
  const length=Number(header[2]);offset=end+1;
  if(!Number.isSafeInteger(length)||length<0||offset+length>=raw.length||raw[offset+length]!==10)invalid();
  const bytes=raw.subarray(offset,offset+length),digest=createHash('sha1').update(Buffer.from('blob '+length+'\0')).update(bytes).digest('hex');if(digest!==entry.id)invalid();offset+=length+1;
  const output=join(destination,entry.path);await mkdir(dirname(output),{recursive:true,mode:0o700});await writeFile(output,bytes,{flag:'wx',mode:entry.mode});
 }
 if(offset!==raw.length)invalid();return source;
}
// Testable checkout verifier; provisioning always supplies the fixed repository
// above. Git facts are obtained here, never accepted from callers or the job env.
export async function verifySourceCheckout(checkout,expectedSHA){
 if(typeof expectedSHA!=='string'||!/^[0-9a-f]{40}$/.test(expectedSHA)||!isAbsolute(checkout)||await realpath(checkout)!==checkout)invalid();
 const git=args=>gitOutput(checkout,args).toString('utf8');
 if(await realpath(git(['rev-parse','--show-toplevel']).trim())!==checkout)invalid();
 const sourceCommit=git(['rev-parse','HEAD']).trim(),sourceTree=git(['rev-parse','HEAD^{tree}']).trim();
 if(sourceCommit!==expectedSHA||!/^[0-9a-f]{40}$/.test(sourceTree)||git(['status','--porcelain=v1','--untracked-files=all','-z'])!=='')invalid();
 // Hidden index flags can make status lie about modified tracked inputs.
 if(git(['ls-files','-v','-z']).split('\0').filter(Boolean).some(entry=>!entry.startsWith('H ')))invalid();
 return {sourceCommit,sourceTree,sourceClean:true};
}
export function sudoBootstrapArguments(argv){
 if(!Array.isArray(argv)||argv.length!==6||argv.slice(0,5).join('\0')!=='--disable-internal-agent\0/usr/bin/python3\0-I\0-S\0-c'||typeof argv[5]!=='string'||argv[5].length<1||Buffer.byteLength(argv[5])>131072)invalid();
 return ['-n',...argv.slice(1)];
}
export function validateInitialBytes({capsule,manifest,configuration,proof}){
 if(!Buffer.isBuffer(capsule)||capsule.length<32||capsule.length>48*1024**2||!Buffer.isBuffer(manifest)||manifest.length<1||manifest.length>65536||capsule.subarray(0,8).toString('binary')!=='DLSBOX1\0'||capsule.readUInt32LE(12)!==0)invalid();
 const ml=capsule.readUInt32LE(8),nl=Number(capsule.readBigUInt64LE(16)),hl=Number(capsule.readBigUInt64LE(24));
 if(ml<1||ml>65536||!Number.isSafeInteger(nl)||nl<1||nl>32*1024**2||!Number.isSafeInteger(hl)||hl<1||hl>8*1024**2||32+ml+nl+hl!==capsule.length||!manifest.equals(capsule.subarray(32,32+ml)))invalid();
 const manager=capsule.subarray(32+ml,32+ml+nl),helper=capsule.subarray(32+ml+nl),regenerated=buildCapsule(configuration,manager,helper);
 if(!regenerated.capsule.equals(capsule)||!proof||proof.capsuleSha512!==hash(capsule)||proof.manifestSha512!==hash(manifest)||proof.embeddedCapsuleSha512!==hash(capsule)||proof.embeddedManifestSha512!==hash(manifest))invalid();
 return {capsuleSha512:hash(capsule),manifestSha512:hash(manifest),maintainerSha512:hash(manager),helperSha512:hash(helper),generation:configuration.generation,manifest};
}
function same(a,b){return ['dev','ino','mode','uid','gid','nlink','size','mtimeNs','ctimeNs'].every(k=>a[k]===b[k]);}
export async function readBoundedOwned(filename,max,{uid=process.getuid(),mode}={}){
 const path=resolve(filename);if(!isAbsolute(filename)||path!==filename||await realpath(path)!==path)invalid();
 const before=await lstat(path,{bigint:true});if(!before.isFile()||before.isSymbolicLink()||before.nlink!==1n||before.uid!==BigInt(uid)||before.size<1n||before.size>BigInt(max)||(before.mode&0o22n)!==0n||(mode!==undefined&&(before.mode&0o7777n)!==BigInt(mode)))invalid();
 const file=await open(path,constants.O_RDONLY|constants.O_NOFOLLOW|constants.O_NONBLOCK);
 try{const pinned=await file.stat({bigint:true});if(!same(before,pinned))invalid();const raw=Buffer.alloc(Number(pinned.size)+1);let length=0;while(length<raw.length){const {bytesRead}=await file.read(raw,length,raw.length-length,length);if(!bytesRead)break;length+=bytesRead;}if(length!==Number(pinned.size)||!same(pinned,await file.stat({bigint:true})))invalid();return {bytes:raw.subarray(0,length),info:pinned};}finally{await file.close();}
}
async function hashOwnedFile(filename,max){
 const path=resolve(filename);if(filename!==path||await realpath(path)!==path)invalid();const file=await open(path,constants.O_RDONLY|constants.O_NOFOLLOW);try{const before=await file.stat({bigint:true});if(!before.isFile()||before.uid!==BigInt(process.getuid())||before.nlink!==1n||before.size<1n||before.size>BigInt(max)||(before.mode&0o22n)!==0n)invalid();const h=createHash('sha512');let total=0;for await(const b of file.createReadStream({autoClose:false})){total+=b.length;if(total>max)invalid();h.update(b);}if(BigInt(total)!==before.size||!same(before,await file.stat({bigint:true})))invalid();return h.digest('base64');}finally{await file.close();}
}
function execute(program,args,{input,env,cwd=repository,timeout=120000,maxBuffer=262144}={}){
 const result=spawnSync(program,args,{cwd,input,env:env??process.env,encoding:'utf8',timeout,maxBuffer});if(result.error||result.signal||result.status!==0)throw Error('Installed sandbox prerequisite command failed');return result.stdout;
}
export function validateReadyProof({status,receipt,expected}){
 if(!exact(status,['format','status','generation','compatibilityEpoch','sandboxApi','maintainerSha512','helperSha512'])||status.format!==1||status.status!=='ready'||status.generation!==expected.generation||status.compatibilityEpoch!==1||status.sandboxApi!==1||status.maintainerSha512!==expected.maintainerSha512||status.helperSha512!==expected.helperSha512)invalid();
 const keys=['format','authorization','initialAnchorSha512','manifest','signature','acceptedAt','lastOperationAt','maximumGeneration','maintainerSha512','helperSha512','compatibilityEpoch','sandboxApi','state','completedOperation','transactionSha512'];
 if(!exact(receipt,keys)||receipt.format!==1||receipt.authorization!=='initial-digest'||receipt.signature!==''||receipt.maximumGeneration!==expected.generation||receipt.maintainerSha512!==expected.maintainerSha512||receipt.helperSha512!==expected.helperSha512||receipt.compatibilityEpoch!==1||receipt.sandboxApi!==1||receipt.state!=='active'||receipt.completedOperation!=='install'||!validHash(receipt.initialAnchorSha512)||!validHash(receipt.transactionSha512)||!Number.isSafeInteger(receipt.acceptedAt)||receipt.acceptedAt<1||!Number.isSafeInteger(receipt.lastOperationAt)||receipt.lastOperationAt<receipt.acceptedAt||typeof receipt.manifest!=='string'||Buffer.from(receipt.manifest,'base64').toString('base64')!==receipt.manifest||!Buffer.from(receipt.manifest,'base64').equals(expected.manifest))invalid();
 return {initialAnchorSha512:receipt.initialAnchorSha512,transactionSha512:receipt.transactionSha512};
}
// Exactly one call per fresh job. Attempt records deliberately forbid retry or
// adoption after interrupted setup; unknown state is failed CI + VM disposal.
export async function provisionInstalledSandbox({image,configurationFile}){
 const job=validateHostedContext({env:process.env,platform:process.platform,arch:process.arch,uid:process.getuid?.()});
 const source=await verifySourceCheckout(repository,job.sha);
 const temp=process.env.RUNNER_TEMP;if(typeof temp!=='string'||!isAbsolute(temp)||await realpath(temp)!==temp)invalid();const tempInfo=await lstat(temp);if(!tempInfo.isDirectory()||tempInfo.isSymbolicLink()||tempInfo.uid!==process.getuid()||(tempInfo.mode&0o22)!==0)invalid();
 const suffix=image.endsWith('.AppImage')?image.slice(0,-9):undefined;if(!suffix)invalid();
 const [{bytes:capsule},{bytes:manifest},{bytes:configurationRaw}]=await Promise.all([readBoundedOwned(suffix+'.sandbox',48*1024**2),readBoundedOwned(suffix+'.sandbox.json',65536),readBoundedOwned(configurationFile,16384)]);
 const configuration=parsePublicConfiguration(configurationRaw.toString('utf8')),imageSha512=await hashOwnedFile(image,4*1024**3);
 // The inspector extracts the actual final image, verifies the seven fixed
 // members/fuses/ASAR and reproduces its launcher against this public config.
 const inspected=await inspectAppImage({image,capsuleFile:suffix+'.sandbox',manifestFile:suffix+'.sandbox.json',configurationFile});
 const expected=validateInitialBytes({capsule,manifest,configuration,proof:inspected});if(await hashOwnedFile(image,4*1024**3)!==imageSha512)invalid();
 try{await lstat(root);throw Error('Installed sandbox prerequisite refuses preexisting root');}catch(error){if(error.code!=='ENOENT')throw error;}
 const attempt=join(temp,'dilla-installed-sandbox-initial-attempt.json');await writeFile(attempt,JSON.stringify({format:1,...job,capsuleSha512:expected.capsuleSha512,imageSha512,phase:'started'})+'\n',{flag:'wx',mode:0o600});
 const build=await realpath(await mkdtemp(join(temp,'dilla-initial-bootstrap-')));
 try{
  const snapshot=join(build,'source');await mkdir(snapshot,{mode:0o700});await createSourceSnapshot(repository,job.sha,snapshot);
  const exporter=join(build,'exporter');const flags='-X main.expectedCapsuleSHA512='+expected.capsuleSha512+' -X main.compiledPublicConfiguration='+Buffer.from(JSON.stringify(configuration)).toString('base64');
  execute('go',['build','-mod=readonly','-trimpath','-buildvcs=false','-ldflags',flags,'-o',exporter,'./packages/desktop/test/fixtures/linux-installed-sandbox-bootstrap'],{cwd:snapshot,env:{...process.env,CGO_ENABLED:'0',GOOS:'linux',GOARCH:'amd64',GOENV:'off',GOWORK:'off',GOFLAGS:'',GOEXPERIMENT:'',GOTOOLCHAIN:configuration.goVersion}});
  const emitted=JSON.parse(execute(exporter,[],{input:capsule,timeout:10000}));
  if(!exact(emitted,['format','capsuleSha512','manifestSha512','maintainerSha512','helperSha512','generation','constants','argv'])||emitted.format!==1||['capsuleSha512','manifestSha512','maintainerSha512','helperSha512','generation'].some(k=>emitted[k]!==expected[k]))invalid();
  const args=sudoBootstrapArguments(emitted.argv);
  const finalSource=await verifySourceCheckout(repository,job.sha);if(finalSource.sourceTree!==source.sourceTree)invalid();
  execute('/usr/bin/sudo',args,{input:capsule,env:{PATH:'/usr/bin:/bin',LANG:'C',LC_ALL:'C'},timeout:90000,maxBuffer:4096});
  const [manager,helper,receiptFile]=await Promise.all([readBoundedOwned(root+'/sandbox-maintainer',32*1024**2,{uid:0,mode:0o755}),readBoundedOwned(root+'/chrome-sandbox',8*1024**2,{uid:0,mode:0o4755}),readBoundedOwned(root+'/sandbox-receipt.json',131072,{uid:0,mode:0o644})]);
  for(const file of [manager,helper,receiptFile])if(file.info.gid!==0n)invalid();if(hash(manager.bytes)!==expected.maintainerSha512||hash(helper.bytes)!==expected.helperSha512)invalid();
  const status=JSON.parse(execute(root+'/sandbox-maintainer',['--check'],{env:{PATH:'/usr/bin:/bin',LANG:'C',LC_ALL:'C'},timeout:5000,maxBuffer:4096}));
  const receipt=parsePublicConfiguration(receiptFile.bytes.toString('utf8'));const committed=validateReadyProof({status,receipt,expected});
  const proof={format:1,kind:'dilla-installed-sandbox-initial',...job,...source,...committed,status:'ready',generation:expected.generation,imageSha512,capsuleSha512:expected.capsuleSha512,manifestSha512:expected.manifestSha512,maintainerSha512:expected.maintainerSha512,helperSha512:expected.helperSha512,receiptSha512:hash(receiptFile.bytes),helperDevice:String(helper.info.dev),helperInode:String(helper.info.ino),helperMode:0o4755,disposalOnly:true};
  const output=join(temp,'dilla-installed-sandbox-initial.json');await writeFile(output,JSON.stringify(proof,null,2)+'\n',{flag:'wx',mode:0o600});return proof;
 }finally{await rm(build,{recursive:true,force:true});}
}
