// Inspect the trusted CI-built AppImage as an unprivileged packaging operation.
// Extraction is inspection only: it is not evidence of mounted startup.
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstat, mkdir, mkdtemp, open, readFile, readlink, rm, writeFile } from 'node:fs/promises';
import { dirname, isAbsolute, join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { getCurrentFuseWire, FuseState, FuseV1Options } from '@electron/fuses';
import { extractFile } from '@electron/asar';
import { buildCapsule, renderLauncher, maintainerConfiguration, parsePublicConfiguration, assertDedicatedKeys } from './linux-sandbox-package.mjs';

const require = createRequire(import.meta.url);
const { PRODUCTION_APP_RUN_SCRIPT } = require('./linux-appimage-launcher.cjs');
const repo = resolve(import.meta.dirname, '../../..');
const sha = b => createHash('sha512').update(b).digest('base64');
const invalid = reason => { throw new Error('Invalid final AppImage: ' + reason); };
const expectedFiles = ['AppRun','chrome-sandbox','dilla-desktop','dilla-sandbox-launcher','dev.dilla.desktop.desktop','resources/app.asar','resources/linux-sandbox-build.json'];

async function readRegular(root, name, max) {
 const parts = name.split('/'); let path = root;
 for (let n=0; n<parts.length; n++) {
  path=join(path,parts[n]); const stat=await lstat(path);
  if (stat.isSymbolicLink() || (n<parts.length-1 ? !stat.isDirectory() : !stat.isFile() || stat.nlink!==1 || stat.size<1 || stat.size>max)) invalid('unsafe or oversized ' + name);
 }
 return readFile(path);
}
function decodeCapsule(raw, config) {
 if (raw.length < 32 || raw.length > 48*1024*1024 || raw.subarray(0,8).toString('binary')!=='DLSBOX1\0' || raw.readUInt32LE(12)!==0) invalid('capsule framing');
 const ml=raw.readUInt32LE(8), nl=Number(raw.readBigUInt64LE(16)), hl=Number(raw.readBigUInt64LE(24));
 if (!ml || ml>65536 || !Number.isSafeInteger(nl) || nl<1 || nl>32*1024*1024 || !Number.isSafeInteger(hl) || hl<1 || hl>8*1024*1024 || 32+ml+nl+hl!==raw.length) invalid('capsule bounds');
 const manifest=raw.subarray(32,32+ml), maintainer=raw.subarray(32+ml,32+ml+nl), helper=raw.subarray(32+ml+nl);
 if (maintainer.length<20 || !maintainer.subarray(0,4).equals(Buffer.from([127,69,76,70])) || maintainer[4]!==2 || maintainer[5]!==1 || maintainer.readUInt16LE(18)!==62 || !maintainer.includes(Buffer.from(maintainerConfiguration(config)))) invalid('maintainer ELF or public configuration');
 const regenerated=buildCapsule(config,maintainer,helper);
 if (!regenerated.capsule.equals(raw)) invalid('component tuple or manifest');
 return {manifest, maintainer, regenerated};
}

async function verifyLauncher(actual, config, regenerated, version) {
 const temp=await mkdtemp(join(tmpdir(),'dilla-launcher-verification-'));
 try {
  const source=join(temp,'main.go'), overlay=join(temp,'overlay.json'), output=join(temp,'launcher');
  await writeFile(source,renderLauncher(config,regenerated,version));
  await writeFile(overlay,JSON.stringify({Replace:{[join(repo,'packages/desktop/native/linux-sandbox/launcher/main.go')]:source}}));
  const env={...process.env,GOOS:'linux',GOARCH:'amd64',GOAMD64:'v1',GOENV:'off',GOWORK:'off',GOFLAGS:'',GOEXPERIMENT:'',GOTOOLCHAIN:config.goVersion,CGO_ENABLED:'1'};
  execFileSync('go',['build','-mod=readonly','-trimpath','-buildvcs=false','-overlay',overlay,'-o',output,'./packages/desktop/native/linux-sandbox/launcher'],{cwd:repo,env,stdio:'ignore',timeout:120000});
  if (!actual.equals(await readFile(output))) invalid('launcher bytes differ from generated source');
 } finally { await rm(temp,{recursive:true,force:true}); }
}

export async function inspectExtracted({directory, capsule, manifest, configurationFile}) {
 if (configurationFile !== undefined && (typeof configurationFile !== 'string' || !isAbsolute(configurationFile))) invalid('public configuration path');
 const config=parsePublicConfiguration(await readFile(configurationFile ?? join(repo,'packages/desktop/linux-sandbox-component.json'),'utf8'));
 assertDedicatedKeys(config,parsePublicConfiguration(await readFile(join(repo,'packages/desktop/release-trust.json'),'utf8')));
 if (!Buffer.isBuffer(capsule) || !Buffer.isBuffer(manifest)) invalid('missing sidecars');
 const parsed=decodeCapsule(capsule,config);
 if (!parsed.manifest.equals(manifest)) invalid('detached manifest');
 const root=resolve(directory), rootStat=await lstat(root);
 if (!rootStat.isDirectory() || rootStat.isSymbolicLink()) invalid('extraction root');
 const [run,elf,launcher,desktop,asar,proofRaw]=await Promise.all([
  readRegular(root,'AppRun',65536), readRegular(root,'dilla-desktop',1024**3), readRegular(root,'dilla-sandbox-launcher',128*1024**2), readRegular(root,'dev.dilla.desktop.desktop',65536), readRegular(root,'resources/app.asar',1024**3), readRegular(root,'resources/linux-sandbox-build.json',4096)
 ]);
 const link=join(root,'chrome-sandbox'), linkStat=await lstat(link);
 if (!linkStat.isSymbolicLink() || await readlink(link)!=='/usr/lib/dilla/appimage/chrome-sandbox') invalid('helper symlink');
 if (!run.equals(Buffer.from(PRODUCTION_APP_RUN_SCRIPT))) invalid('AppRun');
 const lines=desktop.toString('utf8').split(/\r?\n/).filter(x=>x.startsWith('Exec='));
 if (lines.length!==1 || lines[0]!=='Exec=AppRun %U') invalid('desktop Exec');
 if (!launcher.includes(Buffer.from(capsule.toString('base64'))) || config.publicKeys.some(key=>!launcher.includes(Buffer.from(key)))) invalid('embedded capsule or public key');
 const pkg=JSON.parse(extractFile(join(root,'resources/app.asar'),'package.json',false));
 await verifyLauncher(launcher,config,parsed.regenerated,pkg.version);
 const wire=await getCurrentFuseWire(join(root,'dilla-desktop'));
 for (const [name,on] of [['RunAsNode',false],['EnableCookieEncryption',true],['EnableNodeOptionsEnvironmentVariable',false],['EnableNodeCliInspectArguments',false],['EnableEmbeddedAsarIntegrityValidation',true],['OnlyLoadAppFromAsar',true],['GrantFileProtocolExtraPrivileges',false]]) if (wire[FuseV1Options[name]]!==(on?FuseState.ENABLE:FuseState.DISABLE)) invalid('Electron fuse ' + name);
 const archive=join(root,'resources/app.asar');
 for (const file of ['linux-sandbox-bootstrap.cjs','linux-sandbox-probe-arguments.cjs','linux-sandbox-probe-main.cjs','linux-sandbox-probe-preload.cjs','linux-sandbox-probe.html']) {
  const packaged=file==='linux-sandbox-bootstrap.cjs'?'main.cjs':file;
  if (!extractFile(archive,'dist/'+packaged,false).equals(await readFile(join(repo,'packages/desktop/src',file)))) invalid('ASAR bootstrap resource '+file);
 }
 if (pkg.main!=='dist/main.cjs' || extractFile(archive,'dist/main-app.cjs',false).length===0) invalid('ASAR application entry');
 const result={capsuleSha512:sha(capsule),manifestSha512:sha(manifest),electronSha512:sha(elf),asarSha512:sha(asar),embeddedCapsuleSha512:sha(capsule),embeddedManifestSha512:sha(parsed.manifest)};
 const proof=JSON.parse(proofRaw);
 if (Object.keys(proof).sort().join(',')!=='asarSha512,capsuleSha512,electronSha512,format,generation' || proof.format!==1 || proof.generation!==config.generation || proof.electronSha512!==result.electronSha512 || proof.asarSha512!==result.asarSha512 || proof.capsuleSha512!==result.capsuleSha512) invalid('embedded build proof mismatch');
 return result;
}

// The callback sees only a private, bounded extraction of fixed members. The
// image is rehashed after the callback and the owned directory always removed.
export async function withExtractedAppImage(image, callback) {
 const filename=resolve(image), before=await lstat(filename);
 if (!before.isFile() || before.isSymbolicLink() || before.nlink!==1 || before.size<1 || before.size>4*1024**3) invalid('unsafe AppImage input');
 const digestFile=async()=>{const handle=await open(filename,'r');try{const h=createHash('sha512');for await(const chunk of handle.createReadStream({autoClose:false}))h.update(chunk);return h.digest('base64');}finally{await handle.close();}};
 const imageHash=await digestFile();
 const temp=await mkdtemp(join(tmpdir(),'dilla-final-image-'));
 try {
  // Exact fixed members avoid unpacking irrelevant files and keep extraction bounded.
  for (const name of expectedFiles) execFileSync('/usr/bin/prlimit',['--fsize=1073741824:1073741824','--',filename,'--appimage-extract',name],{cwd:temp,env:{PATH:'/usr/bin:/bin',HOME:temp,LANG:'C',LC_ALL:'C'},stdio:'ignore',timeout:60000});
  const result=await callback(join(temp,'squashfs-root'));
  const after=await lstat(filename);
  if (before.dev!==after.dev || before.ino!==after.ino || before.size!==after.size || before.mtimeMs!==after.mtimeMs || await digestFile()!==imageHash) invalid('AppImage changed during extraction');
  return result;
 } finally {await rm(temp,{recursive:true,force:true});}
}

export async function inspectAppImage({image,capsuleFile,manifestFile,configurationFile}) {
 const [capsule,manifest]=await Promise.all([readRegular(dirname(resolve(capsuleFile)),resolve(capsuleFile).split('/').at(-1),48*1024**2),readRegular(dirname(resolve(manifestFile)),resolve(manifestFile).split('/').at(-1),65536)]);
 return withExtractedAppImage(image,directory=>inspectExtracted({directory,capsule,manifest,configurationFile}));
}
