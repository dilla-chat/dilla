import { test as base, expect, type BrowserContext, type BrowserType, type Page, type TestInfo } from '@playwright/test';
import { desktopE2E, launchDesktopContext } from './electron.ts';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { mkdirSync, rmSync } from 'node:fs';

/** Where the browsers' profile directories go. A lane that shares its machine with others sets DILLA_E2E_PROFILES, since each lane empties `<project>-<worker>` at the start of a test. */
export const PROFILE_ROOT = process.env.DILLA_E2E_PROFILES ?? join(tmpdir(), 'dw');
export function profileDirFor(project: string, workerIndex: number): string {
  return `${PROFILE_ROOT}/${project}-${workerIndex}`;
}

type Holder = { context: BrowserContext | null; open: () => Promise<BrowserContext> };
type Fixtures = {
  profileDir: string;
  relaunch: () => Promise<{ context: BrowserContext; page: Page }>;
  holder: Holder;
};

export const test = base.extend<Fixtures>({
  browser: async ({ browser }, use) => {
    if (!desktopE2E()) { await use(browser); return; }
    const original = browser.newContext.bind(browser);
    const contexts: BrowserContext[] = [];
    browser.newContext = async options => { const context = await launchDesktopContext(null, options ?? {}, false); contexts.push(context); return context; };
    try { await use(browser); }
    finally { browser.newContext = original; await Promise.all(contexts.map(context => context.close())); }
  },
  profileDir: async ({}, use, testInfo: TestInfo) => {
    const dir = profileDirFor(testInfo.project.name, testInfo.workerIndex);
    rmSync(dir, { recursive: true, force: true });
    mkdirSync(dir, { recursive: true });
    await use(dir);
    rmSync(dir, { recursive: true, force: true });
  },
  // No context.tracing call anywhere in this file: with trace: 'retain-on-failure' the runner starts
  // tracing on every context launchPersistentContext makes and keeps the trace when the test fails
  // (playwright 1.63.0, lib/index.js:134-145, 648-651); a second tracing.start throws
  // "Tracing has been already started".
  holder: async ({ playwright, browserName, launchOptions, headless, channel, viewport, baseURL, profileDir }, use) => {
    const holder: Holder = {
      context: null,
      open: async () => {
        const type: BrowserType = playwright[browserName];
        const context = desktopE2E()
          ? await launchDesktopContext(profileDir, { launchOptions, viewport, baseURL })
          : await type.launchPersistentContext(profileDir, { ...launchOptions, headless, channel, viewport, baseURL });
        holder.context = context;
        return context;
      },
    };
    await holder.open();
    await use(holder);
    await holder.context?.close();
  },
  context: async ({ holder }, use) => {
    await use(holder.context as BrowserContext);
  },
  page: async ({ context }, use) => {
    await use(context.pages()[0] ?? (await context.newPage()));
  },
  relaunch: async ({ holder }, use) => {
    await use(async () => {
      const previous = holder.context;
      holder.context = null;
      await previous?.close();
      const context = await holder.open();
      return { context, page: context.pages()[0] ?? (await context.newPage()) };
    });
  },
});
export { expect };
