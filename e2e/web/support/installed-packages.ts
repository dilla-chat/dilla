import { build } from 'esbuild';
import { createRequire } from 'node:module';
import { createServer, type Server } from 'node:http';
import { createHash, generateKeyPairSync, randomBytes, sign } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { cp, mkdir, mkdtemp, open, readFile, readdir, realpath, rm, writeFile, chmod } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import type { Artifact, Release } from '../../../packages/desktop/src/update.ts';

const repository = resolve(import.meta.dirname, '../../..');
const desktop = join(repository, 'packages/desktop');
const require = createRequire(join(repository, 'package.json'));
const { getCurrentFuseWire, FuseV1Options, FuseState } = require('@electron/fuses');
const hash = (bytes: Uint8Array) => createHash('sha512').update(bytes).digest('base64');
export const INSTALLED_FUSES = Object.freeze({ runAsNode: false, enableCookieEncryption: true,
  enableNodeOptionsEnvironmentVariable: false, enableNodeCliInspectArguments: false,
  enableEmbeddedAsarIntegrityValidation: true, onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false });
interface FixtureIdentity { product: string; profile: string; reports: string; scheme: string; version: string }
export function fixtureWindowsInstallationSource(source: string, appId: string): string {
  let replaced = 0;
  const compiled = source.replace(/description\.appId\s*!==\s*'dev\.dilla\.desktop'/g, () => {
    replaced++; return `description.appId !== ${JSON.stringify(appId)}`;
  });
  if (replaced !== 2) throw new Error('Installed account fixture Windows identity boundary changed');
  return compiled;
}
function replaceBoundary(source: string, boundary: string, replacement: string): string {
  if (source.split(boundary).length !== 2) throw new Error(`Installed account fixture boundary changed: ${boundary}`);
  return source.replace(boundary, replacement);
}

/** Compiled only into disposable installed fixtures. Events are sanitized before
 * the bounded private write; no request/response payload is inspected. */
export function fixtureStartupDiagnosticsSource(): string {
  return `
let __accountSequence=0;
const __accountRoute=(input:any)=>{try{const p=new URL(typeof input==='string'?input:input.url).pathname;
 if(p==='/v1/instance')return'instance';if(p==='/v1/instance/limits')return'limits';
 if(/^\\/v1\\/sessions?(?:\\/|$)/.test(p))return'session';if(p.includes('/factors'))return'factor';
 if(p.includes('/backup'))return'backup';if(p.includes('/devices'))return'device';if(p.startsWith('/v1/accounts'))return'account';
 if(p.startsWith('/v1/'))return'other-api';if(p.startsWith('/assets/'))return p.endsWith('.wasm')?'asset-wasm':p.endsWith('.js')?'asset-script':p.endsWith('.css')?'asset-style':'other-asset';return'other';}catch{return'other';}};
const __accountCode=(error:any)=>{const name=typeof error==='string'?error:typeof error?.message==='string'?error.message:'';
 const known:{[key:string]:number}={ERR_FAILED:-2,ERR_ABORTED:-3,ERR_TIMED_OUT:-7,ERR_ACCESS_DENIED:-10,ERR_BLOCKED_BY_CLIENT:-20,ERR_CONNECTION_CLOSED:-100,ERR_CONNECTION_RESET:-101,ERR_CONNECTION_REFUSED:-102,ERR_NAME_NOT_RESOLVED:-105,ERR_INTERNET_DISCONNECTED:-106,ERR_CONNECTION_TIMED_OUT:-118};
 const token=/\\b(ERR_[A-Z_]+)\\b/.exec(name.slice(0,512));return token?known[token[1]]??0:0;};
function __accountEmit(stage:string,url:any='',method:any='OTHER',status=0,code=0,elapsedMs=0){
 if(__accountRole!=='ordinary'||__accountSequence>=96)return;
 const event={seq:__accountSequence+1,stage,route:__accountRoute(url),method:['GET','POST','PUT','PATCH','DELETE','HEAD','OPTIONS'].includes(method)?method:'OTHER',status:Number.isInteger(status)&&status>=100&&status<=599?status:0,code:Number.isInteger(code)&&code>=-2147483648&&code<=2147483647?code:0,elapsedMs:Math.max(0,Math.min(600000,Math.floor(elapsedMs)))};
 let fd:number|undefined;try{const path=__accountReports+'/startup-'+app.getVersion()+'.jsonl';let before;try{before=__accountFS.lstatSync(path);if(!before.isFile()||before.isSymbolicLink())return;}catch(e:any){if(e.code!=='ENOENT')return;}
 fd=__accountFS.openSync(path,__accountFS.constants.O_WRONLY|__accountFS.constants.O_APPEND|__accountFS.constants.O_CREAT|(__accountFS.constants.O_NOFOLLOW??0),0o600);
 const opened=__accountFS.fstatSync(fd);if(!opened.isFile()||(before&&(opened.ino!==before.ino||opened.dev!==before.dev)))return;
 const line=JSON.stringify(event)+'\\n';if(opened.size+Buffer.byteLength(line)>32768)return;__accountFS.writeSync(fd,line);__accountSequence++;}catch{}finally{if(fd!==undefined)try{__accountFS.closeSync(fd);}catch{}}
}
function __accountFetch(ses:any,request:any,options:any){const started=Date.now();__accountEmit('fetch-begin',request,request.method);
 try{return ses.fetch(request,options).then((response:any)=>{__accountEmit('fetch-response',request,request.method,response.status,0,Date.now()-started);return response;},(error:any)=>{__accountEmit('fetch-reject',request,request.method,0,__accountCode(error),Date.now()-started);throw error;});}
 catch(error){__accountEmit('fetch-reject',request,request.method,0,__accountCode(error),Date.now()-started);throw error;}}
function __accountObserveSession(ses:any){ses.webRequest.onCompleted((d:any)=>__accountEmit('request-complete',d.url,d.method,d.statusCode));ses.webRequest.onErrorOccurred((d:any)=>__accountEmit('request-error',d.url,d.method,0,__accountCode(d.error)));}
function __accountObserveWindow(win:any){win.webContents.on('did-finish-load',()=>__accountEmit('document-loaded'));win.webContents.on('did-fail-load',(_e:any,code:number)=>__accountEmit('load-failed','','OTHER',0,code));win.webContents.on('render-process-gone',(_e:any,d:any)=>__accountEmit('renderer-gone','','OTHER',0,d.exitCode));}
`;
}

/** Compiled fixture instrumentation only; no shipping URL/key/CLI override or
 * Node inspector. CDP controls the actual isolated bundled renderer. */
export function fixtureMainSource(source: string, cfg: FixtureIdentity): string {
  if (source.includes('ses.webRequest.onCompleted(') || source.includes('ses.webRequest.onErrorOccurred(')) throw new Error('Installed account fixture diagnostic listener boundary changed');
  const identity = `app.setName(${JSON.stringify(cfg.product)}); app.setPath('userData',${JSON.stringify(cfg.profile)});`;
  if (!source.includes("app.setName('Dilla');")) throw new Error('Installed account fixture name boundary changed');
  source = source.replaceAll("app.setName('Dilla');", identity);
  source = source.replace(identity, identity + `
const __accountFS=process.getBuiltinModule('fs'),__accountReports=${JSON.stringify(cfg.reports)};
const __accountRole=process.argv.some(v=>v.startsWith('--update-watchdog'))?'watchdog':process.argv.some(v=>v.startsWith('--update-bootstrap'))?'bootstrap':'ordinary';
${fixtureStartupDiagnosticsSource()}
let __accountStart=null;try{const s=__accountFS.readFileSync('/proc/self/stat','utf8');__accountStart=s.slice(s.lastIndexOf(')')+2).split(' ')[19];}catch{}
__accountFS.appendFileSync(__accountReports+'/processes.jsonl',JSON.stringify({pid:process.pid,role:__accountRole,start:__accountStart,version:app.getVersion(),executable:process.execPath})+'\\n');
if(__accountRole==='ordinary'){app.commandLine.appendSwitch('remote-debugging-address','127.0.0.1');app.commandLine.appendSwitch('remote-debugging-port','0');}
const __accountDialog=dialog.showMessageBox.bind(dialog);
dialog.showMessageBox=(async(...args:any[])=>{const o=args.at(-1);if(o.title==='Dilla permission')return{response:1,checkboxChecked:false};if(o.title==='Dilla is still in use'){__accountFS.writeFileSync(__accountReports+'/protected-quit.json',JSON.stringify({version:app.getVersion(),pid:process.pid}));return{response:0,checkboxChecked:false};}return(__accountDialog as any)(...args);}) as typeof dialog.showMessageBox;
dialog.showErrorBox=((title:string,content:string)=>{__accountFS.writeFileSync(__accountReports+'/native-error.json',JSON.stringify({version:app.getVersion(),title,content}));}) as typeof dialog.showErrorBox;
`);
  source = replaceBoundary(source, 'const ses = session.fromPartition(partitionFor(instance));', 'const ses = session.fromPartition(partitionFor(instance)); __accountObserveSession(ses);');
  source = replaceBoundary(source, "windows.set(instance, win); windowModes.set(instance, 'starting');", "windows.set(instance, win); windowModes.set(instance, 'starting'); __accountObserveWindow(win);");
  for (const request of ['request', 'forwarded']) source = replaceBoundary(source, `return ses.fetch(${request}, { bypassCustomProtocolHandlers: true });`, `return __accountFetch(ses, ${request}, { bypassCustomProtocolHandlers: true });`);
  source = replaceBoundary(source, 'callback({ cancel });', "if(cancel)__accountEmit('request-blocked',details.url,details.method,0,-20); callback({ cancel });");
  source = replaceBoundary(source, "const allowLoopback = !app.isPackaged && process.argv.includes('--allow-loopback');", 'const allowLoopback = true;');
  source = replaceBoundary(source, 'const updater = createUpdater(state => {', `const updater = createUpdater(state => { __accountFS.writeFileSync(__accountReports+'/update-'+app.getVersion()+'.json',JSON.stringify({pid:process.pid,version:app.getVersion(),state}));`);
  return source.replaceAll("app.setAsDefaultProtocolClient('dilla'", `app.setAsDefaultProtocolClient(${JSON.stringify(cfg.scheme)}`);
}

export async function assertInstalledFuses(binary: string): Promise<void> {
  const wire = await getCurrentFuseWire(binary);
  for (const key of [FuseV1Options.RunAsNode, FuseV1Options.EnableNodeOptionsEnvironmentVariable, FuseV1Options.EnableNodeCliInspectArguments, FuseV1Options.GrantFileProtocolExtraPrivileges]) if (wire[key] !== FuseState.DISABLE) throw new Error('Installed account fixture weakened an Electron fuse');
  for (const key of [FuseV1Options.EnableCookieEncryption, FuseV1Options.EnableEmbeddedAsarIntegrityValidation, FuseV1Options.OnlyLoadAppFromAsar]) if (wire[key] !== FuseState.ENABLE) throw new Error('Installed account fixture lacks an Electron fuse');
}
interface Package { artifact: string; executable: string; app?: string; manifest?: string; sparkleSignature?: string }
export interface InstalledPackages { root: string; profile: string; reports: string; product: string; installed: string; old: Package; next: Package; setNext(): Promise<void>; dispose(): Promise<void> }
export async function createInstalledFixtureRoot(parent = tmpdir()): Promise<string> {
  // Native packaging rejects aliased program trees. macOS /var and Windows
  // short TEMP names must resolve before identity, profile and cleanup paths.
  return realpath(await mkdtemp(join(parent, 'dilla-account-installed-')));
}

/** Full production main, bundled UI and core, with private throwaway trust and
 * transport constants compiled into these two acceptance applications only. */
export async function createInstalledPackages(instance: string): Promise<InstalledPackages> {
  const root = await createInstalledFixtureRoot(), id = randomBytes(6).toString('hex'), product = 'DillaAccountAcceptance' + id;
  const appId = process.platform === 'darwin' ? 'dev.dilla.desktop.update-acceptance' : 'dev.dilla.desktop.account-acceptance.' + id;
  const profile = process.platform === 'win32' ? join(process.env.APPDATA!, product) : join(root, 'profile'), reports = join(root, 'reports'), scheme = 'dilla-account-' + id;
  const keys = generateKeyPairSync('ed25519'), publicKey = keys.publicKey.export({ type: 'spki', format: 'pem' }).toString(), sparkleKeys = generateKeyPairSync('ed25519');
  const sparklePublicKey = sparkleKeys.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('base64');
  let server: Server | undefined, metadata: Buffer, signature = '', current: Package | undefined, archive: Buffer | undefined, feed = '';
  const log = await open(join(root, 'package.log'), 'a');
  try {
    await mkdir(profile, { recursive: true }); await mkdir(reports);
    await writeFile(join(profile, 'desktop.json'), JSON.stringify({ version: 1, instances: [instance], autostart: false }));
    server = createServer((request, response) => {
      if (request.url === '/release.json') response.end(metadata);
      else if (request.url === '/release.json.sig') response.end(signature);
      else if (request.url === '/artifact' || request.url === '/DillaUpdateAcceptance.zip') response.end(archive);
      else if (request.url === '/appcast.xml') { response.setHeader('content-type', 'application/xml'); response.end(feed); }
      else if (request.url === '/manifest' && current?.manifest) void readFile(current.manifest).then(bytes => response.end(bytes), () => { response.writeHead(500); response.end(); });
      else { response.writeHead(404); response.end(); }
    });
    await new Promise<void>((done, fail) => { server!.once('error', fail); server!.listen(process.platform === 'darwin' ? 18849 : 0, '127.0.0.1', done); });
    const origin = 'http://127.0.0.1:' + (server.address() as { port: number }).port;
    const sourceMain = await readFile(join(desktop, 'src/main.ts'), 'utf8'), sourceUpdater = await readFile(join(desktop, 'src/updater.ts'), 'utf8');
    if (process.platform === 'darwin') execFileSync(process.execPath, [join(desktop, 'scripts/build-sparkle.mjs'), '--acceptance'], { stdio: ['ignore', log.fd, log.fd] });
    const pack = async (version: string): Promise<Package> => {
      const project = join(root, 'project-' + version), out = join(root, 'out-' + version), dist = join(project, 'dist');
      await mkdir(dist, { recursive: true }); await cp(join(desktop, 'dist'), dist, { recursive: true });
      await rm(join(dist, 'client'), { recursive: true, force: true }); await cp(join(repository, 'packages/web/dist'), join(dist, 'client'), { recursive: true });
      await writeFile(join(project, 'package.json'), JSON.stringify({ name: 'dilla-account-acceptance-' + id, version, main: 'dist/main.cjs', description: 'Installed account acceptance', author: 'Dilla', license: 'AGPL-3.0-or-later' }));
      await build({ entryPoints: [join(desktop, 'src/preload.ts')], outfile: join(dist, 'preload.cjs'), bundle: true, platform: 'node', target: 'es2022', format: 'cjs', external: ['electron'] });
      await build({ entryPoints: [join(desktop, 'src/main.ts')], outfile: join(dist, 'main.cjs'), bundle: true, platform: 'node', target: 'node24', format: 'cjs', external: ['electron'], plugins: [{ name: 'compiled-account-acceptance', setup(plugin) {
        plugin.onLoad({ filter: /[\\/]src[\\/]update-keys\.ts$/ }, () => ({ contents: `export const RELEASE_PUBLIC_KEYS=${JSON.stringify([publicKey])};`, loader: 'ts' }));
        plugin.onLoad({ filter: /[\\/]src[\\/]main\.ts$/ }, () => ({ contents: fixtureMainSource(sourceMain, { product, profile, reports, scheme, version }), loader: 'ts', resolveDir: join(desktop, 'src') }));
        plugin.onLoad({ filter: /[\\/]src[\\/]updater\.ts$/ }, () => {
          const feeds = sourceUpdater.match(/const feed = `[^`]+`;/g);
          if (feeds?.length !== 1) throw new Error('Installed account fixture feed boundary changed');
          let source = replaceBoundary(sourceUpdater, feeds[0], `const feed=${JSON.stringify(origin + '/release.json')};`);
          source = replaceBoundary(source, 'url: new URL(this.artifact.url)', `url: new URL(${JSON.stringify(origin + '/artifact')})`);
          source = replaceBoundary(source, 'boundedFetch(target.programManifest.url,1048576)', `boundedFetch(${JSON.stringify(origin + '/manifest')},1048576)`);
          source = replaceBoundary(source, 'await sparkle.prepare(release, target);', `await sparkle.prepare(release, { ...target, url: ${JSON.stringify(origin + '/DillaUpdateAcceptance.zip')} });`);
          return { contents: source, loader: 'ts', resolveDir: join(desktop, 'src') };
        });
        plugin.onLoad({ filter: /[\\/]src[\\/]windows-installation\.ts$/ }, async args => ({ contents: fixtureWindowsInstallationSource(await readFile(args.path, 'utf8'), appId), loader: 'ts', resolveDir: join(desktop, 'src') }));
      } }] });
      const base: Record<string, unknown> = { appId, productName: product, executableName: 'dilla-desktop', toolsets: require(join(desktop, 'electron-builder.cjs')).toolsets, electronVersion: '45.2.1', electronDist: join(repository, 'node_modules/electron/dist'), directories: { output: out }, files: ['dist/**', 'package.json'], asar: true, afterPack: join(desktop, 'scripts/linux-appimage-launcher.cjs'), appImage: { executableArgs: [] },
        protocols: [{ name: 'CI-owned Dilla account fixture', schemes: [scheme] }], publish: { provider: 'generic', url: origin }, artifactName: `account-${version}.\${ext}`, electronFuses: INSTALLED_FUSES,
        linux: { target: 'AppImage', category: 'Network', icon: join(desktop, 'assets/icon.png') }, win: { target: 'nsis', icon: join(desktop, 'assets/icon.png') },
        nsis: { script: join(desktop, 'build/windows-installer.nsi'), perMachine: false, oneClick: true, allowElevation: false, packElevateHelper: false, preCompressedFileExtensions: [], differentialPackage: false, deleteAppDataOnUninstall: false } };
      if (process.platform === 'darwin') Object.assign(base, { extraResources: [{ from: join(desktop, 'acceptance-bin/dilla-sparkle.node'), to: 'native/dilla-sparkle.node' }], mac: { target: 'dir', identity: '-', notarize: false,
        extraFiles: [{ from: join(desktop, 'native-bin/Sparkle.framework'), to: 'Frameworks/Sparkle.framework' }], extendInfo: { CFBundleVersion: version, SUFeedURL: origin + '/appcast.xml', SUPublicEDKey: sparklePublicKey, SURequireSignedFeed: true, SUVerifyUpdateBeforeExtraction: true, SUEnableAutomaticChecks: false, SUAutomaticallyUpdate: false, SUEnableInstallerLauncherService: true, SUEnableDownloaderService: false, NSAppTransportSecurity: { NSAllowsLocalNetworking: true } } } });
      const config = join(project, 'builder.cjs');
      await writeFile(config, `const hooks=require(${JSON.stringify(join(desktop, 'scripts/windows-builder-hooks.cjs'))}).createWindowsBuilderHooks({metadataPublicKeys:${JSON.stringify([publicKey])}});module.exports={...${JSON.stringify(base)},afterSign:hooks.afterSign,artifactBuildStarted:hooks.artifactBuildStarted};`);
      execFileSync(process.execPath, [join(repository, 'node_modules/electron-builder/cli.js'), '--projectDir', project, '--config', config, process.platform === 'linux' ? '--linux' : process.platform === 'win32' ? '--win' : '--mac', '--' + process.arch, ...(process.platform === 'darwin' ? ['--dir'] : []), '--publish', 'never'], { cwd: repository, stdio: ['ignore', log.fd, log.fd], env: { ...process.env, CSC_FOR_PULL_REQUEST: 'true' } });
      if (process.platform === 'linux') { const executable = join(out, 'linux-unpacked/dilla-desktop'); await assertInstalledFuses(executable); return { artifact: join(out, `account-${version}.AppImage`), executable }; }
      if (process.platform === 'win32') { const versions = join(out, 'win-unpacked/versions'), directories = await readdir(versions); if (directories.length !== 1) throw new Error('Expected one finalized Windows program'); const executable = join(versions, directories[0], 'dilla-desktop.exe'); await assertInstalledFuses(executable); return { artifact: join(out, `account-${version}.exe`), executable, manifest: join(out, `dilla-${version}-win-x64.manifest.json`) }; }
      const app = join(out, process.arch === 'arm64' ? 'mac-arm64' : 'mac', String(base.executableName) + '.app'), bins = await readdir(join(app, 'Contents/MacOS')); if (bins.length !== 1) throw new Error('Expected one macOS runtime');
      const executable = join(app, 'Contents/MacOS', bins[0]); await assertInstalledFuses(executable); execFileSync('codesign', ['--verify', '--deep', '--strict', app], { stdio: ['ignore', log.fd, log.fd] });
      const artifact = join(out, 'DillaUpdateAcceptance.zip'); execFileSync('ditto', ['-c', '-k', '--sequesterRsrc', '--keepParent', app, artifact]); return { artifact, executable, app, sparkleSignature: sign(null, await readFile(artifact), sparkleKeys.privateKey).toString('base64') };
    };
    const old = await pack('1.0.0'), next = await pack('1.0.1');
    const setRelease = async (value: Package, version: string) => {
      current = value; archive = await readFile(value.artifact); const now = Date.now(), extension = process.platform === 'linux' ? 'AppImage' : process.platform === 'win32' ? 'exe' : 'zip';
      const artifact: Artifact = { platform: process.platform, arch: process.arch, url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/account-${version}.${extension}`, size: archive.length, sha512: hash(archive), ...(value.sparkleSignature ? { sparkleSignature: value.sparkleSignature } : {}) };
      if (value.manifest) { const bytes = await readFile(value.manifest); artifact.programManifest = { url: `https://github.com/jonasthim/dilla/releases/download/desktop-v${version}/dilla-${version}-win-x64.manifest.json`, size: bytes.length, sha512: hash(bytes) }; }
      const release: Release = { format: 1, version, channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [artifact] };
      metadata = Buffer.from(JSON.stringify(release)); signature = sign(null, metadata, keys.privateKey).toString('base64');
      if (process.platform === 'darwin') { const { appcast, signedAppcast } = require(join(desktop, 'scripts/feed-format.mjs')); feed = signedAppcast(appcast(release, { ...artifact, url: origin + '/DillaUpdateAcceptance.zip' }, value.sparkleSignature!), sparkleKeys.privateKey); }
    };
    await setRelease(old, '1.0.0');
    let installed: string;
    if (process.platform === 'linux') { await mkdir(join(root, 'Applications')); installed = join(root, 'Applications/Dilla.AppImage'); await cp(old.artifact, installed); await chmod(installed, 0o700); }
    else if (process.platform === 'win32') { execFileSync(old.artifact, ['/S'], { timeout: 120000, stdio: ['ignore', log.fd, log.fd] }); installed = join(process.env.LOCALAPPDATA!, 'Programs', product, 'Dilla.exe'); }
    else { await mkdir(join(root, 'Applications')); const app = join(root, 'Applications', product + '.app'); execFileSync('ditto', [old.app!, app]); installed = join(app, 'Contents/MacOS', (await readdir(join(app, 'Contents/MacOS')))[0]); }
    return { root, profile, reports, product, installed, old, next, setNext: () => setRelease(next, '1.0.1'), dispose: async () => {
      if (process.platform === 'win32') { try { execFileSync(join(process.env.LOCALAPPDATA!, 'Programs', product, 'Uninstall ' + product + '.exe'), ['/S'], { timeout: 120000, stdio: ['ignore', log.fd, log.fd] }); } catch { /* Keep diagnostic files when a safe uninstall refuses. */ } }
      await new Promise<void>(done => server!.close(() => done())); await log.close();
      if (!process.env.DILLA_KEEP_ACCOUNT_FIXTURE) { await rm(root, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); if (process.platform === 'win32') await rm(profile, { recursive: true, force: true, maxRetries: 10, retryDelay: 100 }); }
    } };
  } catch (error) { await log.close(); if (server) await new Promise<void>(done => server!.close(() => done())); const detail = (await readFile(join(root, 'package.log'), 'utf8')).slice(-6000); throw new Error(`Installed account fixture setup failed (${root}): ${String(error)}\n${detail}`); }
}
