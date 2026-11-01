import { expect } from '@playwright/test';
import {
  addServerVia, completeSignUp, composerBox, copy, makeInviteLink, messageRow, newAccount, openChannel, railItem, signUp, test, WAIT,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';

const hostPort = Number(process.env.DILLA_E2E_AUTOMOD_PORT ?? 8511);
const HOST: HostConfig = {
  port: hostPort, control: hostPort + 1, webRoot: 'packages/web/dist', botRuntime: true, advanceClockWithWall: true,
};
const SERVER = 'Automatic Moderation Acceptance';
const OWNER = 'Ada';
const TARGET = 'Bea';
const EXCERPT_BODY = `automod-evidence-${'x'.repeat(540)}-AUTOMOD_TAIL_SENTINEL`;

async function traceCoreWorker(page: import('@playwright/test').Page): Promise<void> {
  await page.addInitScript(() => {
    const root = window as unknown as { __dillaWorkerTrace?: { calls: unknown[]; returns: unknown[] } };
    root.__dillaWorkerTrace = { calls: [], returns: [] };
    const OriginalWorker = window.Worker;
    window.Worker = new Proxy(OriginalWorker, {
      construct(target, args, newTarget) {
        const worker = Reflect.construct(target, args, newTarget) as Worker;
        const post = worker.postMessage.bind(worker);
        worker.postMessage = (message: unknown, transfer?: Transferable[] | StructuredSerializeOptions) => {
          root.__dillaWorkerTrace?.calls.push(message);
          post(message, transfer as Transferable[]);
        };
        worker.addEventListener('message', event => root.__dillaWorkerTrace?.returns.push(event.data));
        return worker;
      },
    }) as typeof Worker;
  });
}

async function workerDiagnostic(page: import('@playwright/test').Page): Promise<unknown> {
  return page.evaluate(() => {
    const trace = (window as unknown as { __dillaWorkerTrace?: { calls: unknown[]; returns: unknown[] } }).__dillaWorkerTrace;
    const calls = (trace?.calls ?? []).slice(-30).map(raw => {
      const event = raw as { id?: number; command?: { m?: string; operation?: number; requestId?: string } };
      return { id: event.id ?? null, method: event.command?.m ?? null,
        operation: event.command?.operation ?? null, requestId: event.command?.requestId ?? null };
    });
    const returns = (trace?.returns ?? []).slice(-30).map(raw => {
      const event = raw as { t?: string; id?: number; ok?: boolean; error?: { code?: string }; value?: { status?: unknown; upload?: unknown } };
      return { type: event.t ?? null, id: event.id ?? null, ok: event.ok ?? null,
        errorCode: event.error?.code ?? null, status: event.value?.status ?? null, upload: event.value?.upload ?? null };
    });
    return { calls, returns };
  });
}

async function sendRepeat(page: import('@playwright/test').Page, text: string): Promise<void> {
  const rows = messageRow(page, 'general', text);
  const previousCount = await rows.count();
  const box = composerBox(page, 'general');
  await box.fill(text);
  await box.press('Enter');
  await expect(rows).toHaveCount(previousCount + 1);
  await expect(rows.last()).toHaveAttribute('data-state', 'ok');
}

test.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });

test('automatic moderation starts log-only, decrypts repeats, bounds evidence, and applies a configured action once', async ({ page, browser, guard, instanceName }) => {
  let host: StartedHost | null = null;
  let targetContext: import('@playwright/test').BrowserContext | null = null;
  try {
    await traceCoreWorker(page);
    host = await startTestHost(HOST);
    const owner = await signUp(page, host.invite.split(',')[0] ?? '', instanceName, OWNER, host.control);
    await addServerVia(page, 'create');
    const create = page.getByRole('dialog', { name: copy('create.server.title'), exact: true });
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');
    await openChannel(page, SERVER, 'general');

    const invite = await makeInviteLink(page, SERVER);
    targetContext = await browser.newContext({ baseURL: host.url });
    await guard.attach(targetContext);
    const targetPage = await targetContext.newPage();
    await targetPage.goto(new URL(invite).pathname);
    await targetPage.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
    await completeSignUp(targetPage, newAccount(TARGET, host.control), instanceName);
    await expect(railItem(targetPage, SERVER)).toBeVisible();
    await openChannel(targetPage, SERVER, 'general');

    // The bot needs only its warnings capability for the later warning action.
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    let nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('admin.nav.bot'), exact: true }).click();
    await settings.getByRole('switch', { name: copy('serverSettings.bot.cap.warnings'), exact: true }).click();
    await settings.getByRole('button', { name: copy('serverSettings.bot.enable'), exact: true }).click();
    const stepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await stepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await stepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
    await expect(settings.locator('.dw-bot-settings').getByRole('status').filter({ hasText: copy('admin.bot.status.ready') })).toHaveText(copy('admin.bot.status.ready'), { timeout: 90_000 });

    // A real user can send repeated plaintext, which the native bot receives as decrypted group events.
    const harmlessRepeat = 'automod default remains passive';
    await sendRepeat(targetPage, harmlessRepeat);
    await sendRepeat(targetPage, harmlessRepeat);
    await sendRepeat(targetPage, harmlessRepeat);
    await expect(targetPage.locator('.d-message-row').filter({ hasText: harmlessRepeat })).toHaveCount(3);

    await settings.getByRole('button', { name: copy('settings.close'), exact: true }).click();
    await expect(settings).toBeHidden();
    await page.getByRole('button', { name: copy('admin.open'), exact: true }).click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    settings = page.getByRole('dialog', { name: copy('admin.title'), exact: true });
    nav = settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
    await nav.getByRole('button', { name: copy('workspace.automoderation'), exact: true }).click();

    const loadSettings = settings.getByRole('button', { name: copy('admin.bot.automod.load'), exact: true });
    await loadSettings.click();
    try {
      await expect(settings.getByText(copy('admin.bot.automod.loaded'), { exact: true })).toBeVisible({ timeout: 90_000 });
    } catch (error) {
      const diagnostic = await workerDiagnostic(page);
      throw new Error(`${String(error)}\nAutomoderation worker RPC summary: ${JSON.stringify(diagnostic)}`);
    }
    const enabled = settings.getByRole('checkbox', { name: copy('admin.bot.automod.enabled'), exact: true });
    const duplicateAction = settings.getByRole('combobox', { name: copy('admin.bot.automod.duplicate'), exact: true });
    await expect(enabled).not.toBeChecked();
    await expect(duplicateAction).toHaveValue('log');

    // The log-only default did not remove content or prevent another send.
    await expect(targetPage.locator('.d-message-row').filter({ hasText: harmlessRepeat })).toHaveCount(3);
    await sendRepeat(targetPage, 'a further message is still allowed under the default policy');

    await enabled.check();
    await duplicateAction.selectOption('warn');
    const saveAutomod = settings.locator('.d-settings-frame__footer').getByRole('button', { name: copy('admin.overview.save'), exact: true });
    await saveAutomod.click();
    const saveStepUp = page.getByRole('dialog', { name: copy('factorSettings.stepUp.title'), exact: true });
    await saveStepUp.getByLabel(copy('factorSettings.stepUp.password'), { exact: true }).fill(owner.password);
    await saveStepUp.getByRole('button', { name: copy('factorSettings.stepUp.confirm'), exact: true }).click();
    await expect(saveAutomod).toHaveCount(0, { timeout: WAIT });
    // The save acknowledgement triggers a fresh read; verify the values that
    // came back from the bot rather than trusting the local draft.
    await expect(settings.getByText(copy('admin.bot.automod.loaded'), { exact: true })).toBeVisible({ timeout: 90_000 });
    await expect(enabled).toBeChecked();
    await expect(duplicateAction).toHaveValue('warn');

    // Acknowledgment activates the policy. Receipts have whole-second precision,
    // so cross that observable server-clock boundary before producing live events.
    const clockURL = `${host.control}/debug/clock`;
    const acknowledgedSecond = (await (await fetch(clockURL)).json() as { now: number }).now;
    await expect.poll(async () => (await (await fetch(clockURL)).json() as { now: number }).now).toBeGreaterThan(acknowledgedSecond);
    await sendRepeat(targetPage, EXCERPT_BODY);
    await sendRepeat(targetPage, EXCERPT_BODY);
    await sendRepeat(targetPage, EXCERPT_BODY);
    await expect(targetPage.locator('.d-message-row').filter({ hasText: EXCERPT_BODY })).toHaveCount(3);

    await nav.getByRole('button', { name: copy('workspace.moderationLog'), exact: true }).click();
    const loadCases = settings.getByRole('button', { name: copy('admin.bot.moderationLog.loadAutomatic'), exact: true });
    const automaticCase = settings.locator('article').filter({ hasText: 'duplicate_body' });
    const automaticEmpty = settings.getByRole('note').filter({ hasText: copy('admin.bot.moderationLog.empty') });
    // A log read is a snapshot. Wait for this request to finish; retry only a
    // completed empty snapshot so a second click cannot replace a pending read.
    const deadline = Date.now() + WAIT;
    let foundAutomaticCase = false;
    while (!foundAutomaticCase) {
      const remaining = deadline - Date.now();
      if (remaining <= 0) throw new Error('Automatic moderation case did not appear before the bounded log wait expired');
      await loadCases.click();
      let completedSnapshot: 'case' | 'empty' | null = null;
      await expect.poll(async () => {
        const count = await automaticCase.count();
        if (count === 1) completedSnapshot = 'case';
        else if (count === 0 && await automaticEmpty.isVisible()) completedSnapshot = 'empty';
        return completedSnapshot;
      }, { timeout: remaining, intervals: [500, 1_000, 2_000] }).not.toBeNull();
      foundAutomaticCase = completedSnapshot === 'case';
    }
    await expect(automaticCase).toHaveCount(1);
    await expect(automaticCase).toContainText('warn');
    const excerpt = await automaticCase.locator('p').nth(1).innerText();
    expect(excerpt.length).toBe(512);
    expect(excerpt).toContain('automod-evidence-');
    expect(excerpt).not.toContain('AUTOMOD_TAIL_SENTINEL');

    // Reloading the durable case feed does not replay the action or duplicate its case.
    await loadCases.click();
    await expect.poll(async () => settings.locator('article').count(), { timeout: 90_000, intervals: [500, 1_000, 2_000] }).toBe(1);
    await expect(targetPage.locator('.d-message-row').filter({ hasText: EXCERPT_BODY })).toHaveCount(3);
  } finally {
    await targetContext?.close();
    await host?.stop();
  }
});
