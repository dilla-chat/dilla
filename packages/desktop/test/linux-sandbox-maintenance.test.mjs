import test from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,readFile,writeFile,mkdir,rm,stat,chmod,symlink,link,copyFile,rename,open} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {createHash} from 'node:crypto';
import {spawn,spawnSync} from 'node:child_process';

const sourceURL=new URL('./fixtures/linux-sandbox-maintenance/root-stage.py',import.meta.url);
const identity=['1234','1','a'.repeat(40)];
async function fixture(t){
 const base=await mkdtemp(join(tmpdir(),'sandbox-maintenance-stage-'));t.after(()=>rm(base,{recursive:true,force:true}));
 let code=await readFile(sourceURL,'utf8');
 for(const [before,after]of [['ANCHOR = "/"','ANCHOR = '+JSON.stringify(base)],['PARTS = ("usr", "lib", "dilla-sandbox-maintenance-ci")','PARTS = ("domain",)'],['LIFE_ANCHOR = "/run"','LIFE_ANCHOR = '+JSON.stringify(base)],['LIFE_NAME = "dilla-sandbox-maintenance-ci"','LIFE_NAME = "lifecycle"'],['LOCK_TIMEOUT = 5.0','LOCK_TIMEOUT = 1.0'],['INPUT_TIMEOUT = 30.0','INPUT_TIMEOUT = 3.0'],['OWNER_UID = 0','OWNER_UID = '+process.getuid()],['OWNER_GID = 0','OWNER_GID = '+process.getgid()]]){
  assert.equal(code.split(before).length,2,'fixed test compile boundary');code=code.replace(before,after);
 }
 const root=join(base,'domain');
 const run=(role,args=[])=>spawnSync('/usr/bin/python3',['-I','-S','-c',code,role,...identity,...(role==='init'&&args.length===0?Array(4).fill(Buffer.alloc(64).toString('base64')):args)],{encoding:'utf8',timeout:3000,env:{PATH:'/untrusted',PYTHONPATH:'/untrusted',PRIVATE_SECRET:'never print'}});
 return {base,root,run,code};
}
function pass(result){assert.equal(result.status,0,result.stderr);assert.equal(result.stderr,'');}
function refused(result){assert.notEqual(result.status,0);assert.doesNotMatch(result.stdout+result.stderr,/never print|Traceback|\/tmp\//);}

test('independent cleanup removes missing or truncated non-SUID native copy without executing it',{skip:process.platform!=='linux'},async t=>{
 for(const kind of ['missing-runner','missing-native','empty-native','partial-native']){
  const f=await fixture(t);pass(f.run('init'));
  if(kind==='missing-runner')await rm(join(f.root,'.runner'),{recursive:true});
  if(kind==='empty-native'||kind==='partial-native')await writeFile(join(f.root,'.runner/native.test'),kind==='empty-native'?'':'PRIVATE_SECRET',{mode:0o700});
  pass(f.run('cleanup'));await assert.rejects(stat(f.root),{code:'ENOENT'});
 }
});
test('foreign job or malformed proof never authorizes cleanup',{skip:process.platform!=='linux'},async t=>{
 for(const change of ['foreign','null','oversized']){
  const f=await fixture(t);pass(f.run('init'));const path=join(f.root,'.ci-owner.json');let marker=JSON.parse(await readFile(path,'utf8'));
  if(change==='foreign')marker.runId='999';
  await writeFile(path,change==='null'?'null':change==='oversized'?' '.repeat(4097):JSON.stringify(marker));
  refused(f.run('cleanup'));assert.ok((await stat(f.root)).isDirectory());
 }
});
test('unexpected child or writable staged file holds before any deletion',{skip:process.platform!=='linux'},async t=>{
 for(const kind of ['unknown','writable']){
  const f=await fixture(t);pass(f.run('init'));await writeFile(join(f.root,'.runner/native.test'),'keep',{mode:0o700});
  if(kind==='unknown')await writeFile(join(f.root,'unexpected'),'keep');else await chmod(join(f.root,'.runner/native.test'),0o722);
  refused(f.run('cleanup'));assert.equal(await readFile(join(f.root,'.runner/native.test'),'utf8'),'keep');
 }
});
test('unverified transaction state cannot execute a partial native binary',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('init'));await mkdir(join(f.root,'appimage'));await writeFile(join(f.root,'.runner/native.test'),'PRIVATE_SECRET',{mode:0o700});
 refused(f.run('cleanup'));assert.equal(await readFile(join(f.root,'.runner/native.test'),'utf8'),'PRIVATE_SECRET');
});

test('proof verifier requires exact actual credential and kernel observations without leaking fields',async()=>{
 const {verifyMaintenanceProof}=await import('./support/linux-sandbox-maintenance.mjs');
 const p={format:1,kind:'dilla-sandbox-maintenance-kernel',positive:0,demoted:65534,exchanged:65534,unlinked:65534,fresh:0,recoveries:6,mounts:2,lateInode:true,nativeUntouched:true,productionAbsent:true};
 const encode=x=>'DILLA_SANDBOX_MAINTENANCE_PROOF '+JSON.stringify(x)+'\n';
 assert.deepEqual(verifyMaintenanceProof(encode(p)),p);
 for(const changed of [{demoted:0},{positive:65534},{recoveries:5},{mounts:1},{nativeUntouched:false},{secret:'PRIVATE_SECRET'},{format:true}])assert.throws(()=>verifyMaintenanceProof(encode({...p,...changed})),/^Error: Invalid maintenance proof$/);
 assert.throws(()=>verifyMaintenanceProof(encode(p)+encode(p)));assert.throws(()=>verifyMaintenanceProof('x'.repeat(65537)));
 assert.throws(()=>verifyMaintenanceProof(encode(p).replace('"format":1','"format":1,"format":1')));
});
test('privileged command manifest has fixed destinations and no root source-path execution',async()=>{
 const {maintenanceCommands}=await import('./support/linux-sandbox-maintenance.mjs');
 const commands=maintenanceCommands({build:'/owned/build',identity,hashes:Array(4).fill(Buffer.alloc(64).toString('base64'))});
 assert.equal(commands.length,8);
 assert.deepEqual(commands.map(command=>command[command.indexOf('-c')+2]),['init','stage-native','stage-v1','stage-v2','stage-observer','verify','run','cleanup']);assert.equal(commands.flat().includes('/usr/bin/install'),false);assert.equal(commands.at(-2).includes('/usr/bin/unshare'),false);
 assert.deepEqual(commands[0].slice(0,4),['/usr/bin/python3','-I','-S','-c']);
 for(const command of commands){assert.equal(command.some(x=>['/bin/sh','bash','--no-sandbox','--disable-setuid-sandbox'].includes(x)),false);if(command[0]!=='/usr/bin/install')assert.equal(command.includes('/owned/build/native.test'),false);}
 assert.equal(commands.at(-1).includes('cleanup'),true);assert.equal(commands.at(-1).includes('/owned/build/native.test'),false);
 assert.throws(()=>maintenanceCommands({build:'/owned/build',identity:['bad',...identity.slice(1)],hashes:Array(4).fill('bad')}));
});

test('unsafe ownership record links, duplicate fields and changed root inode never authorize cleanup',{skip:process.platform!=='linux'},async t=>{
 for(const kind of ['symlink','hardlink','writable','duplicate','unknown','changed-inode']){
  const f=await fixture(t);pass(f.run('init'));const marker=join(f.root,'.ci-owner.json');const raw=await readFile(marker,'utf8');
  if(kind==='symlink'){await rename(marker,join(f.base,'saved'));await symlink(join(f.base,'saved'),marker);}
  if(kind==='hardlink')await link(marker,join(f.base,'linked-marker'));
  if(kind==='writable')await chmod(marker,0o622);
  if(kind==='duplicate')await writeFile(marker,raw.replace('"format":1','"format":1,"format":1'));
  if(kind==='unknown')await writeFile(marker,raw.replace('"format":1','"format":1,"PRIVATE_SECRET":1'));
  if(kind==='changed-inode'){await rename(f.root,join(f.base,'old'));await mkdir(f.root,{mode:0o755});await writeFile(marker,raw,{mode:0o600});}
  refused(f.run('cleanup'));assert.ok((await stat(f.root)).isDirectory());
 }
});
test('unsafe partial stage is rejected before deleting any recognized copy',{skip:process.platform!=='linux'},async t=>{
 for(const kind of ['symlink','hardlink','oversized','fifo']){
  const f=await fixture(t);pass(f.run('init'));const good=join(f.root,'.runner/native.test');await writeFile(good,'keep',{mode:0o700});const bad=join(f.root,'.runner/credential-v1');
  if(kind==='symlink')await symlink('/dev/zero',bad);
  if(kind==='hardlink'){await writeFile(bad,'unsafe',{mode:0o555});await link(bad,join(f.base,'other'));}
  if(kind==='oversized'){const fd=await open(bad,'wx',0o555);await fd.truncate(1048577);await fd.close();}
  if(kind==='fifo')assert.equal(spawnSync('/usr/bin/mkfifo',[bad]).status,0);
  refused(f.run('cleanup'));assert.equal(await readFile(good,'utf8'),'keep');
 }
});
async function stageProbe(t,f){
 const binary=join(f.base,'probe');const c='#include <stdio.h>\n#include <string.h>\n#include <stdlib.h>\nint main(int n,char **v){if(n!=4||strcmp(v[1],"-test.run=^TestDisposableRootCleanup$")||strcmp(v[2],"-test.timeout=25s")||strcmp(v[3],"-test.v")||getenv("PRIVATE_SECRET"))return 7;puts("FIXED_CLEANUP_EXECUTED");return 0;}';
 const built=spawnSync('cc',['-x','c','-','-o',binary],{input:c,encoding:'utf8'});assert.equal(built.status,0,built.stderr);
 const hash=createHash('sha512').update(await readFile(binary)).digest('base64');pass(f.run('init',Array(4).fill(hash)));
 for(const [name,mode]of [['.runner/native.test',0o700],['.runner/credential-v1',0o555],['.runner/credential-v2',0o555],['observer',0o555]]){await copyFile(binary,join(f.root,name));await chmod(join(f.root,name),mode);}
 return Array(4).fill(hash);
}
test('verified hash persists and only exact held ELF receives fixed cleanup argv and environment',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);const hashes=await stageProbe(t,f);pass(f.run('verify',hashes));const marker=JSON.parse(await readFile(join(f.root,'.ci-owner.json'),'utf8'));assert.equal(marker.phase,'verified');assert.equal(marker.nativeSha512,hashes[0]);await mkdir(join(f.root,'appimage'));
 const result=f.run('cleanup');pass(result);assert.equal(result.stdout,'FIXED_CLEANUP_EXECUTED\n');
});
test('bad expected hash or corrupted verified ELF cannot execute transaction cleanup',{skip:process.platform!=='linux'},async t=>{
 for(const kind of ['bad-hash','changed-elf']){
  const f=await fixture(t);const hashes=await stageProbe(t,f);
  if(kind==='bad-hash'){refused(f.run('verify',[Buffer.alloc(64).toString('base64'),...hashes.slice(1)]));assert.equal(JSON.parse(await readFile(join(f.root,'.ci-owner.json'),'utf8')).phase,'initialized');}
  else {pass(f.run('verify',hashes));await writeFile(join(f.root,'.runner/native.test'),'PRIVATE_SECRET');}
  await mkdir(join(f.root,'appimage'));refused(f.run('cleanup'));assert.ok((await stat(join(f.root,'.runner/native.test'))).isFile());
 }
});
test('foreign preexisting root and malformed role arguments never get adopted',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);await mkdir(f.root,{mode:0o755});await writeFile(join(f.root,'foreign'),'keep');refused(f.run('init'));assert.equal(await readFile(join(f.root,'foreign'),'utf8'),'keep');
 for(const [role,args]of [['init',['/arbitrary']],['verify',[]],['--root=/arbitrary',[]]])refused(f.run(role,args));
});

test('cleanup and reporting errors preserve the original assertion and attempt every finalizer',async()=>{
 const {preserveMaintenanceFailure}=await import('./support/linux-sandbox-maintenance.mjs');const original=Error('original assertion');const calls=[];
 const result=await preserveMaintenanceFailure(original,[()=>{calls.push('cleanup');throw Error('cleanup');},()=>{calls.push('report');throw Error('report');},()=>calls.push('remove')]);
 assert.equal(result,original);assert.deepEqual(calls,['cleanup','report','remove']);const first=Error('first cleanup');assert.equal(await preserveMaintenanceFailure(undefined,[()=>{throw first;}]),first);
});
test('masked writable ACL on a staged file is rejected without deleting another copy',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('init'));const path=join(f.root,'.runner/native.test');await writeFile(path,'keep',{mode:0o600});
 const acl='0200000001000600ffffffff02000200feff000004000000ffffffff10000000ffffffff20000000ffffffff';
 const result=spawnSync('/usr/bin/python3',['-I','-S','-c','import os,sys;os.setxattr(sys.argv[1],"system.posix_acl_access",bytes.fromhex(sys.argv[2]))',path,acl],{encoding:'utf8'});assert.equal(result.status,0,result.stderr);
 refused(f.run('cleanup'));assert.equal(await readFile(path,'utf8'),'keep');
});

test('real non-SUID C holder keeps its inode and executes only the fixed ELF with exact credential framing',{skip:process.platform!=='linux'},async t=>{
 const base=await mkdtemp(join(tmpdir(),'maintenance-c-frame-'));t.after(()=>rm(base,{recursive:true,force:true}));const app=join(base,'appimage');await mkdir(app);const helpers=new URL('./fixtures/linux-sandbox-maintenance/',import.meta.url);
 const compile=args=>{const result=spawnSync('cc',['-std=c11','-Wall','-Wextra','-Werror','-O2',...args],{encoding:'utf8'});assert.equal(result.status,0,result.stderr);};
 compile(['-DDILLA_GENERATION=1','-DDILLA_CI_LEASE_UID='+process.getuid(),'-DDILLA_CI_LEASE_GID='+process.getgid(),new URL('credentials.c',helpers).pathname,'-o',join(app,'chrome-sandbox')]);
 compile(['-DDILLA_CI_ROOT='+JSON.stringify(base),'-DDILLA_CI_LEASE_UID='+process.getuid(),'-DDILLA_CI_LEASE_GID='+process.getgid(),new URL('held-fd.c',helpers).pathname,'-o',join(base,'observer')]);
 const bridge='import os,fcntl;fd=os.open('+JSON.stringify(join(base,'lease'))+',os.O_RDONLY|os.O_CREAT,0o600);fcntl.flock(fd,fcntl.LOCK_EX);os.dup2(fd,3,inheritable=True);os.set_inheritable(3,True);os.execve('+JSON.stringify(join(base,'observer'))+',["observer"],{"LANG":"C","LC_ALL":"C"})';
 const result=spawnSync('/usr/bin/python3',['-I','-S','-c',bridge],{input:'EQ',encoding:'utf8',timeout:3000});assert.equal(result.status,0,result.stderr);const lines=result.stdout.trim().split('\n');assert.equal(lines.length,2);const held=lines[0].split(' '),cred=lines[1].split(' ');
 assert.equal(held.length,11);assert.equal(held[0],'HELD');assert.equal(cred.length,9);assert.equal(cred[0],'CRED');assert.deepEqual(cred.slice(1,4),Array(3).fill(String(process.getuid())));assert.deepEqual(cred.slice(4,7),Array(3).fill(String(process.getgid())));assert.equal(cred[8],'1');assert.deepEqual(held.slice(3,9),[...Array(3).fill(String(process.getuid())),...Array(3).fill(String(process.getgid()))]);
 const s=await stat(join(app,'chrome-sandbox'),{bigint:true});assert.equal(held[1],String(s.dev));assert.equal(held[2],String(s.ino));assert.equal(s.mode&0o6000n,0n);
 const rejected=spawnSync('/usr/bin/python3',['-I','-S','-c',bridge],{input:'X',encoding:'utf8',timeout:3000});assert.equal(rejected.status,2);assert.doesNotMatch(rejected.stdout,/CRED/);
 const actor=spawn('/usr/bin/python3',['-I','-S','-c',bridge],{stdio:['pipe','pipe','pipe']});let output='';actor.stdout.on('data',x=>{output+=x.toString();});const exit=new Promise(done=>actor.once('exit',done));t.after(()=>{actor.stdin.end();if(actor.exitCode===null&&actor.signalCode===null)actor.kill('SIGKILL');});
 const ready=async text=>{for(let i=0;i<100;i++){if(output.includes(text))return;await new Promise(done=>setTimeout(done,10));}assert.fail('owned C lease acknowledgement missing');};
 const exclusive=()=>spawnSync('/usr/bin/flock',['--exclusive','--nonblock',join(base,'lease'),'/usr/bin/true']).status;
 await ready('HELD');assert.equal(exclusive(),1);actor.stdin.write('E');await ready('CRED');assert.equal(exclusive(),1,'normal helper execution must retain lease');actor.stdin.end('Q');assert.equal(await exit,0);assert.equal(exclusive(),0);

});

test('late metadata change between lstat and open holds without deleting the changed inode',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('init'));const path=join(f.root,'.runner/native.test');await writeFile(path,'keep',{mode:0o700});
 const boundary='    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=parent)';assert.equal(f.code.split(boundary).length,2);
 const code=f.code.replace(boundary,'    if name == "native.test":\n        os.chmod(name, 0o722, dir_fd=parent)\n'+boundary);
 const result=spawnSync('/usr/bin/python3',['-I','-S','-c',code,'cleanup',...identity],{encoding:'utf8',timeout:3000});
 refused(result);assert.equal(await readFile(path,'utf8'),'keep');
});

function startedProgram(t,f,role,code=f.code,args=[],withTimeout=false){
 const program=withTimeout?'/usr/bin/timeout':'/usr/bin/python3';const argv=['-I','-S','-c',code,role,...identity,...args];
 const child=spawn(program,withTimeout?['--signal=TERM','--kill-after=1s','5s','/usr/bin/python3',...argv]:argv,{stdio:['pipe','pipe','pipe'],env:{PATH:'/untrusted',PRIVATE_SECRET:'not visible'}});
 child.stdin.on('error',()=>{});let output='';child.stdout.on('data',x=>{output+=x.toString();assert.ok(output.length<4096);});
 const exit=new Promise((done,reject)=>{child.once('error',reject);child.once('exit',code=>done(code));});
 t.after(()=>{child.stdin.end();if(child.exitCode===null&&child.signalCode===null)child.kill('SIGKILL');});
 return {child,exit,ready:async text=>{for(let i=0;i<100;i++){if(output.includes(text))return;await new Promise(done=>setTimeout(done,10));}assert.fail('owned fixture acknowledgement absent');}};
}
test('cleanup holds while an actual staged-copy actor retains the lifecycle lease',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('init'));const actor=startedProgram(t,f,'stage-native');actor.child.stdin.write('partial');
 for(let i=0;i<100;i++){try{if((await stat(join(f.root,'.runner/native.test'))).size>0)break;}catch{}await new Promise(done=>setTimeout(done,10));}
 const marker=await readFile(join(f.root,'.ci-owner.json'),'utf8');refused(f.run('cleanup'));assert.equal(await readFile(join(f.root,'.ci-owner.json'),'utf8'),marker);assert.equal(JSON.parse(await readFile(join(f.base,'lifecycle/state.json'),'utf8')).phase,'active');assert.equal(await readFile(join(f.root,'.runner/native.test'),'utf8'),'partial');
 actor.child.stdin.end();assert.notEqual(await actor.exit,0);pass(f.run('cleanup'));await assert.rejects(stat(f.root),{code:'ENOENT'});assert.equal(JSON.parse(await readFile(join(f.base,'lifecycle/state.json'),'utf8')).phase,'closed');refused(f.run('init'));refused(f.run('stage-native'));
});
test('launcher cancellation cannot release the lease held by its surviving controlled descendant',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);const hashes=await stageProbe(t,f);pass(f.run('verify',hashes));
 const boundary='            os.execve("/usr/bin/unshare", ["unshare", "--mount", "--propagation", "private", "--", "/usr/lib/dilla-sandbox-maintenance-ci/.runner/native.test", "-test.run=^TestDisposableRootTransaction$", "-test.timeout=4m", "-test.v"], SAFE_ENV)';assert.equal(f.code.split(boundary).length,2);
 const body='import os,sys,select,time,fcntl;assert fcntl.fcntl(3,fcntl.F_GETFL)&os.O_ACCMODE==os.O_RDONLY;fcntl.flock(3,fcntl.LOCK_EX|fcntl.LOCK_NB);pid=os.fork()\nif pid==0:\n print("DESCENDANT_READY",flush=True)\n time.sleep(3)\n os._exit(0)\nprint("LAUNCHER_READY",flush=True)\ntime.sleep(1)';
 const code=f.code.replace(boundary,'            os.execve("/usr/bin/python3", ["python3", "-I", "-S", "-c", '+JSON.stringify(body)+'], SAFE_ENV)');
 const actor=startedProgram(t,f,'run',code,[],true);await actor.ready('DESCENDANT_READY');await actor.ready('LAUNCHER_READY');actor.child.kill('SIGKILL');assert.equal(await actor.exit,null);
 refused(f.run('cleanup'));assert.ok((await stat(f.root)).isDirectory());actor.child.stdin.end();await new Promise(done=>setTimeout(done,1200));refused(f.run('cleanup'));
 let result;for(let i=0;i<100;i++){result=f.run('cleanup');if(result.status===0)break;await new Promise(done=>setTimeout(done,20));}pass(result);await assert.rejects(stat(f.root),{code:'ENOENT'});
});
test('a queued stage revalidates the closed tombstone after acquiring the lifecycle lock',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('init'));
 const boundary='            write_lifecycle(control, state)\n            cleanup(parent, root, value)';assert.equal(f.code.split(boundary).length,2);
 const cleanupCode=f.code.replace(boundary,'            write_lifecycle(control, state)\n            print("CLEANUP_CLOSED", flush=True)\n            sys.stdin.buffer.read(1)\n            raise ValueError("fixture deletion failure")\n            cleanup(parent, root, value)');
 const cleanup=startedProgram(t,f,'cleanup',cleanupCode);await cleanup.ready('CLEANUP_CLOSED');
 const waitBoundary='            deadline = time.monotonic() + LOCK_TIMEOUT';assert.equal(f.code.split(waitBoundary).length,2);
 const waitingCode=f.code.replace(waitBoundary,'            print("STAGE_WAITING", flush=True)\n'+waitBoundary);
 const queued=startedProgram(t,f,'stage-native',waitingCode);queued.child.stdin.end('late bytes');await queued.ready('STAGE_WAITING');assert.equal(queued.child.exitCode,null);
 cleanup.child.stdin.end('release');assert.notEqual(await cleanup.exit,0);assert.notEqual(await queued.exit,0);await assert.rejects(stat(join(f.root,'.runner/native.test')),{code:'ENOENT'});assert.ok((await stat(f.root)).isDirectory());pass(f.run('cleanup'));
});

test('a replacement lifecycle lock or foreign lifecycle record cannot authorize cleanup',{skip:process.platform!=='linux'},async t=>{
 for(const change of ['lock-inode','foreign-state','lock-mode']){
  const f=await fixture(t);pass(f.run('init'));const control=join(f.base,'lifecycle');
  if(change==='lock-inode'){await rename(join(control,'lock'),join(f.base,'original-lock'));await writeFile(join(control,'lock'),'',{mode:0o600});}
  if(change==='foreign-state'){const file=join(control,'state.json');const state=JSON.parse(await readFile(file,'utf8'));state.runId='999';await writeFile(file,JSON.stringify(state));}
  if(change==='lock-mode')await chmod(join(control,'lock'),0o644);
  refused(f.run('cleanup'));assert.ok((await stat(f.root)).isDirectory());
 }
});

test('cleanup before initialization leaves a closed admission tombstone that rejects a late initializer',{skip:process.platform!=='linux'},async t=>{
 const f=await fixture(t);pass(f.run('cleanup'));refused(f.run('init'));await assert.rejects(stat(f.root),{code:'ENOENT'});const state=JSON.parse(await readFile(join(f.base,'lifecycle/state.json'),'utf8'));assert.equal(state.phase,'closed');
});

test('actual failing child retains audited native assertion and syscall reason in proof and TAP without raw private output',async t=>{
 const {maintenanceCommandFailure,preserveMaintenanceFailure,captureMaintenanceFailure}=await import('./support/linux-sandbox-maintenance.mjs');
 const output='=== RUN   TestDisposableRootTransaction\n    native_root_linux_test.go:801: invalid Linux sandbox protected state\n    native_root_linux_test.go:802: open /usr/lib/dilla-sandbox-maintenance-ci/appimage/chrome-sandbox: permission denied\n--- FAIL: TestDisposableRootTransaction (0.01s)\nPRIVATE_FIXTURE_KEY=secret-private-material\n';
 const result=spawnSync(process.execPath,['-e','process.stdout.write('+JSON.stringify(output)+');process.stderr.write("SECRET_ENV=unrelated\\n");process.exit(1)'],{encoding:'utf8'});
 const original=maintenanceCommandFailure(result),proof={stage:'kernel',primaryFailure:false};assert.equal(captureMaintenanceFailure(proof,original),original);
 const base=await mkdtemp(join(tmpdir(),'native-failure-proof-'));t.after(()=>rm(base,{recursive:true,force:true}));const file=join(base,'proof.json');await writeFile(file,JSON.stringify(proof),{mode:0o600});const rendered=await readFile(file,'utf8');assert.equal(JSON.parse(rendered).primaryFailure,true);
 assert.equal(original.maintenanceDiagnostic.exitCode,1);assert.match(rendered,/invalid Linux sandbox protected state/);assert.match(rendered,/permission denied/);assert.match(rendered,/801/);assert.match(rendered,/TestDisposableRootTransaction/);
 assert.doesNotMatch(rendered,/PRIVATE_FIXTURE_KEY|secret-private-material|SECRET_ENV|unrelated/);
 const actions=[];assert.equal(await preserveMaintenanceFailure(original,[()=>{actions.push('cleanup');throw Error('cleanup');},()=>{actions.push('proof');t.diagnostic(rendered);}]),original);assert.deepEqual(actions,['cleanup','proof']);
});
test('native diagnostics enforce finite output bounds, safe signal codes and explicit unknown/truncated markers',async()=>{
 const {maintenanceCommandFailure}=await import('./support/linux-sandbox-maintenance.mjs');
 for(const result of [{status:null,signal:'SIGTERM',error:{code:'ETIMEDOUT'},stdout:'x'.repeat(100000),stderr:'secret'}, {status:1,signal:'secret-signal',error:{code:'SECRET'},stdout:'native_root_linux_test.go:99: PRIVATE_SECRET\n'},{status:1,stdout:'native_root_linux_test.go:12: operation not permitted\n'.repeat(5000)}]){
  const value=maintenanceCommandFailure(result).maintenanceDiagnostic,raw=JSON.stringify(value);assert.ok(Buffer.byteLength(raw)<=65536);assert.doesNotMatch(raw,/PRIVATE_SECRET|SECRET|secret-signal/);assert.equal(value.format,1);assert.ok(value.entries.length<=128);assert.equal(typeof value.unknownLines,'number');assert.match(value.unknownSha256,/^[a-f0-9]{64}$/);
 }
 const timed=maintenanceCommandFailure({status:null,signal:'SIGTERM',error:{code:'ETIMEDOUT'},stdout:'x'.repeat(100000)}).maintenanceDiagnostic;assert.equal(timed.truncated,true);assert.equal(timed.signal,'SIGTERM');assert.equal(timed.spawnError,'ETIMEDOUT');
});

test('native failure output audit covers every current fixed assertion without admitting arbitrary text',async()=>{
 const {AUDITED_NATIVE_REASONS,maintenanceCommandFailure}=await import('./support/linux-sandbox-maintenance.mjs');
 const source=await readFile(new URL('../../../internal/desktopsandbox/native_root_linux_test.go',import.meta.url),'utf8');
 assert.doesNotMatch(source,/t\.(?:Fatalf|Errorf|Logf|Log)\(/);
 for(const match of source.matchAll(/t\.Fatal\("([^"\n]*)"/g))assert.ok(AUDITED_NATIVE_REASONS.has(match[1]),'unaudited fixed assertion');
 const value=maintenanceCommandFailure({status:1,stdout:'native_root_linux_test.go:99: '+('fixed crash point not reached '.repeat(100))+'permission denied\nnative_root_linux_test.go:100: SECRET_ENV=private\n'}).maintenanceDiagnostic;
 assert.equal(value.entries[0].kind,'unrecognized-assertion');assert.equal(value.entries[1].kind,'unrecognized-assertion');assert.doesNotMatch(JSON.stringify(value),/SECRET_ENV|private/);
});
