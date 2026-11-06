import '@dilla/ui/base.css';
import './app.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { connectCore, createCoreWorker } from '@dilla/client-core';
import { DesktopTitlebar } from './DesktopTitlebar.tsx';
import { App } from './App.tsx';
import { CoreProvider } from './core/context.tsx';
import type { UiError } from './core/errors.ts';
import { readTheme } from './prefs.ts';
import { applyPreference } from './theme.ts';
import { browser } from './browser.ts';
import { watchUpdates } from './update.ts';
import { createUpdateCheckpointStore } from './update-checkpoint.ts';
import { desktopBridge, startDesktopLifecycle } from './desktop.ts';
import { MOBILE_INFO_TIMEOUT_MS, mobileBridge, mobileInfoWithin, watchMobilePermission } from './mobile.ts';
import { MobileBootError } from './screens/Boot.tsx';
import { t } from './strings/index.ts';

startDesktopLifecycle();
applyPreference(window, await readTheme());
const desktop = await desktopBridge()?.info();
if (desktop) { document.documentElement.dataset.desktop = ''; document.documentElement.dataset.desktopPlatform = desktop.platform; }
const mobileShell = mobileBridge();
const mobileInfo = mobileShell && await mobileInfoWithin(mobileShell);
if (mobileShell && (mobileInfo === 'timeout' || mobileInfo === 'failed')) {
  // The native shell never answered, or refused: show the error splash (Try again, Switch instance), never a blank page.
  // No core starts, since the instance it would open is unknown.
  document.documentElement.dataset.mobile = '';
  const element = document.getElementById('root');
  if (!element) throw new Error('dilla: #root is missing');
  createRoot(element).render(<StrictMode><MobileBootError bridge={mobileShell} detail={t(mobileInfo === 'timeout' ? 'mobile.boot.timeout.detail' : 'mobile.boot.failed.detail')} /></StrictMode>);
  throw new Error(mobileInfo === 'timeout' ? `dilla: the mobile shell did not answer within ${MOBILE_INFO_TIMEOUT_MS} ms` : 'dilla: the mobile shell refused its info');
}
const mobile = mobileInfo === 'timeout' || mobileInfo === 'failed' ? undefined : mobileInfo;
if (mobile) { document.documentElement.dataset.mobile = ''; document.documentElement.dataset.mobilePlatform = mobile.platform; }
// Mobile wins when both bridges exist, as in layout.ts and notify.ts.
const native = mobile ?? desktop;
if (mobile) watchMobilePermission(mobileBridge()!);
const worker = createCoreWorker(native ? { instanceURL: native.instance } : undefined);
const client = connectCore(worker);
const element = document.getElementById('root');
if (!element) throw new Error('dilla: #root is missing');
const root = createRoot(element);
// Installed clients still checkpoint drafts/files for an explicit reload.
// Their updater belongs to the native shell; a server's web-build hint cannot
// replace bundled code or cause an automatic renderer reload.
const update = { currentEntry: new URL(import.meta.url).pathname, watch: native ? () => () => {} : watchUpdates,
  store: createUpdateCheckpointStore(indexedDB, crypto, Date.now), reload: () => browser.reload() };
const show = (fatal: UiError | null) => root.render(<StrictMode><CoreProvider client={client}>{desktop && <DesktopTitlebar bridge={desktopBridge()!} info={desktop} />}<App fatal={fatal} update={update} /></CoreProvider></StrictMode>);
show(null);
worker.addEventListener('error', e => {
  e.preventDefault();
  show({ code: 'E_WORKER', detail: '', status: 0, retryAfterMs: null });
}, { once: true });
