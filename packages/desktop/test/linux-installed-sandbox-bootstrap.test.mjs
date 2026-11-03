import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createHash,generateKeyPairSync} from 'node:crypto';
import {mkdtemp,realpath,writeFile,chmod,link,symlink,mkdir,rm} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {execFileSync} from 'node:child_process';
import {buildCapsule} from '../scripts/linux-sandbox-package.mjs';
const hash=b=>createHash('sha512').update(b).digest('base64');
function fixture(){const helper=Buffer.from('actual pinned helper fixture'),maintainer=Buffer.from('actual maintainer fixture');const {publicKey}=generateKeyPairSync('ed25519');const now=Date.now();const configuration={goVersion:'go1.27.3',generation:1,publishedAt:new Date(now-1000).toISOString(),expiresAt:new Date(now+3600000).toISOString(),publicKeys:[Buffer.from(publicKey.export({format:'jwk'}).x,'base64url').toString('base64')],helper:{version:'ephemeral-fixture',size:helper.length,sha512:hash(helper),sha256:createHash('sha256').update(helper).digest('hex')}};const value=buildCapsule(configuration,maintainer,helper);return {...value,configuration};}
test('initial prerequisite binds exact unsigned capsule, detached manifest and finalized package proof',async()=>{
 const {validateInitialBytes}=await import('./support/linux-installed-sandbox-bootstrap.mjs');const f=fixture();const proof={capsuleSha512:hash(f.capsule),manifestSha512:hash(f.manifest),embeddedCapsuleSha512:hash(f.capsule),embeddedManifestSha512:hash(f.manifest)};
 const value=validateInitialBytes({...f,proof});assert.equal(value.capsuleSha512,proof.capsuleSha512);
 for(const change of ['capsule','manifest','proof','signed']){const input={...f,proof:{...proof}};if(change==='capsule'){input.capsule=Buffer.from(f.capsule);input.capsule.at(-1);input.capsule[input.capsule.length-1]^=1;}if(change==='manifest')input.manifest=Buffer.from('{}');if(change==='proof')input.proof.capsuleSha512=hash(Buffer.from('other'));if(change==='signed'){input.capsule=Buffer.from(f.capsule);input.capsule.writeUInt32LE(64,12);}assert.throws(()=>validateInitialBytes(input));}
});
test('CI auth replacement only selects noninteractive sudo and the exact reviewed OS interpreter argv',async()=>{
 const {sudoBootstrapArguments}=await import('./support/linux-installed-sandbox-bootstrap.mjs');const argv=['--disable-internal-agent','/usr/bin/python3','-I','-S','-c','fixed literal'];assert.deepEqual(sudoBootstrapArguments(argv),['-n',...argv.slice(1)]);
 for(const index of [0,1,2,3,4]){const changed=[...argv];changed[index]='/user/program';assert.throws(()=>sudoBootstrapArguments(changed));}assert.throws(()=>sudoBootstrapArguments([...argv,'extra']));
});
test('hosted bootstrap boundary rejects local, self-hosted, wrong repository and missing immutable job identity',async()=>{
 const {validateHostedContext}=await import('./support/linux-installed-sandbox-bootstrap.mjs');const env={GITHUB_ACTIONS:'true',RUNNER_ENVIRONMENT:'github-hosted',GITHUB_REPOSITORY:'dilla-chat/dilla',GITHUB_RUN_ID:'123',GITHUB_RUN_ATTEMPT:'1',GITHUB_SHA:'a'.repeat(40)};
 assert.deepEqual(validateHostedContext({env,platform:'linux',arch:'x64',uid:1000}),{runId:'123',attempt:'1',sha:'a'.repeat(40)});
 for(const [key,value]of [['GITHUB_ACTIONS','false'],['RUNNER_ENVIRONMENT','self-hosted'],['GITHUB_REPOSITORY','other/repo'],['GITHUB_RUN_ID',''],['GITHUB_SHA','unknown']])assert.throws(()=>validateHostedContext({env:{...env,[key]:value},platform:'linux',arch:'x64',uid:1000}));
 for(const changed of [{platform:'darwin'},{arch:'arm64'},{uid:0}])assert.throws(()=>validateHostedContext({env,platform:'linux',arch:'x64',uid:1000,...changed}));
});

test('bounded file reader pins actual owned regular bytes and rejects unsafe inputs',{skip:process.platform==='win32'},async()=>{
 const {readBoundedOwned}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const root=await realpath(await mkdtemp(join(tmpdir(),'installed-sandbox-input-')));
 try{
  const file=join(root,'capsule');await writeFile(file,Buffer.from([1,2,3]),{mode:0o600});
  assert.deepEqual((await readBoundedOwned(file,3)).bytes,Buffer.from([1,2,3]));
  await assert.rejects(readBoundedOwned(file,2));await assert.rejects(readBoundedOwned(file,3,{uid:process.getuid()+1}));
  await assert.rejects(readBoundedOwned(file,3,{mode:0o644}));
  const alias=join(root,'alias');await symlink(file,alias);await assert.rejects(readBoundedOwned(alias,3));
  const second=join(root,'second');await link(file,second);await assert.rejects(readBoundedOwned(file,3));await rm(second);
  await chmod(file,0o622);await assert.rejects(readBoundedOwned(file,3));await chmod(file,0o600);
  const directory=join(root,'directory');await mkdir(directory);await assert.rejects(readBoundedOwned(directory,3));
  await writeFile(file,'');await assert.rejects(readBoundedOwned(file,3));
 }finally{await rm(root,{recursive:true,force:true});}
});
test('ready proof requires exact initial committed receipt and real component tuple',async()=>{
 const {validateReadyProof,validateInitialBytes}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const f=fixture(),proof={capsuleSha512:hash(f.capsule),manifestSha512:hash(f.manifest),embeddedCapsuleSha512:hash(f.capsule),embeddedManifestSha512:hash(f.manifest)};
 const expected=validateInitialBytes({...f,proof});
 const status={format:1,status:'ready',generation:1,compatibilityEpoch:1,sandboxApi:1,maintainerSha512:expected.maintainerSha512,helperSha512:expected.helperSha512};
 const receipt={format:1,authorization:'initial-digest',initialAnchorSha512:hash(Buffer.from('root anchor')),manifest:f.manifest.toString('base64'),signature:'',acceptedAt:Date.now(),lastOperationAt:Date.now(),maximumGeneration:1,maintainerSha512:expected.maintainerSha512,helperSha512:expected.helperSha512,compatibilityEpoch:1,sandboxApi:1,state:'active',completedOperation:'install',transactionSha512:hash(Buffer.from('durable transaction'))};
 assert.deepEqual(validateReadyProof({status,receipt,expected}),{initialAnchorSha512:receipt.initialAnchorSha512,transactionSha512:receipt.transactionSha512});
 for(const change of [{signature:null},{authorization:'maintenance-signature'},{manifest:Buffer.from('{}').toString('base64')},{maximumGeneration:2},{lastOperationAt:0},{transactionSha512:'unknown'},{state:'removed'},{extra:'unknown'}])assert.throws(()=>validateReadyProof({status,receipt:{...receipt,...change},expected}));
 for(const change of [{status:'pending'},{generation:2},{helperSha512:hash(Buffer.from('other'))},{extra:1}])assert.throws(()=>validateReadyProof({status:{...status,...change},receipt,expected}));
});

test('source authority binds actual clean Git HEAD/tree and rejects changed or missing checkout',async()=>{
 const {verifySourceCheckout}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const root=await realpath(await mkdtemp(join(tmpdir(),'sandbox-source-authority-')));
 const git=args=>execFileSync('git',args,{cwd:root,encoding:'utf8',stdio:['ignore','pipe','pipe']}).trim();
 try{
  await assert.rejects(verifySourceCheckout(root,'a'.repeat(40)));
  git(['init','-q']);git(['config','user.email','fixture@example.invalid']);git(['config','user.name','Fixture']);
  await writeFile(join(root,'source.go'),'reviewed source');await writeFile(join(root,'.gitignore'),'generated/\n');git(['add','.']);git(['commit','-qm','fixture']);
  const head=git(['rev-parse','HEAD']),tree=git(['rev-parse','HEAD^{tree}']);
  assert.deepEqual(await verifySourceCheckout(root,head),{sourceCommit:head,sourceTree:tree,sourceClean:true});
  await assert.rejects(verifySourceCheckout(root,'a'.repeat(40)));
  await mkdir(join(root,'generated'));await writeFile(join(root,'generated','fixture-public.json'),'ignored build input');
  assert.equal((await verifySourceCheckout(root,head)).sourceTree,tree);
  await writeFile(join(root,'source.go'),'unreviewed source');await assert.rejects(verifySourceCheckout(root,head));git(['checkout','--','source.go']);
  await writeFile(join(root,'unknown.go'),'untracked source');await assert.rejects(verifySourceCheckout(root,head));await rm(join(root,'unknown.go'));
  git(['update-index','--skip-worktree','source.go']);await writeFile(join(root,'source.go'),'hidden changed source');await assert.rejects(verifySourceCheckout(root,head));
 }finally{await rm(root,{recursive:true,force:true});}
});
test('exporter builds only HEAD bytes even when Git ignores an injected compiler input',{skip:process.platform==='win32'},async()=>{
 const {createSourceSnapshot}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const root=await realpath(await mkdtemp(join(tmpdir(),'sandbox-go-snapshot-'))),destination=await realpath(await mkdtemp(join(tmpdir(),'sandbox-head-')));
 const run=(program,args,cwd=root)=>execFileSync(program,args,{cwd,encoding:'utf8',env:{...process.env,CGO_ENABLED:'0',GOWORK:'off'},stdio:['ignore','pipe','pipe']}).trim();
 try{
  run('git',['init','-q']);run('git',['config','user.email','fixture@example.invalid']);run('git',['config','user.name','Fixture']);
  await writeFile(join(root,'go.mod'),'module fixture.invalid/sandbox\n\ngo 1.27.0\n');
  await writeFile(join(root,'main.go'),'package main\nimport "fmt"\nfunc main(){fmt.Println("reviewed")}\n');run('git',['add','.']);run('git',['commit','-qm','reviewed compiler inputs']);
  const head=run('git',['rev-parse','HEAD']);await writeFile(join(root,'.git','info','exclude'),'injected.go\n');
  await writeFile(join(root,'injected.go'),'package main\nimport "fmt"\nfunc init(){fmt.Println("injected")}\n');
  assert.equal(run('git',['status','--porcelain','--untracked-files=all']),'');
  const unsafe=join(destination,'unsafe');run('go',['build','-buildvcs=false','-o',unsafe,'.']);assert.equal(run(unsafe,[]),'injected\nreviewed');await rm(unsafe);
  await createSourceSnapshot(root,head,destination);
  const safe=join(destination,'safe');run('go',['build','-buildvcs=false','-o',safe,'.'],destination);assert.equal(run(safe,[]),'reviewed');
 }finally{await rm(root,{recursive:true,force:true});await rm(destination,{recursive:true,force:true});}
});

test('actual Node socket stdin is rejected while exact unlinked read-only capsule FD satisfies the existing Python contract',{skip:process.platform!=='linux'},async()=>{
 const {openCapsuleStdin}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const probe='import os,stat,fcntl,sys; s=os.fstat(0); ok=stat.S_ISREG(s.st_mode) or stat.S_ISFIFO(s.st_mode); print("REG" if stat.S_ISREG(s.st_mode) else "SOCKET" if stat.S_ISSOCK(s.st_mode) else "OTHER"); sys.stdout.flush(); sys.exit(42) if not ok else None; assert fcntl.fcntl(0,fcntl.F_GETFL)&os.O_ACCMODE==os.O_RDONLY; assert s.st_nlink==0; assert sys.stdin.buffer.read()==b"exact public capsule bytes"';
 const {spawnSync}=await import('node:child_process');const bytes=Buffer.from('exact public capsule bytes');
 const rejected=spawnSync('/usr/bin/python3',['-I','-S','-c',probe],{input:bytes,encoding:'utf8'});assert.equal(rejected.status,42);assert.equal(rejected.stdout.trim(),'SOCKET');
 const root=await realpath(await mkdtemp(join(tmpdir(),'sandbox-stdin-')));let input;
 try{input=await openCapsuleStdin(root,bytes);const accepted=spawnSync('/usr/bin/python3',['-I','-S','-c',probe],{stdio:[input.fd,'pipe','pipe'],encoding:'utf8'});assert.equal(accepted.status,0,accepted.stderr);assert.equal(accepted.stdout.trim(),'REG');assert.deepEqual(await (await import('node:fs/promises')).readdir(root),[]);}finally{await input?.close();await rm(root,{recursive:true,force:true});}
});
test('prerequisite command errors retain fixed bootstrap stage and exit reason without raw arguments or output',async()=>{
 const {prerequisiteCommandFailure}=await import('./support/linux-installed-sandbox-bootstrap.mjs');
 const error=prerequisiteCommandFailure('bootstrap',{status:1,stdout:'PRIVATE_CAPSULE=never',stderr:'Dilla sandbox bootstrap refused unsafe or incomplete setup.\n'});
 assert.match(error.message,/bootstrap.*exit=1.*bootstrap-refused/);assert.equal(error.prerequisiteDiagnostic.reason,'bootstrap-refused');
 const unknown=prerequisiteCommandFailure('bootstrap',{status:null,signal:'SIGTERM',error:{code:'ETIMEDOUT'},stderr:'PRIVATE_ENV=secret\n'.repeat(10000)});const raw=JSON.stringify(unknown.prerequisiteDiagnostic);assert.doesNotMatch(raw,/PRIVATE_ENV|secret|PRIVATE_CAPSULE/);assert.equal(unknown.prerequisiteDiagnostic.signal,'SIGTERM');assert.equal(unknown.prerequisiteDiagnostic.spawnError,'ETIMEDOUT');assert.equal(unknown.prerequisiteDiagnostic.truncated,true);assert.ok(raw.length<1024);
});
