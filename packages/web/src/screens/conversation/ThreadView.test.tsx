import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { PERM, type ThreadState } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { account } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { formatTime } from '../../strings/index.ts';
import { A, GEN, NOW, MEMBERS, O1, P1, P2, PEER, ownRow, peerRow } from '../../test/conversation.tsx';
import { ThreadView } from './ThreadView.tsx';
import type { ProfileOpener } from './MessageItem.tsx';
import { toolbarButton, toolbarLabels } from '../../test/toolbar.ts';

const ROOT = P1;

function thread(over: Partial<ThreadState> = {}): ThreadState {
  return {
    channelId: GEN, rootMsgId: ROOT, root: peerRow(ROOT, '1', 'the root post', { thread: { count: 2, lastTs: 3 } }),
    items: [peerRow(P2, '2', 'first reply'), ownRow(O1, '3', 'my reply')], hasEarlier: false, ...over,
  };
}

function setup(over: { thread?: ThreadState | null; phone?: boolean; initialDraft?: string; blocked?: string | null; onDraftChange?: (text: string) => void; onOpenProfile?: ProfileOpener } = {}) {
  window.history.replaceState(null, '', `/c/${A}/${GEN}`);
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set(`members:${A}`, MEMBERS);
  fake.set(`thread:${GEN}`, over.thread === undefined ? thread() : over.thread);
  const onClose = vi.fn();
  const announce = vi.fn();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}>
    <ThreadView channelId={GEN} rootMsgId={ROOT} channelName="general" phone={over.phone ?? false} onClose={onClose} announce={announce}
      initialDraft={over.initialDraft} blocked={over.blocked} onDraftChange={over.onDraftChange} onOpenProfile={over.onOpenProfile} />
  </CoreProvider></div>);
  return { fake, user, view, onClose, announce };
}
const box = () => screen.getByRole('textbox', { name: en['thread.composer'] });

describe('ThreadView profile opens (MP7)', () => {
  it('opens the author card from the root post and from a mention in a reply', async () => {
    const onOpenProfile = vi.fn();
    const { user } = setup({ onOpenProfile, thread: thread({ items: [peerRow(P2, '2', `hey <@${PEER}>`)] }) });
    const root = document.querySelector('.d-thread-pane__root') as HTMLElement;
    await user.click(within(root).getByRole('button', { name: 'bob' }));
    expect(onOpenProfile).toHaveBeenLastCalledWith(PEER, expect.any(HTMLElement), 'author', 'bob');
    await user.click(within(document.querySelector('.d-thread-pane__replies') as HTMLElement).getByRole('button', { name: '@bob' }));
    expect(onOpenProfile).toHaveBeenLastCalledWith(PEER, expect.any(HTMLElement), 'mention', 'bob');
  });
  it('shows no buttons without a handler', () => {
    setup();
    expect(document.querySelector('.d-message-row__author-open')).toBeNull();
  });
});

describe('ThreadView (web3.7)', () => {
  it('blocks thread replies when the community timeout is active', () => {
    setup({ blocked: 'You cannot post messages in this community until 11:20 AM.' });
    expect(box()).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText('You cannot post messages in this community until 11:20 AM.')).toBeInTheDocument();
  });
  it('restores a thread draft and reports later edits for update recovery', async () => {
    const onDraftChange = vi.fn();
    const { user } = setup({ initialDraft: 'before refresh', onDraftChange });
    expect(box()).toHaveValue('before refresh');
    await user.type(box(), '!');
    expect(onDraftChange).toHaveBeenLastCalledWith('before refresh!');
  });
  it('opens the thread on mount and closes it on unmount, in that order', () => {
    const { fake, view } = setup();
    expect(fake.calls).toEqual([{ m: 'openThread', channelId: GEN, rootMsgId: ROOT }]);
    view.unmount();
    expect(fake.calls).toEqual([{ m: 'openThread', channelId: GEN, rootMsgId: ROOT }, { m: 'closeThread', channelId: GEN }]);
  });
  it('shows the title, a close control, the root and the replies', () => {
    const { onClose } = setup();
    expect(screen.getByRole('heading', { level: 2, name: en['thread.title'] })).toBeInTheDocument();
    const root = document.querySelector('.d-thread-pane__root') as HTMLElement;
    expect(within(root).getByText('the root post')).toBeInTheDocument();
    const replies = document.querySelector('.d-thread-pane__replies') as HTMLElement;
    expect(within(replies).getAllByText(/reply$/).map(e => e.textContent)).toEqual(['first reply', 'my reply']);
    screen.getByRole('button', { name: en['thread.close'] }).click();
    expect(onClose).toHaveBeenCalledTimes(1);
  });
  it('says the root cannot be shown when it is not held, and still lists the replies', () => {
    setup({ thread: thread({ root: null }) });
    expect(screen.getByText(en['thread.rootMissing'])).toBeInTheDocument();
    expect(screen.getByText('first reply')).toBeInTheDocument();
  });
  it('shows no root text while the thread is still being read', () => {
    setup({ thread: null });
    expect(screen.queryByText(en['thread.rootMissing'])).toBeNull();
    expect(box()).toBeInTheDocument();
  });
  it('is a sheet with its own header on a phone', () => {
    setup({ phone: true });
    expect(document.querySelector('.d-thread-pane')).toHaveAttribute('data-phone', 'true');
    expect(screen.getByRole('button', { name: en['thread.close'] })).toBeInTheDocument();
  });
  it('focuses the thread composer on opening', () => {
    setup();
    expect(box()).toHaveFocus();
  });
  it('focuses no field on a phone (script focus opens no keyboard there)', () => {
    setup({ phone: true });
    expect(box()).not.toHaveFocus();
  });
  it('closes on Escape inside it', async () => {
    const { user, onClose } = setup();
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
  });
  it('sends a reply with the thread id, clears the draft once it is sent and keeps the focus', async () => {
    const { fake, user } = setup();
    await user.type(box(), 'a new reply');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(fake.callsOf('send')).toEqual([{ m: 'send', channelId: GEN, text: 'a new reply', threadId: ROOT }]));
    await waitFor(() => expect(box()).toHaveValue(''));
    expect(box()).toHaveFocus();
  });
  it('keeps the draft and says why when a send is refused', async () => {
    const { fake, user } = setup();
    fake.handler = c => (c.m === 'send' ? Promise.reject(refusal({ code: 'E_NETWORK' })) : Promise.resolve(null));
    await user.type(box(), 'will fail');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('alert')).toHaveTextContent('E_NETWORK');
    expect(box()).toHaveValue('will fail');
  });
  it('shows the slow-mode wait of the channel in the thread composer too', () => {
    const fake = new FakeClient();
    fake.set(`slow:${GEN}`, { until: Date.now() + 30_000, queued: false });
    window.history.replaceState(null, '', `/c/${A}/${GEN}`);
    fake.set('account', account());
    fake.set(`members:${A}`, MEMBERS);
    fake.set(`thread:${GEN}`, thread());
    render(<div className="d-root"><CoreProvider client={fake}>
      <ThreadView channelId={GEN} rootMsgId={ROOT} channelName="general" phone={false} onClose={vi.fn()} announce={vi.fn()} />
    </CoreProvider></div>);
    expect(document.querySelector('.d-composer__notice')).toHaveTextContent('Slow mode is on.');
  });
  const toolbar = (id: string) => within(document.querySelector(`.d-thread-pane .d-message-row[data-msg-id="${id}"]`) as HTMLElement)
    .getByRole('toolbar', { name: 'actions for this message' });
  const labels = (id: string) => toolbarLabels(toolbar(id));
  it('offers react on every row, edit and delete on own replies, and no reply, pin or thread', () => {
    setup();
    expect(labels(P2)).toEqual([en['shell.message.react']]);
    expect(labels(O1)).toEqual([en['shell.message.react'], en['shell.message.edit'], en['shell.message.delete']]);
  });
  it('edits an own reply', async () => {
    const { fake, user } = setup();
    await user.click(toolbarButton(toolbar(O1), en['shell.message.edit']));
    const editor = screen.getByRole('textbox', { name: en['shell.edit.label'] });
    expect(editor).toHaveValue('my reply');
    await user.clear(editor);
    await user.type(editor, 'my better reply');
    await user.click(screen.getByRole('button', { name: en['shell.edit.save'] }));
    expect(fake.callsOf('editMessage')).toEqual([{ m: 'editMessage', channelId: GEN, msgId: O1, text: 'my better reply' }]);
    expect(screen.queryByRole('textbox', { name: en['shell.edit.label'] })).toBeNull();
  });
  it('deletes an own reply after asking', async () => {
    const { fake, user } = setup();
    await user.click(toolbarButton(toolbar(O1), en['shell.message.delete']));
    expect(fake.callsOf('deleteMessage')).toEqual([]);
    await user.click(screen.getByRole('button', { name: en['shell.delete.confirm'] }));
    expect(fake.callsOf('deleteMessage')).toEqual([{ m: 'deleteMessage', channelId: GEN, msgId: O1, seq: '3' }]);
    expect(fake.callsOf('moderateRemoveMessage')).toEqual([]);
  });
  it('offers the separate moderator action on a peer reply and sends its timeline sequence', async () => {
    const { fake, user } = setup();
    fake.handler = command => command.m === 'moderateRemoveMessage' ? Promise.resolve({ removal: 'unknown',
      proofRequestId: '11'.repeat(16), evidenceRequestId: '22'.repeat(16), evidenceUpload: 'unknown' })
      : command.m === 'resolveManualRemoval' ? Promise.resolve({ removal: 'accepted', evidenceUpload: 'pending' })
      : command.m === 'humanInteractionRequest' ? Promise.resolve({ upload: 'accepted' }) : Promise.resolve(null);
    const everyone = 'e0'.repeat(16);
    const moderators = 'a0'.repeat(16);
    const self = account().user?.id ?? 'bb'.repeat(16);
    fake.set('communities', [{ id: A, name: 'Midgard', owner: 'ee'.repeat(16) }]);
    fake.set(`members:${A}`, [{ userId: self, username: 'ada', display: '', kind: 0, roleIds: [moderators] }]);
    fake.set(`roles:${A}`, [
      { id: everyone, name: '@everyone', color: 0, position: 0, hoist: false, mentionable: false, allow: PERM.viewChannel, deny: 0, members: 1 },
      { id: moderators, name: 'Moderators', color: 0, position: 1, hoist: false, mentionable: false, allow: PERM.manageMessages, deny: 0, members: 1 },
    ] as never);
    fake.set(`permissions:${A}`, { top: 0, owner: false, manageCommunity: false, manageChannels: false, kickMembers: false,
      banMembers: false, createInvite: false, manageRoles: false, loaded: true, everyoneId: everyone, overwritesLoaded: true });
    fake.set(`overwrites:${A}`, {});
    const action = await waitFor(() => toolbarButton(toolbar(P2), en['shell.message.moderateRemove']));
    await user.click(action);
    expect(fake.callsOf('moderateRemoveMessage')).toEqual([]);
    const dialog = screen.getByRole('dialog', { name: en['shell.moderation.removeTitle'] });
    const reason = within(dialog).getByLabelText(en['shell.moderation.reason']);
    expect(within(dialog).getByRole('button', { name: en['shell.moderation.removeConfirm'] })).toHaveAttribute('aria-disabled', 'true');
    await user.type(reason, 'reviewed by moderator');
    expect(within(dialog).getByRole('button', { name: en['shell.moderation.removeConfirm'] })).not.toHaveAttribute('aria-disabled', 'true');
    await user.click(within(dialog).getByRole('button', { name: en['shell.moderation.removeConfirm'] }));
    expect(fake.callsOf('moderateRemoveMessage')).toEqual([{ m: 'moderateRemoveMessage', channelId: GEN, msgId: P2, seq: '2',
      reason: 'reviewed by moderator', excerpt: 'first reply' }]);
    expect(await screen.findByText(en['shell.moderation.removalUnknown'])).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: en['shell.moderation.retryRemoval'] }));
    expect(await screen.findByText(en['shell.moderation.evidencePending'])).toBeInTheDocument();
    expect(fake.callsOf('resolveManualRemoval')).toEqual([{ m: 'resolveManualRemoval', proofRequestId: '11'.repeat(16) }]);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: en['shell.delete.cancel'] }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByText(en['shell.moderation.evidencePending'])).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: en['shell.moderation.retryEvidence'] }));
    await waitFor(() => expect(screen.getByText(en['shell.moderation.evidenceSent'])).toBeInTheDocument());
    expect(fake.callsOf('moderateRemoveMessage')).toHaveLength(1);
    expect(fake.callsOf('humanInteractionRequest')).toEqual([expect.objectContaining({ operation: 61, communityId: A,
      channelId: null, args: ['11'.repeat(16), 'reviewed by moderator', 'first reply'], revision: '0' })]);
    expect(fake.callsOf('deleteMessage')).toEqual([]);
  });
  it('says there are no replies yet, but not while the thread is still being read', () => {
    setup({ thread: thread({ items: [] }) });
    expect(screen.getByText(en['thread.noReplies'])).toBeInTheDocument();
  });
  it('shows no empty line while reading', () => {
    setup({ thread: null });
    expect(screen.queryByText(en['thread.noReplies'])).toBeNull();
  });
  it('toggles a reaction on a reply', async () => {
    const reaction = { emoji: '👍', count: 1, mine: false };
    const { fake, user } = setup({ thread: thread({ items: [peerRow(P2, '2', 'first reply', { reactions: [reaction] })] }) });
    await user.click(screen.getByRole('button', { name: /thumbs|👍/ }));
    expect(fake.callsOf('react')).toEqual([{ m: 'react', channelId: GEN, msgId: P2, emoji: '👍', on: true }]);
  });
  it('reaches the root row and its toolbar by keyboard, like the replies', async () => {
    const { user } = setup();
    const rootRow = document.querySelector('.d-thread-pane__root .d-message-row') as HTMLElement;
    // The active (last) row is the one tab stop of the rows: the root is reached with the arrows and Home.
    const lastRow = document.querySelector(`.d-thread-pane .d-message-row[data-msg-id="${O1}"]`) as HTMLElement;
    expect(lastRow.tabIndex).toBe(0);
    expect(rootRow.tabIndex).toBe(-1);
    lastRow.focus();
    await user.keyboard('{Home}');
    expect(rootRow).toHaveFocus();
    expect(rootRow.tabIndex).toBe(0);
    await user.tab();
    expect(within(rootRow).getByRole('button', { name: en['shell.message.react'] })).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(screen.getAllByRole('dialog', { name: en['shell.emoji.label'] }).length).toBeGreaterThan(0);
  });
  it('takes the root as the tab stop when there are no replies', () => {
    setup({ thread: thread({ items: [] }) });
    expect((document.querySelector('.d-thread-pane__root .d-message-row') as HTMLElement).tabIndex).toBe(0);
  });
  it('shows times as the channel rows do', () => {
    setup();
    const row = document.querySelector(`.d-thread-pane .d-message-row[data-msg-id="${P2}"]`) as HTMLElement;
    expect(row).toHaveTextContent(formatTime(NOW));
  });
  it('does not announce a queued send itself (the channel’s composer does, once)', () => {
    window.history.replaceState(null, '', `/c/${A}/${GEN}`);
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set(`members:${A}`, MEMBERS);
    fake.set(`thread:${GEN}`, thread());
    fake.set(`slow:${GEN}`, { until: Date.now() + 30_000, queued: true });
    const announce = vi.fn();
    render(<div className="d-root"><CoreProvider client={fake}>
      <ThreadView channelId={GEN} rootMsgId={ROOT} channelName="general" phone={false} onClose={vi.fn()} announce={announce} />
    </CoreProvider></div>);
    expect(document.querySelector('.d-composer__notice')).toHaveTextContent(en['slow.queued']);
    expect(announce).not.toHaveBeenCalled();
  });
  it('returns focus to the control that opened it when it closes (contract 2)', async () => {
    window.history.replaceState(null, '', `/c/${A}/${GEN}`);
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set(`members:${A}`, MEMBERS);
    fake.set(`thread:${GEN}`, thread());
    function Host() {
      const [open, setOpen] = useState(false);
      return <div className="d-root"><button type="button" onClick={() => setOpen(true)}>open it</button>
        {open ? <ThreadView channelId={GEN} rootMsgId={ROOT} channelName="general" phone={false} onClose={() => setOpen(false)} announce={vi.fn()} /> : null}</div>;
    }
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><Host /></CoreProvider>);
    await user.click(screen.getByRole('button', { name: 'open it' }));
    expect(box()).toHaveFocus();
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'open it' })).toHaveFocus());
  });
  it('returns focus to the root’s row when the opener is gone', async () => {
    window.history.replaceState(null, '', `/c/${A}/${GEN}`);
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set(`members:${A}`, MEMBERS);
    fake.set(`thread:${GEN}`, thread());
    function Host() {
      const [open, setOpen] = useState(false);
      const [chip, setChip] = useState(true);
      return <div className="d-root">
        <div className="d-message-row" data-msg-id={ROOT} tabIndex={-1}>root row
          {chip ? <button type="button" onClick={() => { setOpen(true); setChip(false); }}>chip</button> : null}</div>
        {open ? <ThreadView channelId={GEN} rootMsgId={ROOT} channelName="general" phone={false} onClose={() => setOpen(false)} announce={vi.fn()} /> : null}</div>;
    }
    const user = userEvent.setup();
    render(<CoreProvider client={fake}><Host /></CoreProvider>);
    await user.click(screen.getByRole('button', { name: 'chip' }));
    await user.click(screen.getByRole('button', { name: en['thread.close'] }));
    await waitFor(() => expect(document.querySelector('.d-message-row[data-msg-id]')).toHaveFocus());
  });
  it('has no serious axe violations, as a column and as a sheet', async () => {
    const { view } = setup();
    await expectNoAxeViolations(view.container);
    view.unmount();
    const sheet = setup({ phone: true });
    await expectNoAxeViolations(sheet.view.container);
  });
});

void act; void PEER;
