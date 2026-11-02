import { readFile, readlink } from 'node:fs/promises';

export function processIdentity(pid, status, stat, uid) {
 const field = name => new RegExp(`^${name}:\\s+([^\\n]+)$`, 'm').exec(status)?.[1].trim();
 const uids = field('Uid')?.split(/\s+/).map(Number);
 if (!Number.isSafeInteger(pid) || pid < 2 || Number(field('Pid')) !== pid || uids?.length !== 4 || uids.some(value => value !== uid) || uid === 0) throw new Error('Unexpected process PID/UID');
 const tail = stat.slice(stat.lastIndexOf(')') + 2).split(/\s+/), start = tail[19];
 if (!/^[1-9]\d*$/.test(start ?? '') || Number(stat.slice(0, stat.indexOf(' '))) !== pid) throw new Error('Invalid process start identity');
 const result = { pid, uid, parent: Number(field('PPid')), start, capEff: field('CapEff'), noNewPrivs: Number(field('NoNewPrivs')), seccomp: Number(field('Seccomp')), filters: Number(field('Seccomp_filters')) };
 if (!Number.isSafeInteger(result.filters) || result.filters < 0 || !/^[a-f0-9]{16}$/i.test(result.capEff ?? '')) throw new Error('Missing kernel sandbox observations');
 return result;
}
export function assertRendererSandbox(browser, before, after, cmdline) {
 if (JSON.stringify(before) !== JSON.stringify(after)) throw new Error('Renderer process identity changed during observation');
 if (before.noNewPrivs !== 1 || before.seccomp !== 2 || !/^0+$/.test(before.capEff)) throw new Error('Renderer kernel sandbox is not active');
 if (before.filters <= browser.filters) throw new Error('Renderer has no additional seccomp filter above browser/container inheritance');
 const records = cmdline.split('\0').filter(Boolean);
 // Chromium's Linux zygote calls SetProcessTitleFromCommandLine after fork,
 // flattening argv into one NUL-terminated title: executable --type=... .
 // Preserve actual argv boundaries otherwise, including spaces inside values.
 const args = records.length === 1 && /^\/\S+ --type=/.test(records[0]) ? records[0].split(' ') : records;
 const types = args.filter(arg => arg.startsWith('--type='));
 if (types.length !== 1 || types[0] !== '--type=renderer') throw new Error('Expected a real renderer process');
 if (args.some(arg => /^--(?:no-sandbox|disable-(?:setuid-sandbox|seccomp-filter-sandbox|namespace-sandbox))(?:=|$)/.test(arg))) throw new Error('Renderer sandbox bypass');
}
export async function observeRendererSandbox(browser, appPid, pages) {
 const uid = process.getuid();
 const observe = async pid => processIdentity(pid, await readFile(`/proc/${pid}/status`, 'utf8'), await readFile(`/proc/${pid}/stat`, 'utf8'), uid);
 const appBefore = await observe(appPid);
 const cdp = await browser.newBrowserCDPSession();
 try {
  const { processInfo } = await cdp.send('SystemInfo.getProcessInfo');
  if (!processInfo.some(item => item.type === 'browser' && item.id === appPid)) throw new Error('CDP browser PID differs from installed app');
  const renderers = processInfo.filter(item => item.type === 'renderer');
  if (!renderers.length) throw new Error('No real renderer process observed');
  const evidence = [];
  for (const { id: pid } of renderers) {
   const before = await observe(pid), chain = [before];
   let parent = before.parent;
   while (parent !== appPid && chain.length < 16) {
    if (parent < 2 || chain.some(item => item.pid === parent)) throw new Error('Renderer is not an owned app descendant');
    const ancestor = await observe(parent); chain.push(ancestor); parent = ancestor.parent;
   }
   if (parent !== appPid) throw new Error('Renderer ancestry did not reach installed app');
   const cmdline = await readFile(`/proc/${pid}/cmdline`, 'utf8');
   let executable;
   try { executable = await readlink(`/proc/${pid}/exe`); if (executable !== '/opt/Dilla/dilla-desktop') throw new Error('Unexpected renderer executable'); }
   catch (error) { if (!['EACCES', 'EPERM'].includes(error.code)) throw error; executable = 'unavailable: sandbox nondumpability; no privilege override'; }
   for (const ancestor of chain.slice(1)) if (JSON.stringify(ancestor) !== JSON.stringify(await observe(ancestor.pid))) throw new Error('Renderer ancestor identity changed');
   const after = await observe(pid); assertRendererSandbox(appBefore, before, after, cmdline);
   evidence.push({ ...after, executable, ancestry: chain.map(item => ({ pid: item.pid, start: item.start })) });
  }
  for (const page of pages) {
   const session = await page.context().newCDPSession(page), contexts = [];
   try {
    session.on('Runtime.executionContextCreated', event => contexts.push(event.context));
    await session.send('Runtime.enable');
    const { frameTree } = await session.send('Page.getFrameTree');
    if (!contexts.some(context => context.name === 'Electron Isolated Context' && context.auxData?.frameId === frameTree.frame.id && context.auxData?.isDefault === false)) throw new Error('Missing main-frame Electron isolated context');
    const globals = await page.evaluate(() => ({ process: typeof globalThis.process, require: typeof globalThis.require, module: typeof globalThis.module }));
    if (Object.values(globals).some(value => value !== 'undefined')) throw new Error('Node globals exposed to page main world');
   } finally { await session.detach(); }
  }
  if (JSON.stringify(appBefore) !== JSON.stringify(await observe(appPid))) throw new Error('Installed browser identity changed');
  return { browser: appBefore, renderers: evidence, isolatedMainFrames: pages.length, mainWorldNodeGlobals: 'undefined' };
 } finally { await cdp.detach(); }
}
