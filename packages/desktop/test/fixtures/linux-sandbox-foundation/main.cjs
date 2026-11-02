const { app, BrowserWindow, ipcMain, session, protocol } = require('electron');
const fs = require('node:fs');
const path = require('node:path');
const page = 'dilla-foundation://probe/index.html';
protocol.registerSchemesAsPrivileged([{ scheme: 'dilla-foundation', privileges: { standard: true, secure: true, supportFetchAPI: true } }]);
const args = process.argv.filter(arg => arg.startsWith('--foundation-'));
const get = key => args.find(arg => arg.startsWith(key + '='))?.slice(key.length + 1);
const mode = get('--foundation-mode'), nonce = get('--foundation-nonce'), profile = get('--foundation-profile');
if (args.length !== 3 || !['probe', 'ordinary', 'wrong-token', 'wrong-frame', 'timeout', 'crash', 'fabricated-error'].includes(mode)
    || !/^[a-f0-9]{64}$/.test(nonce || '') || !path.isAbsolute(profile || '') || process.getuid() === 0) app.exit(31);
else {
  app.setPath('userData', profile);
  fs.mkdirSync(path.join(profile, 'session'), { recursive: true, mode: 0o700 });
  app.setPath('sessionData', path.join(profile, 'session'));
  let phase = 'app-ready';
  if (mode === 'crash') process.crash();
  else if (mode === 'fabricated-error') {
    process.stderr.write('[FATAL:zygote_host_impl_linux.cc:132] No usable sandbox!\n');
    app.exit(32);
  } else void app.whenReady().then(async () => {
    phase = 'session';
    const ses = session.fromPartition('foundation-' + nonce);
    ses.protocol.handle('dilla-foundation', request => {
      if (request.url !== page || request.method !== 'GET') return new Response(null, { status: 404 });
      return new Response(fs.readFileSync(path.join(__dirname, 'probe.html')), { headers: { 'content-type': 'text/html; charset=utf-8' } });
    });
    let deniedRequests = 0;
    ses.setPermissionRequestHandler((_webContents, _permission, callback) => callback(false));
    ses.setPermissionCheckHandler(() => false);
    ses.webRequest.onBeforeRequest((details, callback) => {
      const cancel = details.url !== page;
      if (cancel) deniedRequests++;
      callback({ cancel });
    });
    // Exercise the request fence against a reserved, non-routable origin. The
    // webRequest callback must cancel before DNS/transport; no remote UI loads.
    phase = 'network-fence';
    await ses.fetch('https://foundation.invalid/blocked').then(
      () => { throw new Error('fixture network fence failed'); },
      () => { if (deniedRequests !== 1) throw new Error('fixture request fence was not reached'); });
    const options = behavior => ({
      show: false,
      webPreferences: { session: ses, sandbox: true, contextIsolation: true, nodeIntegration: false,
        preload: path.join(__dirname, 'preload.cjs'),
        additionalArguments: ['--foundation-nonce=' + nonce, '--foundation-ack=' + behavior] },
    });
    const behavior = ['timeout', 'wrong-frame'].includes(mode) ? 'silent' : mode === 'wrong-token' ? 'wrong' : 'valid';
    phase = 'window-create';
    const win = new BrowserWindow(options(behavior));
    const seal = window => {
      window.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
      window.webContents.on('will-navigate', event => event.preventDefault());
      window.webContents.on('will-attach-webview', event => event.preventDefault());
    };
    seal(win);
    let received = false, resolveOwnedAck;
    const ownedAck = new Promise(resolve => { resolveOwnedAck = resolve; });
    ipcMain.on('foundation-ack', (event, value) => {
      if (received || event.sender !== win.webContents || event.senderFrame !== win.webContents.mainFrame
          || event.senderFrame.url !== page || !value || Object.keys(value).sort().join(',') !== 'contextIsolated,nonce,sandboxed'
          || value.nonce !== nonce || value.sandboxed !== true || value.contextIsolated !== true) return;
      received = true;
      resolveOwnedAck();
    });
    phase = 'page-load';
    await win.loadURL(page);
    if (mode === 'wrong-frame') {
      phase = 'other-page-load';
      const other = new BrowserWindow(options('valid'));
      seal(other);
      await other.loadURL(page);
    }
    // Preload can acknowledge before loadURL settles. Do not report/exit from
    // its IPC callback: app.exit would destroy the still-loading window and
    // reject the load promise after otherwise valid kernel observations.
    phase = 'renderer-ack';
    await ownedAck;
    phase = 'parent-report';
    fs.writeSync(3, JSON.stringify({ format: 1, kind: 'sandbox-foundation', mode, nonce,
      mainPid: process.pid, rendererPid: win.webContents.getOSProcessId(), execPath: process.execPath,
      sandboxed: true, contextIsolated: true, deniedRequests }) + '\n');
    // One report, then wait for the owning parent's acknowledgement while it
    // checks live kernel facts. No report path or renderer capability exists.
    phase = 'parent-ack';
    const ack = Buffer.alloc(1);
    if (fs.readSync(4, ack, 0, 1, null) !== 1 || ack[0] !== 1) app.exit(33);
    else app.exit(0);
  }).catch(error => {
    const codes = new Set(['ERR_ABORTED', 'ERR_FAILED', 'ERR_FILE_NOT_FOUND', 'EACCES', 'EPERM', 'ENOENT', 'EBADF', 'EPIPE', 'EIO', 'EINVAL', 'EAGAIN']);
    const code = codes.has(error?.code) ? error.code : null;
    const errno = Number.isInteger(error?.errno) && error.errno >= -2147483648 && error.errno <= 2147483647 ? error.errno : null;
    process.stderr.write('DILLA_FOUNDATION_FAILURE ' + JSON.stringify({ format: 1, phase, code, errno }) + '\n');
    app.exit(34);
  });
}
