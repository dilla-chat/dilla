import type { BrowserContext, TestInfo } from '@playwright/test';
import { randomUUID } from 'node:crypto';

/** Electron.launch does not invoke Playwright's browser-context tracing hook.
 * Record its actual renderer actions, snapshots, console and network explicitly. */
export async function nativeTrace(context: BrowserContext, info?: TestInfo): Promise<() => Promise<void>> {
 if (!info || info.project.use.trace === 'off') return async () => {};
 await context.tracing.start({ screenshots: true, snapshots: true, sources: true });
 let stopped = false;
 return async () => {
  if (stopped) return; stopped = true;
  if (info.status === info.expectedStatus) { await context.tracing.stop(); return; }
  const path = info.outputPath(`electron-${randomUUID()}.zip`);
  await context.tracing.stop({ path });
  await info.attach('Electron renderer trace', { path, contentType: 'application/zip' });
 };
}
