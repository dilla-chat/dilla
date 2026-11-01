// Lesson d (the leaked-contexts lesson, media suite): a context a test opens itself outlives the test unless the test
// closes it, still joined to its LiveKit room and still encoding, and every later test runs slower beside it. Each voice
// spec closes what it opened in `finally` (peer-browser.ts); these hooks prove it: after each test the worker's browser
// holds exactly as many contexts as before it.
import { expect, type TestType } from '@playwright/test';
import { desktopE2E } from '../../web/support/electron.ts';
import { liveDesktopVoiceContexts } from './desktop.ts';

/** Registers the before/after count of `browser.contexts()` on the spec's `test`. Call once per spec file. */
// eslint-disable-next-line @typescript-eslint/no-explicit-any
export function watchContexts(test: TestType<any, any>): void {
  let before = -1;
  test.beforeEach(({ browser }) => {
    before = desktopE2E() ? liveDesktopVoiceContexts.size : browser.contexts().length;
  });
  test.afterEach(({ browser }) => {
    expect(desktopE2E() ? liveDesktopVoiceContexts.size : browser.contexts().length, 'client contexts left open by the test (lesson d)').toBe(before);
  });
}
