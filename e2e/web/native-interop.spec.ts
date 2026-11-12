// Interop of the native GPUI client library (clients/desktop/dilla-client, driven by its `peer` example) and the shipping
// web client against one real dillad: the browser makes the server and its invite link, the native client signs up,
// joins through that code and reads and writes the channel, and each side shows the other's messages under the right
// author, within 10 s. Anti-goal of the native client: it must interoperate with the web and phone clients.
import { expect } from '@playwright/test';
import { GpuiPeer } from './support/gpui-peer';
import {
  CONTROL_URL, HEX32, PUBLIC_URL, TEST_TIMEOUT, WAIT, addServerVia, copy, expectComposerReady, instanceInvite,
  makeInviteLink, messageRow, railItem, sendText, signUp, tag, test,
} from './support/app';

test.describe.configure({ timeout: TEST_TIMEOUT });

const EXCHANGE_MS = 10_000;

test('native client and web client exchange messages', async ({ page, instanceName }) => {
  // The web user: signs up with the bootstrap invite and starts a server (its #general opens at once).
  await signUp(page, instanceInvite(), instanceName);
  const server = `Interop ${tag()}`;
  await addServerVia(page, 'create');
  await page.getByRole('textbox', { name: copy('create.server.name'), exact: true }).fill(server);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/c\/[0-9a-f]{32}\/[0-9a-f]{32}$/, { timeout: WAIT });
  const community = new URL(page.url()).pathname.split('/')[2] ?? '';
  expect(community).toMatch(HEX32);
  await expect(railItem(page, server)).toHaveAttribute('aria-current', 'page');
  await expectComposerReady(page, 'general');
  const code = (await makeInviteLink(page, server)).split('/').pop() ?? '';
  expect(code).not.toBe('');

  // The native client: a sign-up of its own (a second person), then the join through the web user's invite code.
  const peer = GpuiPeer.spawn();
  try {
    const username = `gpui${tag()}`;
    const me = await peer.start({ server: PUBLIC_URL, control: CONTROL_URL, instanceInvite: instanceInvite(), community, code, username });
    expect(me.user).toMatch(HEX32);
    const channel = await peer.channel(community, 'general');
    expect(channel).toMatch(HEX32);

    // Web to native: the native client shows the web user's message, authored by someone other than itself.
    const fromWeb = `hello from web ${tag()}`;
    await sendText(page, 'general', fromWeb);
    const author = await peer.wait(channel, fromWeb, EXCHANGE_MS);
    expect(author).toMatch(HEX32);
    expect(author).not.toBe(me.user);

    // Native to web: the page shows it, confirmed, under the native user's name.
    const fromNative = `hello from gpui ${tag()}`;
    await peer.send(channel, fromNative);
    const row = messageRow(page, 'general', fromNative);
    await expect(row).toHaveCount(1, { timeout: EXCHANGE_MS });
    await expect(row).toHaveAttribute('data-state', 'ok');
    await expect(row).toContainText(username);
  } finally {
    await peer.close();
  }
});
