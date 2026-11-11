import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { ScreenSourcePicker, type ScreenSourceTile } from './ScreenSourcePicker.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const labels = { title: 'Share your screen', screens: 'Screens', windows: 'Windows', share: 'Share', cancel: 'Cancel', empty: 'Nothing to share here' };
const sources: ScreenSourceTile[] = [
  { id: 's1', name: 'Built-in', kind: 'screen', thumbnail: 'blob:s1', icon: null },
  { id: 's2', name: 'Studio Display', kind: 'screen', thumbnail: null, icon: null },
  { id: 'w1', name: 'Notes', kind: 'window', thumbnail: 'blob:w1', icon: 'blob:i1' },
  { id: 'w2', name: 'Terminal', kind: 'window', thumbnail: 'blob:w2', icon: null },
];
function setup(list = sources) {
  const onShare = vi.fn(), onCancel = vi.fn();
  const view = render(<ScreenSourcePicker open sources={list} labels={labels} onShare={onShare} onCancel={onCancel} />);
  return { onShare, onCancel, view };
}

describe('ScreenSourcePicker', () => {
  it('is a modal named "Share your screen" with Screens and Windows tabs over a listbox of tiles', async () => {
    const { view } = setup();
    const dialog = screen.getByRole('dialog', { name: 'Share your screen' });
    const tabs = within(dialog).getAllByRole('tab');
    expect(tabs.map(t => t.textContent)).toEqual(['Screens', 'Windows']);
    expect(within(dialog).getByRole('tablist')).toBeInTheDocument();
    expect(tabs[0]).toHaveAttribute('aria-selected', 'true');
    const panel = within(dialog).getByRole('tabpanel');
    expect(tabs[0]).toHaveAttribute('aria-controls', panel.id);
    expect(panel).toHaveAccessibleName('Screens');
    expect(within(panel).getAllByRole('option').map(o => o.textContent)).toEqual(['Built-in', 'Studio Display']);
    await expectNoAxeViolations(view.container);
  });

  it('switches tabs by click and by arrow keys; windows show their app icon', async () => {
    setup();
    await userEvent.click(screen.getByRole('tab', { name: 'Windows' }));
    expect(screen.getByRole('tab', { name: 'Windows' })).toHaveAttribute('aria-selected', 'true');
    const options = screen.getAllByRole('option');
    expect(options.map(o => o.textContent)).toEqual(['Notes', 'Terminal']);
    expect(options[0].querySelector('.d-screen-picker__icon')).not.toBeNull();
    screen.getByRole('tab', { name: 'Windows' }).focus();
    await userEvent.keyboard('{ArrowLeft}');
    expect(screen.getByRole('tab', { name: 'Screens' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tab', { name: 'Screens' })).toHaveFocus();
  });

  it('keeps Share disabled until a tile is selected', async () => {
    const { onShare } = setup();
    const share = screen.getByRole('button', { name: 'Share' });
    expect(share).toBeDisabled();
    await userEvent.click(screen.getByRole('option', { name: 'Studio Display' }));
    expect(screen.getByRole('option', { name: 'Studio Display' })).toHaveAttribute('aria-selected', 'true');
    expect(share).toBeEnabled();
    await userEvent.click(share);
    expect(onShare).toHaveBeenCalledWith('s2');
  });

  it('moves between tiles with the arrow keys and shares the focused tile with Enter', async () => {
    const { onShare } = setup();
    await userEvent.click(screen.getByRole('option', { name: 'Built-in' }));
    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('option', { name: 'Studio Display' })).toHaveFocus();
    expect(screen.getByRole('option', { name: 'Studio Display' })).toHaveAttribute('aria-selected', 'true');
    await userEvent.keyboard('{ArrowRight}');
    expect(screen.getByRole('option', { name: 'Studio Display' })).toHaveFocus();
    await userEvent.keyboard('{ArrowLeft}');
    expect(screen.getByRole('option', { name: 'Built-in' })).toHaveFocus();
    await userEvent.keyboard('{End}');
    expect(screen.getByRole('option', { name: 'Studio Display' })).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(onShare).toHaveBeenCalledWith('s2');
  });

  it('shares on double-click', async () => {
    const { onShare } = setup();
    await userEvent.dblClick(screen.getByRole('option', { name: 'Built-in' }));
    expect(onShare).toHaveBeenCalledWith('s1');
  });

  it('Escape and Cancel start nothing', async () => {
    const { onShare, onCancel } = setup();
    await userEvent.click(screen.getByRole('option', { name: 'Built-in' }));
    await userEvent.keyboard('{Escape}');
    expect(onCancel).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(2);
    expect(onShare).not.toHaveBeenCalled();
  });

  it('drops a selection whose source vanished in a refresh, and says when a tab is empty', async () => {
    const { view, onShare, onCancel } = setup();
    await userEvent.click(screen.getByRole('option', { name: 'Studio Display' }));
    view.rerender(<ScreenSourcePicker open sources={sources.filter(s => s.id !== 's2')} labels={labels} onShare={onShare} onCancel={onCancel} />);
    expect(screen.getByRole('button', { name: 'Share' })).toBeDisabled();
    view.rerender(<ScreenSourcePicker open sources={sources.filter(s => s.kind === 'screen')} labels={labels} onShare={onShare} onCancel={onCancel} />);
    await userEvent.click(screen.getByRole('tab', { name: 'Windows' }));
    expect(screen.getByText('Nothing to share here')).toBeInTheDocument();
  });
});
