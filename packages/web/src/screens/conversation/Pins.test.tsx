import { describe, it, expect } from 'vitest';
import { act, screen, within } from '@testing-library/react';
import type { PinnedItem } from '@dilla/client-core';
import { ME } from '../../test/fixtures.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { DM, GEN, NOW, P1, P2, PEER, peerRow, renderConversation, rowOf } from '../../test/conversation.tsx';
import { messageTime } from '../shell-model.ts';
import { rowMenuItem } from '../../test/row-menu.ts';
import type { UserEvent } from '@testing-library/user-event';

const PINNED_TS = 1700000000;
const pin = (over: Partial<PinnedItem> = {}): PinnedItem => ({ msgId: P1, seq: '1', senderUser: PEER, excerpt: 'pin me', pinnedBy: ME.id, pinnedSeq: '9', ts: PINNED_TS, threadRoot: null, ...over });
/** Ruling X (Jonas 2026-10-27): pins live in the channel row's context menu. */
const openPins = (user: UserEvent) => rowMenuItem(user, 'general', 'pinned');

describe('pins (Q20, ruling 23)', () => {
  it('opens from the header, loads the pins, names who pinned, unpins and closes', async () => {
    const { fake, user, view } = renderConversation({ items: [peerRow(P1, '1', 'pin me', { pinned: true })] });
    await openPins(user);
    expect(fake.callsOf('loadPins')).toEqual([{ m: 'loadPins', channelId: GEN }]);
    const dialog = screen.getByRole('dialog', { name: 'Pinned in #general' });
    expect(dialog).toHaveTextContent('Nothing is pinned here yet.');
    act(() => fake.set(`pins:${GEN}`, [pin()]));
    expect(dialog).toHaveTextContent('pin me');
    expect(dialog).toHaveTextContent('pinned by Ada L');
    expect(dialog).toHaveTextContent('bob');
    await expectNoAxeViolations(view.container);
    await user.click(within(dialog).getByRole('button', { name: 'unpin' }));
    expect(fake.callsOf('pin')).toEqual([{ m: 'pin', channelId: GEN, msgId: P1, on: false }]);
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(fake.callsOf('closePins')).toEqual([{ m: 'closePins', channelId: GEN }]);
  });
  it('jumps to a loaded pinned message and says when one is further back', async () => {
    const { fake, user } = renderConversation({ items: [peerRow(P1, '1', 'pin me', { pinned: true })] });
    await openPins(user);
    act(() => fake.set(`pins:${GEN}`, [pin(), pin({ msgId: P2, seq: '0', excerpt: 'older' })]));
    const dialog = screen.getByRole('dialog', { name: 'Pinned in #general' });
    // The time of a pin whose target is not loaded comes from PinnedItem.ts (ruling 36). messageTime takes
    // milliseconds (shell-model.ts:83); NOW is in seconds, and 1700000000 is never today, so the string is stable.
    const older = within(dialog).getByText('older').closest('li') as HTMLElement;
    expect(older).toHaveTextContent(messageTime(PINNED_TS, NOW * 1000));
    const jumps = within(dialog).getAllByRole('button', { name: 'go to message' });
    await user.click(jumps[0]);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(rowOf(P1)).toHaveFocus();
    expect(rowOf(P1)).toHaveAttribute('data-flash', 'true');
    expect(fake.callsOf('closePins')).toEqual([{ m: 'closePins', channelId: GEN }]);
    await openPins(user);
    await user.click(within(screen.getByRole('dialog', { name: 'Pinned in #general' })).getAllByRole('button', { name: 'go to message' })[1]);
    expect(screen.getByText('The original message is further back. Load earlier messages to reach it.')).toBeInTheDocument();
  });
  it('titles a DM’s pins by the DM’s name', async () => {
    const { fake, user } = renderConversation({ dm: true });
    // Ruling Z: a DM's pins are in its row's context menu, as a channel's are.
    await rowMenuItem(user, 'bob', 'pinned', 'dms');
    expect(screen.getByRole('dialog', { name: 'Pinned with bob' })).toBeInTheDocument();
    expect(fake.callsOf('loadPins')).toEqual([{ m: 'loadPins', channelId: DM }]);
  });
});

describe('pin focus handoff (F4)', () => {
  it('keeps the dialog open and moves unpin to the next item, else Close', async () => {
    const { fake, user } = renderConversation();
    await openPins(user);
    const p3 = '0d'.repeat(16);
    act(() => fake.set(`pins:${GEN}`, [pin(), pin({ msgId: P2, seq: '2' }), pin({ msgId: p3, seq: '3' })]));
    const dialog = screen.getByRole('dialog', { name: 'Pinned in #general' });
    await user.click(within(dialog).getAllByRole('button', { name: 'unpin' })[0]);
    act(() => fake.set(`pins:${GEN}`, [pin({ msgId: P2, seq: '2' }), pin({ msgId: p3, seq: '3' })]));
    expect(dialog).toBeInTheDocument();
    expect(within(dialog).getAllByRole('button', { name: 'unpin' })[0]).toHaveFocus();
    await user.click(within(dialog).getAllByRole('button', { name: 'unpin' })[1]);
    act(() => fake.set(`pins:${GEN}`, [pin({ msgId: P2, seq: '2' })]));
    expect(within(dialog).getByRole('button', { name: 'Close' })).toHaveFocus();
  });
});
