import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ManageList, type ManageListProps } from './ManageList.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

function props(over: Partial<ManageListProps> = {}): ManageListProps {
  return {
    label: 'invites', emptyLabel: 'No invites yet.', busyId: null, onAction: vi.fn(),
    rows: [
      { id: 'i1', title: 'Invite 1', detail: ['made by Astrid', '3 of 10 uses'], action: { label: 'revoke', ariaLabel: 'revoke invite 1' } },
      { id: 'i2', title: 'Invite 2', detail: ['made by Bjorn'], action: { label: 'revoke', ariaLabel: 'revoke invite 2' } },
      { id: 'i3', title: 'Invite 3', detail: ['ended'], action: null },
    ],
    ...over,
  };
}

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('ManageList', () => {
  it('is a labelled list of rows with their title and detail lines', () => {
    render(<ManageList {...props()} />);
    const list = screen.getByRole('list', { name: 'invites' });
    expect(list).toBeInTheDocument();
    const rows = screen.getAllByRole('listitem');
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent('Invite 1');
    expect(rows[0]).toHaveTextContent('made by Astrid');
    expect(rows[0]).toHaveTextContent('3 of 10 uses');
  });

  it('shows the empty label and no list when there are no rows', () => {
    render(<ManageList {...props({ rows: [] })} />);
    expect(screen.getByText('No invites yet.')).toBeInTheDocument();
    expect(screen.queryByRole('list')).toBeNull();
  });

  it('uses the action’s ariaLabel as the button’s accessible name, and reports its row', async () => {
    const user = userEvent.setup();
    const p = props();
    render(<ManageList {...p} />);
    expect(screen.queryByRole('button', { name: 'revoke' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'revoke invite 2' }));
    expect(p.onAction).toHaveBeenCalledExactlyOnceWith('i2');
  });

  it('a row without an action has no button', () => {
    render(<ManageList {...props()} />);
    expect(screen.getAllByRole('button')).toHaveLength(2);
  });

  it('busyId disables only that row’s action', async () => {
    const user = userEvent.setup();
    const p = props({ busyId: 'i1' });
    render(<ManageList {...p} />);
    const busy = screen.getByRole('button', { name: 'revoke invite 1' });
    expect(busy).toBeDisabled();
    expect(screen.getByRole('button', { name: 'revoke invite 2' })).toBeEnabled();
    await user.click(busy);
    expect(p.onAction).not.toHaveBeenCalled();
  });

  it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s, listed, busy and empty', async theme => {
    document.documentElement.dataset.theme = theme;
    for (const over of [{}, { busyId: 'i1' }, { rows: [] }]) {
      const { container, unmount } = render(<div className="d-root"><ManageList {...props(over)} /></div>);
      await expectNoAxeViolations(container);
      unmount();
    }
  });

  // I6: admin rows carry icon actions (edit, delete) in the trailing slot; a fixed row (the AFK channel) has no delete.
  describe('icon actions', () => {
    const channels = (over: Partial<ManageListProps> = {}): ManageListProps => props({
      label: 'channels', onEdit: vi.fn(), onDelete: vi.fn(),
      rows: [
        { id: 'c1', title: 'general', detail: [], action: null, edit: { ariaLabel: 'edit general' }, remove: { ariaLabel: 'delete general' } },
        { id: 'c2', title: 'AFK', detail: [], action: null, fixed: true, edit: { ariaLabel: 'edit AFK' }, remove: { ariaLabel: 'delete AFK' } },
      ],
      ...over,
    });

    it('names the icon buttons by their aria labels and reports the row', async () => {
      const user = userEvent.setup();
      const p = channels();
      render(<ManageList {...p} />);
      await user.click(screen.getByRole('button', { name: 'edit general' }));
      expect(p.onEdit).toHaveBeenCalledExactlyOnceWith('c1');
      await user.click(screen.getByRole('button', { name: 'delete general' }));
      expect(p.onDelete).toHaveBeenCalledExactlyOnceWith('c1');
    });

    it('a fixed row has no delete action but keeps its edit (control: the other row has both)', () => {
      render(<ManageList {...channels()} />);
      expect(screen.getByRole('button', { name: 'edit AFK' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'delete AFK' })).toBeNull();
      expect(screen.getByRole('button', { name: 'delete general' })).toBeInTheDocument();
    });

    it('a fixed row still reserves the delete slot, so the edit icons stay on one line', () => {
      render(<ManageList {...channels()} />);
      const rows = screen.getAllByRole('listitem');
      expect(rows[0].querySelectorAll('.d-slot')).toHaveLength(2);
      expect(rows[1].querySelectorAll('.d-slot')).toHaveLength(2);
    });

    it('busyId disables both icons of that row only', () => {
      render(<ManageList {...channels({ busyId: 'c1' })} />);
      expect(screen.getByRole('button', { name: 'edit general' })).toBeDisabled();
      expect(screen.getByRole('button', { name: 'delete general' })).toBeDisabled();
      expect(screen.getByRole('button', { name: 'edit AFK' })).toBeEnabled();
    });

    it('draws the square-pen and x icons', () => {
      render(<ManageList {...channels()} />);
      expect(screen.getByRole('button', { name: 'edit general' }).querySelector('[data-icon="square-pen"]')).not.toBeNull();
      expect(screen.getByRole('button', { name: 'delete general' }).querySelector('[data-icon="x"]')).not.toBeNull();
    });

    it('a text action (privacy rows, I7) is one ghost button in the trailing area', () => {
      render(<ManageList {...props()} />);
      const button = screen.getByRole('button', { name: 'revoke invite 1' });
      expect(button).toHaveClass('d-btn');
      expect(button.closest('.d-manage-list__trail')).not.toBeNull();
    });

    it.each(['signal', 'light', 'high-contrast'])('has no serious axe violations in %s', async theme => {
      document.documentElement.dataset.theme = theme;
      const { container } = render(<div className="d-root"><ManageList {...channels()} /></div>);
      await expectNoAxeViolations(container);
    });
  });
});

// M1: in a phone frame the icon buttons are the 44px primary tier and the slots are 44 wide. A CSS-text pin: jsdom has no layout.
describe('ManageList in a phone frame', () => {
  it('sizes the icon buttons and the trailing slot to 44px, and lets the name shrink', async () => {
    const fs = await import(/* @vite-ignore */ 'node:fs' as string) as { readFileSync(path: string, encoding: 'utf8'): string };
    const cwd = (globalThis as unknown as { process: { cwd(): string } }).process.cwd();
    const strip = (c: string) => c.replace(/\/\*[\s\S]*?\*\//g, '');
    const base = strip(fs.readFileSync(`${cwd}/src/styles/base.css`, 'utf8'));
    const list = strip(fs.readFileSync(`${cwd}/src/ManageList/ManageList.css`, 'utf8'));
    const tier = /\.d-settings-frame\[data-layout="phone"\] :is\([^)]*\.d-icon-button[^)]*\) \{([^}]*)\}/.exec(base)?.[1] ?? '';
    expect(tier).toMatch(/min-height: 2\.75rem;/);
    expect(tier).toMatch(/min-width: 2\.75rem;/);
    expect(/\.d-settings-frame\[data-layout="phone"\] \{([^}]*)\}/.exec(base)?.[1] ?? '').toMatch(/--slot-w: 44px;/);
    expect(/\.d-manage-list__text \{ flex: 1 1 0;/.test(list)).toBe(true);
  });
});
