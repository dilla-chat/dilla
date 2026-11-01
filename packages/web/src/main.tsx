import '@dilla/ui/base.css';
import './app.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { connectCore, createCoreWorker } from '@dilla/client-core';
import { App } from './App.tsx';
import { CoreProvider } from './core/context.tsx';
import type { UiError } from './core/errors.ts';
import { readTheme } from './prefs.ts';
import { applyPreference } from './theme.ts';
import { browser } from './browser.ts';
import { watchUpdates } from './update.ts';
import { createUpdateCheckpointStore } from './update-checkpoint.ts';
import { desktopBridge, startDesktopLifecycle } from './desktop.ts';

startDesktopLifecycle();
applyPreference(window, await readTheme());
const desktop = await desktopBridge()?.info();
const worker = createCoreWorker(desktop ? { instanceURL: desktop.instance } : undefined);
const client = connectCore(worker);
const element = document.getElementById('root');
if (!element) throw new Error('dilla: #root is missing');
const root = createRoot(element);
const update = { currentEntry: new URL(import.meta.url).pathname, watch: watchUpdates,
  store: createUpdateCheckpointStore(indexedDB, crypto, Date.now), reload: () => browser.reload() };
const show = (fatal: UiError | null) => root.render(<StrictMode><CoreProvider client={client}><App fatal={fatal} update={update} /></CoreProvider></StrictMode>);
show(null);
worker.addEventListener('error', e => {
  e.preventDefault();
  show({ code: 'E_WORKER', detail: '', status: 0, retryAfterMs: null });
}, { once: true });
