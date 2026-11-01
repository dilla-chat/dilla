import { describe, expect, it, vi } from 'vitest';
import { render, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { GEN, MEMBERS, O1, P1, P2, peerRow, ownRow, timeline } from '../../test/conversation.tsx';
import { nameBook } from '../shell-model.ts';
import { Conversation, INITIAL_UI } from './Conversation.tsx';
import { toolbarLabels as labelsOf } from '../../test/toolbar.ts';

function log(items: ReturnType<typeof peerRow>[], onOpenThread: ((msgId: string) => void) | undefined) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set(`pins:${GEN}`, []);
  const book = nameBook({ id: ME.id, username: 'ada' }, MEMBERS);
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <Conversation channelId={GEN} logKey="k" label="log" timeline={timeline(items)} book={book} encodeWith={[]} broadcast bits={null}
      pinsTitle="Pinned" ui={INITIAL_UI} dispatch={vi.fn()} loadingEarlier={false} onLoadEarlier={vi.fn()} onError={vi.fn()}
      {...(onOpenThread !== undefined ? { onOpenThread } : {})} />
  </CoreProvider></div>);
  return { view, user: userEvent.setup() };
}
const row = (id: string) => document.querySelector<HTMLElement>(`.d-message-row[data-msg-id="${id}"]`) as HTMLElement;
const toolbarLabels = (id: string) => labelsOf(within(row(id)).getByRole('toolbar', { name: 'actions for this message' }));

describe('thread entry points on a message (web3.7)', () => {
  it('puts reply in thread in the toolbar of a shown message and opens that message', async () => {
    const open = vi.fn();
    const { user } = log([peerRow(P1, '1', 'first'), peerRow(P2, '2', 'second')], open);
    expect(toolbarLabels(P2)).toContain(en['thread.open']);
    await user.click(within(row(P2)).getByRole('button', { name: en['thread.open'] }));
    expect(open).toHaveBeenCalledWith(P2);
  });
  it('shows the summary chip of a root: 1 reply, then a count', async () => {
    const open = vi.fn();
    const { user } = log([peerRow(P1, '1', 'first', { thread: { count: 1, lastTs: 1 } }), peerRow(P2, '2', 'second', { thread: { count: 3, lastTs: 2 } })], open);
    expect(within(row(P1)).getByRole('button', { name: en['thread.summary.one'] })).toBeInTheDocument();
    const chip = within(row(P2)).getByRole('button', { name: en['thread.summary.other'].replace('{count}', '3') });
    await user.click(chip);
    expect(open).toHaveBeenCalledWith(P2);
  });
  it('shows no chip for a message without a thread', () => {
    log([peerRow(P1, '1', 'first')], vi.fn());
    expect(within(row(P1)).queryByRole('button', { name: /^(1 reply|\d+ replies)$/ })).toBeNull();
  });
  it('offers neither the item nor the chip to a message that is not shown (pending or failed)', () => {
    log([ownRow(O1, null, 'waiting', { state: 'pending', thread: { count: 2, lastTs: 1 } })], vi.fn());
    expect(row(O1).querySelector('[role="toolbar"]')).toBeNull();
    expect(within(row(O1)).queryByRole('button', { name: en['thread.open'] })).toBeNull();
    expect(within(row(O1)).queryByRole('button', { name: /^(1 reply|\d+ replies)$/ })).toBeNull();
  });
  it('offers neither where no thread can be opened (the thread pane’s own rows)', () => {
    log([peerRow(P1, '1', 'first', { thread: { count: 2, lastTs: 1 } })], undefined);
    expect(toolbarLabels(P1)).not.toContain(en['thread.open']);
    expect(within(row(P1)).queryByRole('button', { name: /^(1 reply|\d+ replies)$/ })).toBeNull();
  });
  it('has no serious axe violations with a chip', async () => {
    const { view } = log([peerRow(P1, '1', 'first', { thread: { count: 2, lastTs: 1 } })], vi.fn());
    await expectNoAxeViolations(view.container);
  });
});
