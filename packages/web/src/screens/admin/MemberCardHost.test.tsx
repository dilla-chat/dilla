import { describe, it, expect, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import type { MemberSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { account, ME } from '../../test/fixtures.ts';
import { t } from '../../strings/index.ts';
import { MemberCardHost } from './MemberCardHost.tsx';

const CID = 'a1'.repeat(16);
const PEER = '28'.repeat(16);
const peer = (over: Partial<MemberSummary> = {}): MemberSummary => ({ userId: PEER, username: 'bob', display: 'Bob', kind: 0, roleIds: [], ...over });

function open(member: MemberSummary) {
  const fake = new FakeClient();
  fake.set('account', account());
  fake.set('communities', [{ id: CID, name: 'Midgard', owner: ME.id }]);
  fake.set(`members:${CID}`, [member]);
  fake.set(`presence:${CID}`, {});
  fake.set(`roles:${CID}`, []);
  const anchor = document.createElement('button');
  document.body.appendChild(anchor);
  return render(
    <div className="d-root"><CoreProvider client={fake}>
      <MemberCardHost communityId={CID} selfId={ME.id} card={{ id: PEER, source: 'mention', anchor }} onCard={vi.fn()}
        onMessage={vi.fn()} onCompare={vi.fn()} announce={vi.fn()} />
    </CoreProvider></div>,
  );
}

describe('the member profile card (EOD E6)', () => {
  it('reanchors a list card only to the row inside its scoped workspace', () => {
    const fake = new FakeClient();
    fake.set('account', account());
    fake.set('communities', [{ id: CID, name: 'Midgard', owner: ME.id }]);
    fake.set(`members:${CID}`, [peer()]);
    fake.set(`presence:${CID}`, {});
    fake.set(`roles:${CID}`, []);
    const background = document.createElement('button');
    background.dataset.focusKey = `member:${PEER}`;
    document.body.append(background);
    const scope = document.createElement('div');
    const row = document.createElement('button');
    row.dataset.focusKey = `member:${PEER}`;
    scope.append(row);
    const staleAnchor = document.createElement('button');
    const onCard = vi.fn();
    render(<div className="d-root"><CoreProvider client={fake}><MemberCardHost communityId={CID} selfId={ME.id}
      card={{ id: PEER, source: 'list', anchor: staleAnchor }} onCard={onCard} onMessage={vi.fn()} onCompare={vi.fn()}
      announce={vi.fn()} focusScope={scope} /></CoreProvider></div>);
    expect(onCard).toHaveBeenCalledWith(expect.objectContaining({ anchor: row }));
    background.remove();
  });
  it('shows the member status message after the presence word, and the bio under the head', () => {
    open(peer({ statusMsg: 'on a call', bio: 'Builds things.\nSecond line.' }));
    const card = screen.getByRole('dialog', { name: 'Bob' });
    expect(within(card).getByText('on a call')).toBeInTheDocument();
    expect(within(card).getByRole('group', { name: t('profile.card.about') })).toHaveTextContent('Builds things. Second line.');
  });

  it('shows neither when the member has set neither', () => {
    open(peer());
    const card = screen.getByRole('dialog', { name: 'Bob' });
    expect(within(card).queryByRole('group', { name: t('profile.card.about') })).toBeNull();
    expect(card.querySelector('.d-profile-card__status')?.textContent).toBe('@bob · offline');
  });
});
