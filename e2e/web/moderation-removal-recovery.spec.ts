// Signed moderator removals through the shipped browser client. The host starts empty and every
// account, role, invite, message and removal is made through the UI; no database fixture is used.
import { expect, type BrowserContext, type Locator, type Page } from '@playwright/test';
import { addServerVia, completeSignUp, copy, makeInviteLink, messageRow, newAccount, openChannel, railItem, sendText, signUp, test, expectRowMenuAction, rowAction,  makeRole, roleButton,
} from './support/app';
import { startTestHost, type HostConfig, type StartedHost } from './support/host';
import { arr, decode } from '../../packages/client-core/src/cbor/index';

const HOST: HostConfig = { port: 8533, control: 8534, webRoot: 'packages/web/dist', advanceClockWithWall: true };
const SERVER = 'Removal Lab';
const OWNER = 'Ada';
const MODERATOR = 'Bea';
const AUTHOR = 'Cato';
const UNAUTHORIZED = 'Dara';
const ROLE = 'Moderators';
const BODY = 'late-history removal target';

test.describe.configure({ mode: 'serial', timeout: 480_000 });
test.use({ baseURL: `http://127.0.0.1:${String(HOST.port)}` });

let host: StartedHost | null = null;
let instance = '';

test.beforeAll(async () => {
  host = await startTestHost(HOST);
  const response = await fetch(`${host.url}/v1/instance`, { headers: { accept: 'application/cbor' } });
  expect(response.status).toBe(200);
  const name = arr(decode(new Uint8Array(await response.arrayBuffer())))[5];
  if (typeof name !== 'string' || name === '') throw new Error('testhost instance name was not present');
  instance = name;
});

test.afterAll(async () => { await host?.stop(); });

const button = (scope: Page | Locator, key: string, vars?: Record<string, string>) =>
  scope.getByRole('button', { name: copy(key, vars), exact: true });
const dialog = (page: Page, key: string, vars?: Record<string, string>) =>
  page.getByRole('dialog', { name: copy(key, vars), exact: true });
const settingsNav = (settings: Locator) => settings.getByRole('navigation', { name: copy('admin.nav'), exact: true });
const roleRow = (settings: Locator, name: string) => settings.getByRole('list', { name: copy('roles.title'), exact: true })
  .getByRole('listitem').filter({ hasText: name });
const memberRow = (page: Page, name: string) => page.getByRole('complementary', {
  name: copy('members.region', { server: SERVER }), exact: true,
}).getByRole('listitem').filter({ hasText: name });

async function memberPage(page: Page, invite: string, name: string): Promise<void> {
  await page.goto(new URL(invite).pathname);
  await page.getByRole('link', { name: 'Open dilla in this browser', exact: true }).click();
  await completeSignUp(page, newAccount(name, `http://127.0.0.1:${String(HOST.control)}`), instance);
  await expect(railItem(page, SERVER)).toBeVisible();
  await openChannel(page, SERVER, 'general');
}

test('signed moderator removal reaches a live client and recovers as a tombstone after an offline reload', async ({ page, browser, guard }) => {
  const h = host;
  if (h === null) throw new Error('testhost did not start');
  const contexts: BrowserContext[] = [];
  try {
    await signUp(page, h.invite.split(',')[0] ?? '', instance, OWNER, `http://127.0.0.1:${String(HOST.control)}`);
    await addServerVia(page, 'create');
    const create = dialog(page, 'create.server.title');
    await create.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(SERVER);
    await page.keyboard.press('Enter');
    await openChannel(page, SERVER, 'general');

    const moderatorInvite = await makeInviteLink(page, SERVER);
    const authorInvite = await makeInviteLink(page, SERVER);
    const unauthorizedInvite = await makeInviteLink(page, SERVER);
    const openMember = async (invite: string, name: string) => {
      const context = await browser.newContext({ baseURL: h.url });
      contexts.push(context);
      await guard.attach(context);
      const member = await context.newPage();
      await memberPage(member, invite, name);
      return { context, page: member };
    };
    const mod = await openMember(moderatorInvite, MODERATOR);
    const author = await openMember(authorInvite, AUTHOR);
    const unauthorized = await openMember(unauthorizedInvite, UNAUTHORIZED);

    // Make a real community role with manage-messages authority and assign it through the role UI.
    await button(page, 'admin.open').click().then(() => page.getByRole('menuitem', { name: copy('signalSettings.menu.serverSettings'), exact: true }).click());
    let settings = dialog(page, 'admin.title');
    await settingsNav(settings).getByRole('button', { name: copy('roles.tab'), exact: true }).click();
    // Signal task 10c: the inline role editor; the switch saves at once.
    await makeRole(page, settings, ROLE, { perms: ['manageMessages'] });
    await expect(roleButton(settings, ROLE)).toHaveAccessibleName(copy('serverSettings.roles.row.other', { name: ROLE, count: 0 }));
    await page.keyboard.press('Escape');
    await expect(settings).toBeHidden();

    await memberRow(page, MODERATOR).locator('.d-member-list__open').click();
    await page.getByRole('dialog', { name: MODERATOR, exact: true })
      .getByRole('button', { name: copy('layout.profile.manageRoles'), exact: true }).click();
    const assign = dialog(page, 'roles.assign.title', { name: MODERATOR });
    const roleCheckbox = assign.getByRole('checkbox', { name: ROLE, exact: true });
    await roleCheckbox.click();
    await expect(roleCheckbox).toBeChecked({ timeout: 90_000 });
    await button(assign, 'roles.assign.close').click();
    await expect(memberRow(mod.page, MODERATOR)).toBeVisible();

    // Cato sends an ordinary encrypted channel message. Dara sees it but has no moderation control.
    await sendText(author.page, 'general', BODY);
    const authorRow = messageRow(author.page, 'general', BODY);
    await expect(authorRow).toHaveAttribute('data-state', 'ok');
    await expect(messageRow(page, 'general', BODY)).toBeVisible();
    const unauthorizedRow = messageRow(unauthorized.page, 'general', BODY);
    await expect(unauthorizedRow).toBeVisible();
    await unauthorizedRow.focus();
    await expectRowMenuAction(unauthorizedRow, 'moderateRemove', false);

    // Leave Cato's device offline while Bea signs a removal proof and submits it to the real API.
    const msgId = await authorRow.getAttribute('data-msg-id');
    expect(msgId).toMatch(/^[0-9a-f]{32}$/);
    const authorPath = new URL(author.page.url()).pathname;
    await author.context.setOffline(true);
    await author.page.close();

    const modRow = messageRow(mod.page, 'general', BODY);
    await expect(modRow).toBeVisible();
    await modRow.focus();
    await expectRowMenuAction(modRow, 'moderateRemove', true);
    await rowAction(modRow, 'moderateRemove');
    const confirm = dialog(mod.page, 'shell.moderation.removeTitle');
    await confirm.getByRole('textbox', { name: copy('shell.moderation.reason'), exact: true }).fill('reviewed by moderator');
    const removalResult: { value: number | null } = { value: null };
    const removalRoute = async (route: import('@playwright/test').Route) => {
      if (route.request().method() !== 'POST') { await route.continue(); return; }
      const response = await route.fetch();
      removalResult.value = response.status();
      await route.fulfill({ response });
    };
    await mod.context.route('**/moderation/removals', removalRoute);
    await button(confirm, 'shell.moderation.removeConfirm').click();
    await expect.poll(() => removalResult.value, { timeout: 30_000 }).not.toBeNull();
    await mod.context.unroute('**/moderation/removals', removalRoute);
    const accepted = removalResult.value;
    if (accepted === null) throw new Error('moderation removal did not reach the server');
    expect(accepted, 'moderation removal must be accepted by the real API').toBe(200);
    const moderatorTombstone = mod.page.locator(`.d-message-row[data-msg-id="${msgId}"]`);
    const ownerTombstone = page.locator(`.d-message-row[data-msg-id="${msgId}"]`);
    await expect(moderatorTombstone).toHaveAttribute('data-state', 'deleted');
    await expect(ownerTombstone).toHaveAttribute('data-state', 'deleted', { timeout: 90_000 });

    // The offline browser's history request arrives after the delete. The signed removal feed must keep
    // its cached message deleted; a second reload proves the recovered state is durable locally too.
    await author.context.setOffline(false);
    const recovered = await author.context.newPage();
    await recovered.goto(authorPath);
    const recoveredRow = recovered.locator(`.d-message-row[data-msg-id="${msgId}"]`);
    await expect(recoveredRow).toHaveAttribute('data-state', 'deleted', { timeout: 90_000 });
    await expect(recoveredRow).not.toContainText(BODY);
    await recovered.reload();
    await expect(recoveredRow).toHaveAttribute('data-state', 'deleted', { timeout: 90_000 });
    await expect(recoveredRow).not.toContainText(BODY);
  } finally {
    for (const context of contexts.reverse()) await context.close().catch(() => undefined);
  }
});
