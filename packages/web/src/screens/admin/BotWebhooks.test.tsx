// Bot v2 Task 22: webhook admin on the Bot page. The address is shown once, by create or rotate, and never again.
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import type { BotWebhook } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { en, t } from '../../strings/index.ts';
import { BotWebhooks } from './BotWebhooks.tsx';

const SERVER = 'a1'.repeat(16);
const BOTS = 'c3'.repeat(16);
const HOOK = '77'.repeat(16);
const FIRST = `https://dilla.example/hooks/${HOOK}/first-secret`;
const SECOND = `https://dilla.example/hooks/${HOOK}/second-secret`;

function setup(initial: BotWebhook[] = []) {
  const fake = new FakeClient();
  fake.set('account', account());
  let rows = initial;
  fake.handler = command => {
    switch (command.m) {
      case 'listBotWebhooks': return Promise.resolve(rows);
      case 'createBotWebhook':
        rows = [{ webhookId: HOOK, channelId: command.channelId, name: command.name, avatar: null, allowEveryone: command.allowEveryone,
          createdAt: 1_800_000_000, rotatedAt: null }];
        return Promise.resolve({ webhookId: HOOK, url: FIRST });
      case 'rotateBotWebhook': return Promise.resolve({ url: SECOND });
      default: return Promise.resolve(null);
    }
  };
  const user = userEvent.setup();
  const view = render(<CoreProvider client={fake}>
    <BotWebhooks communityId={SERVER} channels={[{ id: BOTS, name: 'bots' }]} canManage announce={vi.fn()} />
  </CoreProvider>);
  return { fake, user, view };
}
const hook = (over: Partial<BotWebhook> = {}): BotWebhook =>
  ({ webhookId: HOOK, channelId: BOTS, name: 'CI', avatar: null, allowEveryone: false, createdAt: 1_800_000_000, rotatedAt: null, ...over });

describe('webhooks on the Bot page (Task 22)', () => {
  it('the webhook URL is shown once and never again after closing', async () => {
    const { fake, user, view } = setup();
    await screen.findByText(en['bot.webhooks.empty']);
    await user.click(screen.getByRole('button', { name: en['bot.webhooks.create'] }));
    const dialog = screen.getByRole('dialog');
    await user.type(within(dialog).getByRole('textbox', { name: en['bot.webhooks.name'] }), 'CI');
    await user.click(within(dialog).getByRole('switch', { name: en['bot.webhooks.allowEveryone'] }));
    await user.click(within(dialog).getByRole('button', { name: en['bot.webhooks.create'] }));
    expect(fake.callsOf('createBotWebhook')).toEqual([{ m: 'createBotWebhook', communityId: SERVER, channelId: BOTS, name: 'CI', allowEveryone: true }]);
    const shown = await screen.findByDisplayValue(FIRST);
    expect(shown).toBeVisible();
    expect(screen.getByText(en['bot.webhooks.urlOnce'])).toBeVisible();
    await user.click(screen.getByRole('button', { name: en['bot.webhooks.done'] }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(await screen.findByRole('group', { name: 'CI' })).toBeVisible();
    expect(view.container.innerHTML).not.toContain('first-secret');
    expect(document.body.innerHTML).not.toContain('first-secret');
  });

  it('rotate shows a new URL once', async () => {
    const { fake, user } = setup([hook()]);
    const row = await screen.findByRole('group', { name: 'CI' });
    expect(within(row).getByText(t('bot.webhooks.posts', { channel: 'bots' }))).toBeVisible();
    await user.click(within(row).getByRole('button', { name: en['bot.webhooks.rotate'] }));
    expect(fake.callsOf('rotateBotWebhook')).toEqual([{ m: 'rotateBotWebhook', communityId: SERVER, webhookId: HOOK }]);
    expect(await screen.findByDisplayValue(SECOND)).toBeVisible();
    await user.click(screen.getByRole('button', { name: en['bot.webhooks.done'] }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(document.body.innerHTML).not.toContain('second-secret');
  });

  it('allow @everyone and delete act on the webhook', async () => {
    const { fake, user } = setup([hook()]);
    const row = await screen.findByRole('group', { name: 'CI' });
    await user.click(within(row).getByRole('switch', { name: en['bot.webhooks.allowEveryone'] }));
    expect(fake.callsOf('updateBotWebhook')).toEqual([{ m: 'updateBotWebhook', communityId: SERVER, webhookId: HOOK, name: 'CI', allowEveryone: true }]);
    await user.click(within(row).getByRole('button', { name: en['bot.webhooks.delete'] }));
    const confirm = screen.getByRole('dialog');
    await user.click(within(confirm).getByRole('button', { name: en['bot.webhooks.delete'] }));
    await waitFor(() => expect(fake.callsOf('deleteBotWebhook')).toEqual([{ m: 'deleteBotWebhook', communityId: SERVER, webhookId: HOOK }]));
  });

  it('with no Bot channel, create says to mark one first', async () => {
    const fake = new FakeClient();
    fake.set('account', account());
    fake.handler = command => Promise.resolve(command.m === 'listBotWebhooks' ? [] : null);
    render(<CoreProvider client={fake}><BotWebhooks communityId={SERVER} channels={[]} canManage announce={vi.fn()} /></CoreProvider>);
    expect(await screen.findByText(en['bot.webhooks.needsChannel'])).toBeVisible();
    expect(screen.getByRole('button', { name: en['bot.webhooks.create'] })).toHaveAttribute('aria-disabled', 'true');
  });
});
