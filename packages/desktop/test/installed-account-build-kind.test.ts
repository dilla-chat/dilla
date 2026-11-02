import { fixtureComponent, fixtureSandbox } from './support/fixture-update-runtime.ts';
import { assertPortableEsbuildFilter } from './support/portable-esbuild-filter.ts';
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile, mkdtemp, mkdir, writeFile, rm } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { createRequire } from 'node:module';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { createServer } from 'node:http';
import vm from 'node:vm';
import ts from 'typescript';
import { build, transform, type BuildOptions } from 'esbuild';

import { fixtureMainSource } from '../../../e2e/web/support/installed-packages.ts';

const load = createRequire(import.meta.url);
const bundleFilter = /[\\/]bundle\.ts$/;
const desktop = resolve(import.meta.dirname, '..');
async function actualAccountBuildOptions(platform: string, publicKey: string, root = '/owned-fixture', origin = 'http://127.0.0.1:12345'): Promise<BuildOptions> {
 const source = await readFile(resolve(desktop, '../../e2e/web/support/installed-packages.ts'), 'utf8');
 const parsed = ts.createSourceFile('installed-packages.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
 const options: string[] = [];
 let boundary = '';
 const visit = (node: ts.Node): void => {
  if (ts.isCallExpression(node) && node.expression.getText(parsed) === 'build' && node.arguments[0]?.getText(parsed).includes("'src/main.ts'")) options.push(node.arguments[0].getText(parsed));
  if (ts.isFunctionDeclaration(node) && node.name?.text === 'replaceBoundary') boundary = node.getText(parsed);
  ts.forEachChild(node, visit);
 };
 visit(parsed);
 assert.equal(options.length, 1, 'capture the actual installed-account main compiler invocation');
 assert.ok(boundary);
 // Evaluate the actual compiler options and fixture transport plugin closures,
 // without invoking electron-builder, a package install, server or profile boot.
 const names = ['process', 'join', 'desktop', 'dist', 'publicKey', 'installedEntrypointBanner', 'reports', 'version', 'origin', 'sourceUpdater', 'readFile', 'appId', 'fixtureMainSource', 'sourceMain', 'product', 'profile', 'scheme', 'sandboxConfig'];
 const code = await transform('(function(scope) { const {' + names.join(',') + '} = scope; ' + boundary + '\nreturn (' + options[0] + '); })', { loader: 'ts' });
 return vm.runInThisContext(code.code)({
  process: { platform }, join, desktop, dist: join(root, 'dist'), publicKey,
  installedEntrypointBanner: () => '', reports: join(root, 'reports'), version: '1.0.0',
  origin, fixtureMainSource, sourceMain: await readFile(join(desktop, 'src/main.ts'), 'utf8'), product: 'DillaAccountFixture', profile: join(root, 'profile'), scheme: 'dilla-account-fixture', readFile, appId: 'dev.dilla.desktop.account-acceptance.fixture', sourceUpdater: await readFile(join(desktop, 'src/updater.ts'), 'utf8'), sandboxConfig: fixtureComponent.config,
 });
}
test('installed account build selects AppImage authority only for Linux on every host', async () => {
 const pair = generateKeyPairSync('ed25519'), publicKey = pair.publicKey.export({ type: 'spki', format: 'pem' }).toString();
 assert.equal((await actualAccountBuildOptions('linux', publicKey)).define?.__DILLA_LINUX_INSTALL_KIND__, JSON.stringify('appimage'));
 for (const platform of ['win32', 'darwin']) assert.equal((await actualAccountBuildOptions(platform, publicKey)).define?.__DILLA_LINUX_INSTALL_KIND__, undefined);
});

test('actual installed account Linux build retains AppImage authority through authenticated download to ready', { skip: process.platform !== 'linux' ? 'requires Linux UID and private filesystem modes' : false }, async t => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-account-kind-'));
 t.after(() => rm(root, { recursive: true, force: true }));
 const pair = generateKeyPairSync('ed25519'), publicKey = pair.publicKey.export({ type: 'spki', format: 'pem' }).toString();
 const sandbox=fixtureSandbox('1.0.1'), requested:string[]=[]; let release=Buffer.alloc(0), signature='';
 const server=createServer((req,res)=>{const path=req.url ?? ''; requested.push(path); const file=path.split('/').at(-1); const bytes=path==='/release.json.sig' ? Buffer.from(signature) : path==='/release.json' ? release : path.startsWith('/sandbox-fixture/') && file?.endsWith('.sandbox.json') ? sandbox.manifest : path.startsWith('/sandbox-fixture/') && file?.endsWith('.sandbox.sig') ? sandbox.signature : path.startsWith('/sandbox-fixture/') && file?.endsWith('.sandbox') ? sandbox.capsule : undefined; if(!bytes){res.writeHead(404);res.end();return;} res.writeHead(200,{'content-length':bytes.length});res.end(bytes); });
 await new Promise<void>((done,fail)=>{server.once('error',fail);server.listen(0,'127.0.0.1',done);}); t.after(()=>new Promise<void>(done=>server.close(()=>done())));
 const origin=`http://127.0.0.1:${(server.address() as {port:number}).port}`; const options = await actualAccountBuildOptions('linux', publicKey, root, origin);
 const artifact = Buffer.from('verified acceptance AppImage bytes'), file = join(root, 'download.AppImage');
 await writeFile(file, artifact);
 const now = Date.now(); release = Buffer.from(JSON.stringify({ format: 1, assets:sandbox.assets, version: '1.0.1', channel: 'stable', publishedAt: new Date(now - 1000).toISOString(), expiresAt: new Date(now + 3600000).toISOString(), artifacts: [{
  platform: 'linux', arch: 'x64', url: 'https://github.com/jonasthim/dilla/releases/download/desktop-v1.0.1/dilla-1.0.1-linux-x64.AppImage', size: artifact.length, sha512: createHash('sha512').update(artifact).digest('base64'), linuxSandbox:sandbox.descriptor,
 }] })); signature = sign(null, release, pair.privateKey).toString('base64');
 const compiled = await build({ ...options, entryPoints: [join(desktop, 'src/updater.ts')], outfile: undefined, write: false,
  external: ['electron', 'electron-updater', 'electron-updater/*'] });
 const effects: string[] = [], phases: string[] = [];
 class AppImageUpdater extends EventEmitter {
  constructor() { super(); effects.push('AppImageUpdater'); }
  setFeedURL() {}
  async checkForUpdates() { effects.push('check'); return { isUpdateAvailable: true }; }
  async downloadUpdate() { effects.push('download'); return [file]; }
 }
 const app = Object.assign(new EventEmitter(), { isPackaged: true, getVersion: () => '1.0.0', getPath: () => root });
 const module = { exports: {} as any };
 vm.runInNewContext(compiled.outputFiles![0].text, { module, exports: module.exports,
  require(name: string) {
   if (name === 'electron') return { app };
   if (name === 'electron-updater') return { AppImageUpdater, NsisUpdater: class {} };
   if (name.includes('/providers/Provider')) return { Provider: class {} };
   return load(name);
  },
  process: { platform: 'linux', arch: 'x64', versions: {}, env: { APPIMAGE: '/owned-fixture/Dilla.AppImage' }, cwd: () => process.cwd(), getuid: () => process.getuid!() },
  fetch,
  Buffer, URL, TextDecoder, AbortSignal, Error,
 });
 const updater = module.exports.createUpdater((state: { phase: string }) => phases.push(state.phase), () => assert.fail('no installation attempted'));
 await updater.check();
 assert.equal(updater.state.phase, 'ready', updater.state.message);
 assert.deepEqual(phases, ['checking', 'downloading', 'ready']);
 assert.deepEqual(effects, ['AppImageUpdater', 'check', 'download']);
 assert.deepEqual(requested, ['/release.json', '/release.json.sig', ...['','.json','.sig'].map(suffix=>`/sandbox-fixture/dilla-1.0.1-linux-x64.sandbox${suffix}`)]);
 const {readdir,readFile:readCached}=await import('node:fs/promises'), cacheRoot=join(root,'sandbox-sidecars'), dirs=await readdir(cacheRoot); assert.equal(dirs.length,1);
 for (const [name,expected] of [['capsule',sandbox.capsule],['manifest',sandbox.manifest],['signature',sandbox.signature]] as const) assert.deepEqual(await readCached(join(cacheRoot,dirs[0],name)),expected);
});

for (const platform of ['linux', 'win32', 'darwin']) test('compiled installed account main shares actual lifecycle diagnostics across initializer and desktop: ' + platform, async t => {
 const root = await mkdtemp(join(tmpdir(), 'dilla-account-scope-'));
 t.after(() => rm(root, { recursive: true, force: true }));
 await mkdir(join(root, 'reports')); await mkdir(join(root, 'profile'));
 const instance = 'http://127.0.0.1:12345/';
 await writeFile(join(root, 'profile/desktop.json'), JSON.stringify({ version: 1, instances: [instance], autostart: false }));
 const options = await actualAccountBuildOptions(platform, generateKeyPairSync('ed25519').publicKey.export({ type: 'spki', format: 'pem' }).toString(), root);
 // Substitute native OS entry verification and bundle resources only; compile
 // the actual transformed main and all its injected diagnostic callbacks.
 const compiled = await build({ ...options, write: false, outfile: undefined, external: ['electron', 'electron-updater', 'electron-updater/*'], plugins: [{ name: 'native-os-boundaries', setup(plugin) {
  plugin.onLoad({ filter: bundleFilter }, () => ({ contents: 'export async function loadBundle(){return {}}; export function bundledResponse(){return undefined}', loader: 'ts' }));
 }, }, ...(options.plugins ?? [])] });
 let requestHandler: ((request: Request) => Promise<Response>) | undefined;
 let requests = 0, windows = 0, quit = false;
 const errors: string[] = [];
 const ses = Object.assign(new EventEmitter(), {
  protocol: { handle: async (_scheme: string, handler: typeof requestHandler) => { requestHandler = handler; } },
  webRequest: { onBeforeRequest() {}, onCompleted() {}, onErrorOccurred() {} },
  setPermissionRequestHandler() {}, setPermissionCheckHandler() {}, setDisplayMediaRequestHandler() {},
  fetch: async (request: Request) => { requests++; assert.equal(request.url, instance + 'v1/instance'); return new Response('{}'); },
 });
 class Window extends EventEmitter {
  webContents = Object.assign(new EventEmitter(), { setWindowOpenHandler() {}, getURL: () => instance, isDestroyed: () => false, send() {} });
  constructor() { super(); windows++; }
  setEnabled() {} setWindowButtonVisibility() {} isDestroyed() { return false; } isMinimized() { return false; } show() {} focus() {}
  async loadURL() { await requestHandler!(new Request(instance + 'v1/instance')); this.webContents.emit('did-finish-load'); }
 }
 class Tray extends EventEmitter { setToolTip() {} setContextMenu() {} }
 const app = Object.assign(new EventEmitter(), { isPackaged: true, getVersion: () => '1.0.0', getName: () => 'DillaAccountFixture', getPath: () => join(root, 'profile'), setPath() {},
  setName() {}, setDesktopName() {}, setAppUserModelId() {}, enableSandbox() {}, setBadgeCount() {}, setAsDefaultProtocolClient() {},
  commandLine: { appendSwitch() {} }, requestSingleInstanceLock: () => true, whenReady: async () => {}, dock: { setIcon() {} },
  quit() { quit = true; }, exit() { quit = true; },
 });
 const electron = { app, BrowserWindow: Window, Tray, nativeTheme: {}, nativeImage: { createFromPath: () => ({ resize: () => ({}) }) },
  protocol: { registerSchemesAsPrivileged() {}, handle: async () => {} }, ipcMain: { on() {}, handle() {} }, session: { fromPartition: () => ses, defaultSession: ses },
  Menu: { buildFromTemplate: (menu: unknown) => menu, setApplicationMenu() {} }, dialog: { showMessageBox: async () => ({ response: 0 }), showErrorBox: (_title: string, message: string) => errors.push(message) },
 };
 const module = { exports: {} };
 vm.runInNewContext(compiled.outputFiles![0].text, { module, exports: module.exports,
  require(name: string) { if (name === 'electron') return electron; if (name === 'electron-updater') return {}; if (name.includes('/providers/Provider')) return { Provider: class {} }; return load(name); },
  process: { platform, arch: 'x64', versions: {}, argv: ['dilla'], env: { APPIMAGE: '/owned/Dilla.AppImage' }, pid: process.pid, cwd: () => process.cwd(), execPath: '/owned/dilla-desktop', resourcesPath: '/owned/resources', getBuiltinModule: () => load('node:fs') },
  __dirname: '/owned/dist', Buffer, Request, Response, URL, TextDecoder, AbortController, AbortSignal, Error,
  fetch: () => new Promise(() => {}), setInterval: () => ({ unref() {} }), clearInterval() {}, setTimeout, clearTimeout,
  console: { error: (...values: unknown[]) => errors.push(values.map(String).join(' ')) },
 });
 const deadline = Date.now() + 3000;
 let update: { state: { phase: string } } | undefined;
 while (!quit && Date.now() < deadline) {
  try { update = JSON.parse(await readFile(join(root, 'reports/update-1.0.0.json'), 'utf8')); } catch { /* Startup has not published its observation yet. */ }
  if (update?.state.phase === 'checking') break;
  await new Promise(done => setTimeout(done, 5));
 }
 const failure = await readFile(join(root, 'reports/native-error.json'), 'utf8').catch(() => '');
 assert.equal(requests, 1, failure || errors.join(' ')); assert.equal(windows, 1); assert.equal(quit, false);
 const startup = await readFile(join(root, 'reports/startup-1.0.0.jsonl'), 'utf8');
 assert.match(startup, /"stage":"fetch-begin"/); assert.match(startup, /"stage":"fetch-response"/); assert.match(startup, /"stage":"document-loaded"/);
 assert.equal(update?.state.phase, 'checking'); assert.deepEqual(errors, []);
});

test('bundle fixture intercepts both POSIX and Windows module paths', async () => {
 await assertPortableEsbuildFilter(bundleFilter, 'bundle');
});
