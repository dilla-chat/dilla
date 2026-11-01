import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../core/context.tsx';
import { FakeClient } from '../test/fake-client.ts';
import { account } from '../test/fixtures.ts';
import { en } from '../strings/index.ts';
import { InviteFlow } from './Create.tsx';

// Moved from InvitesSection.test.tsx (Signal task 10c rewrote the Invites tab; the server menu's quick invite dialog stays).
const SERVER = 'a1'.repeat(16);
const EXPIRES = 1_900_000_000;

describe('InviteFlow more options', () => {
  function flow(onMoreOptions?: () => void) {
    const fake = new FakeClient();
    fake.set('account', account());
    fake.handler = () => Promise.resolve({ url: 'https://dilla.test/join/q', code: 'q', expires: EXPIRES });
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText: () => Promise.resolve() } });
    const onClose = vi.fn();
    const user = userEvent.setup();
    render(<div className="d-root"><CoreProvider client={fake}>
      <InviteFlow open community={{ id: SERVER, name: 'Midgard' }} onClose={onClose} onMoreOptions={onMoreOptions} />
    </CoreProvider></div>);
    return { fake, user, onClose };
  }
  it('keeps the quick path: ten uses, seven days, with no link to options when nobody wired it', async () => {
    const { fake } = flow();
    await screen.findByDisplayValue('https://dilla.test/join/q');
    expect(fake.callsOf('createInvite')).toEqual([{ m: 'createInvite', communityId: SERVER, maxUses: 10, ttlSeconds: 604_800 }]);
    expect(screen.queryByRole('button', { name: en['invite.options'] })).toBeNull();
  });
  it('offers more options, which closes the dialog and asks the shell for the invites tab', async () => {
    const onMore = vi.fn();
    const { user, onClose } = flow(onMore);
    await screen.findByDisplayValue('https://dilla.test/join/q');
    await user.click(await screen.findByRole('button', { name: en['invite.options'] }));
    expect(onMore).toHaveBeenCalledTimes(1);
    expect(onClose).toHaveBeenCalled();
  });
});
