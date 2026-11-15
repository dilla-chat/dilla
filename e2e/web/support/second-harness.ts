import type {
  BrowserContext, LaunchOptions, Page, PlaywrightWorkerArgs, PlaywrightWorkerOptions, ViewportSize,
} from '@playwright/test';
import { mkdirSync, rmSync } from 'node:fs';
import { PROFILE_ROOT, expect, test as base } from './persistent';

/** The second browser's persistent profile: one directory per project and worker, beside the first's
 *  (persistent.ts:5-7 names `${PROFILE_ROOT}/${project}-${workerIndex}`). */
export function secondProfileDir(project: string, workerIndex: number): string {
  return `${PROFILE_ROOT}/${project}-${workerIndex}-b`;
}

/** Empties `dir` and launches a persistent context on it with the project's own launch options. */
export async function launchSecond(
  playwright: PlaywrightWorkerArgs['playwright'],
  browserName: PlaywrightWorkerOptions['browserName'],
  options: { launchOptions: LaunchOptions; headless: boolean; channel: string | undefined; viewport: ViewportSize | null; baseURL: string | undefined },
  dir: string,
): Promise<BrowserContext> {
  rmSync(dir, { recursive: true, force: true });
  mkdirSync(dir, { recursive: true });
  const { launchOptions, headless, channel, viewport, baseURL } = options;
  return playwright[browserName].launchPersistentContext(dir, { ...launchOptions, headless, channel, viewport, baseURL });
}

/** A second browser opened on demand, closed and its profile removed in finally whatever the test did (lesson d). */
type SecondFixtures = { secondBrowser: () => Promise<{ context: BrowserContext; page: Page }> };

export const test = base.extend<SecondFixtures>({
  secondBrowser: async ({ playwright, browserName, launchOptions, headless, channel, viewport, baseURL }, use, testInfo) => {
    const dir = secondProfileDir(testInfo.project.name, testInfo.workerIndex);
    const held: { context: BrowserContext | null } = { context: null };
    try {
      await use(async () => {
        const context = held.context
          ?? await launchSecond(playwright, browserName, { launchOptions, headless, channel, viewport, baseURL }, dir);
        held.context = context;
        return { context, page: context.pages()[0] ?? (await context.newPage()) };
      });
    } finally {
      await held.context?.close();
      rmSync(dir, { recursive: true, force: true });
    }
  },
});
export { expect };
