import { render, screen, fireEvent } from '@testing-library/react';
import { describe, it, expect, vi } from 'vitest';
import { MentionList } from './MentionList.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const OPTIONS = [
  { id: 'mention-mira', primary: 'mira', secondary: '@mira' },
  { id: 'mention-mike', primary: 'Mike Dahl', secondary: '@mike' },
  { id: 'mention-everyone', primary: '@everyone', secondary: 'everyone in this channel' },
];

describe('MentionList', () => {
  it('is a named listbox of options carrying their ids, the active one selected', () => {
    render(<MentionList id="mentions" label="people to mention" options={OPTIONS} activeId="mention-mike" onPick={() => {}} />);
    const list = screen.getByRole('listbox', { name: 'people to mention' });
    expect(list).toHaveAttribute('id', 'mentions');
    expect(list.closest('.d-mention-list')).not.toBeNull();
    const options = screen.getAllByRole('option');
    expect(options.map((o) => o.id)).toEqual(['mention-mira', 'mention-mike', 'mention-everyone']);
    expect(options.map((o) => o.getAttribute('aria-selected'))).toEqual(['false', 'true', 'false']);
    expect(options[1]?.querySelector('.d-mention-list__primary')?.textContent).toBe('Mike Dahl');
    expect(options[1]?.querySelector('.d-mention-list__secondary')?.textContent).toBe('@mike');
    expect(options.every((o) => !o.hasAttribute('tabindex'))).toBe(true);
  });

  it('shows its title over the list and draws a person with an avatar, a role or @everyone with an @ tile', () => {
    const { container } = render(<MentionList id="mentions" label="people to mention" title="Members" options={[{ ...OPTIONS[0]!, person: true }, OPTIONS[2]!]} activeId={null} onPick={() => {}} />);
    expect(container.querySelector('.d-mention-list__title')).toHaveTextContent('Members');
    const [mira, everyone] = screen.getAllByRole('option');
    expect(mira?.querySelector('.d-avatar')).not.toBeNull();
    expect(everyone?.querySelector('.d-mention-list__icon svg')).not.toBeNull();
  });

  it('picks on pointer down without taking focus', () => {
    const onPick = vi.fn();
    render(<MentionList id="mentions" label="people to mention" options={OPTIONS} activeId={null} onPick={onPick} />);
    const notCancelled = fireEvent.mouseDown(screen.getAllByRole('option')[2]!);
    expect(notCancelled).toBe(false);
    expect(onPick).toHaveBeenCalledWith('mention-everyone');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><MentionList id="mentions" label="people to mention" options={OPTIONS} activeId="mention-mira" onPick={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
