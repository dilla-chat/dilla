import { expect } from '@playwright/test';
import { spawn } from 'node:child_process';
import { join } from 'node:path';
import { desktopApplication, desktopE2E } from './support/electron.ts';
import {
  COPY, TEST_TIMEOUT, WAIT, expectAccessible, expectShell, instanceInvite, joinCommunity, messageRow,
  openChannel, railNav, sendText, signUp, splash, tag, test, REPO_ROOT,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

test('a second tab waits while the first holds the device and takes over when it closes', async ({ page, context, peer, instanceName }) => {
  const setup = await peer.setup();
  await peer.register();
  const channel = peer.channelName;
  await signUp(page, instanceInvite(), instanceName);
  await joinCommunity(page, peer, setup);
  await openChannel(page, peer.communityName, channel);
  const browserDevice = (await peer.waitForDevices(2)).find((d) => d !== setup.device_id);

  if (desktopE2E()) {
    const application = desktopApplication(context), worker = page.workers()[0];
    const profile = await application.evaluate(({ app }) => app.getPath('userData'));
    // Playwright launches through cmd.exe on Windows. Its ChildProcess.spawnfile
    // is that wrapper, so ask the actual Electron main process for its binary.
    const executable = await application.evaluate(() => process.execPath);
    const native = await application.browserWindow(page);
    const identity = await native.evaluate(win => { win.hide(); return win.id; }); await native.dispose();
    const child = spawn(executable, [join(REPO_ROOT, 'packages/desktop'), `--profile=${profile}`, `--instance=${new URL(page.url()).origin}`, '--allow-loopback'], { stdio: 'ignore' });
    const timer = setTimeout(() => child.kill('SIGKILL'), 15000);
    try {
      const code = await new Promise<number | null>((done, reject) => { child.once('error', reject); child.once('exit', done); });
      expect(code, 'a duplicate launch hands off and exits cleanly').toBe(0);
    } finally { clearTimeout(timer); }
    await expect.poll(() => application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows().map(win => ({ id: win.id, visible: win.isVisible() })))).toEqual([{ id: identity, visible: true }]);
    expect(context.pages()).toHaveLength(1); expect(page.workers()[0]).toBe(worker);
    const text = `after duplicate desktop launch ${tag()}`;
    await sendText(page, channel, text);
    expect((await peer.waitFor(text)).sender_device).toBe(browserDevice);
    return;
  }

  const second = await context.newPage();
  await second.goto('/');
  await expect(splash(second, COPY.otherTab)).toBeVisible({ timeout: WAIT });
  await expect(railNav(second)).toHaveCount(0);
  await expectAccessible(second, 'other-tab');

  // The first tab keeps working while the second waits.
  const first = `from the first tab ${tag()}`;
  await sendText(page, channel, first);
  await peer.waitFor(first);

  await page.close();
  await expectShell(second);
  await openChannel(second, peer.communityName, channel);
  await expect(messageRow(second, channel, first)).toHaveAttribute('data-state', 'ok', { timeout: WAIT });
  const later = `from the second tab ${tag()}`;
  await sendText(second, channel, later);
  expect((await peer.waitFor(later)).sender_device).toBe(browserDevice);
  await second.close();
});
