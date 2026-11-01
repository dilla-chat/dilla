import { act, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { NowPlaying, playerTime } from './NowPlaying.tsx';
import { npHarbour } from './np.fixture.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const handlers = () => ({ onToggle: vi.fn(), onSkip: vi.fn(), onStop: vi.fn() });
afterEach(() => { vi.useRealTimers(); });

describe('NowPlaying', () => {
  it('formats times as m:ss', () => {
    expect(playerTime(37)).toBe('0:37');
    expect(playerTime(214)).toBe('3:34');
    expect(playerTime(-3)).toBe('0:00');
  });

  it('card: a named group with the state, title, meta, named controls and a progress bar with an accessible value', async () => {
    const h = handlers();
    const { container } = render(<NowPlaying {...npHarbour} {...h} />);
    const card = screen.getByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Now playing');
    expect(card).toHaveTextContent('Sea shanties for rowing');
    expect(card).toHaveTextContent('YouTube · requested by Ada · harbour');
    const bar = within(card).getByRole('progressbar', { name: 'Playback position' });
    expect(bar).toHaveAttribute('aria-valuenow', '37');
    expect(bar).toHaveAttribute('aria-valuemax', '214');
    expect(bar).toHaveAttribute('aria-valuetext', '0:37 of 3:34');
    await userEvent.click(within(card).getByRole('button', { name: 'Pause' }));
    await userEvent.click(within(card).getByRole('button', { name: 'Skip' }));
    await userEvent.click(within(card).getByRole('button', { name: 'Stop' }));
    expect(h.onToggle).toHaveBeenCalledTimes(1);
    expect(h.onSkip).toHaveBeenCalledTimes(1);
    expect(h.onStop).toHaveBeenCalledTimes(1);
    expect(card).toHaveTextContent('Volume 60%');
    await expectNoAxeViolations(container);
  });

  it('paused: the toggle is named Play and the state says Paused', () => {
    render(<NowPlaying {...npHarbour} playing={false} />);
    expect(screen.getByRole('button', { name: 'Play' })).toBeInTheDocument();
    expect(screen.getByRole('group')).toHaveTextContent('Paused');
  });

  it('the queue toggle says how many are up next and opens the queue list (current track marked)', async () => {
    render(<NowPlaying {...npHarbour} />);
    const toggle = screen.getByRole('button', { name: 'Up next · 1' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('list', { name: 'Queue' })).toBeNull();
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const list = screen.getByRole('list', { name: 'Queue' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveAttribute('aria-current', 'true');
    expect(rows[1]).toHaveTextContent('Fjord ambience, 4 hours');
    expect(rows[1]).toHaveTextContent('Björn');
    expect(rows[1]).toHaveTextContent('4:12');
    await userEvent.click(toggle);
    expect(screen.queryByRole('list', { name: 'Queue' })).toBeNull();
  });

  it('a controlled queue reports the toggle and follows its prop', async () => {
    const onQueueToggle = vi.fn();
    render(<NowPlaying {...npHarbour} queueOpen={false} onQueueToggle={onQueueToggle} />);
    await userEvent.click(screen.getByRole('button', { name: 'Up next · 1' }));
    expect(onQueueToggle).toHaveBeenCalledWith(true);
    expect(screen.queryByRole('list', { name: 'Queue' })).toBeNull();
  });

  it('without control the controls are aria-disabled, say why, and a press shows the toast instead of acting', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const h = handlers();
    render(<NowPlaying {...npHarbour} {...h} canControl={false} />);
    const pause = screen.getByRole('button', { name: 'Pause' });
    expect(pause).toHaveAttribute('aria-disabled', 'true');
    expect(pause).toHaveAccessibleDescription('Join harbour to control playback');
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    await user.click(pause);
    await user.click(screen.getByRole('button', { name: 'Stop' }));
    expect(h.onToggle).not.toHaveBeenCalled();
    expect(h.onStop).not.toHaveBeenCalled();
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('Join harbour to control playback');
    act(() => { vi.advanceTimersByTime(2500); });
    expect(status).toHaveTextContent('');
  });

  it('idle: the card says nothing plays with the idle sentence; compact and stage draw nothing', () => {
    const idle = { ...npHarbour, track: undefined, playing: false, positionS: 0, queue: [], meta: 'Glenn is idle in harbour. /play starts the next one.' };
    render(<NowPlaying {...idle} />);
    const card = screen.getByRole('group');
    expect(card).toHaveTextContent('Stopped');
    expect(card).toHaveTextContent('Nothing playing');
    expect(screen.getByRole('button', { name: 'Play' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuetext', '0:00 of 0:00');
    const compact = render(<NowPlaying {...idle} variant="compact" />);
    expect(compact.container).toBeEmptyDOMElement();
    const stage = render(<NowPlaying {...idle} variant="stage" />);
    expect(stage.container).toBeEmptyDOMElement();
  });

  it('compact: the bot, the title over "state · pos / dur", play/pause and skip, no stop', () => {
    render(<NowPlaying {...npHarbour} variant="compact" />);
    const row = screen.getByRole('group', { name: 'Music player' });
    expect(within(row).getByRole('img', { name: 'Glenn' })).toBeInTheDocument();
    expect(row).toHaveTextContent('Now playing · 0:37 / 3:34');
    expect(within(row).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual(['Pause', 'Skip']);
    expect(within(row).getByRole('progressbar')).toHaveAttribute('aria-valuenow', '37');
  });

  it('stage: the bot, title and meta, times, play/pause, skip, stop and "n up next"', async () => {
    const { container } = render(<NowPlaying {...npHarbour} variant="stage" />);
    const bar = screen.getByRole('group', { name: 'Music player' });
    expect(bar).toHaveTextContent('YouTube · requested by Ada · harbour');
    expect(bar).toHaveTextContent('0:37 / 3:34');
    expect(bar).toHaveTextContent('1 up next');
    expect(within(bar).getAllByRole('button').map(b => b.getAttribute('aria-label'))).toEqual(['Pause', 'Skip', 'Stop']);
    await expectNoAxeViolations(container);
  });
});

describe('NowPlaying toasts (fix round 2)', () => {
  it('several players share one live region, and the last one leaving takes it away', async () => {
    const first = render(<NowPlaying {...npHarbour} variant="compact" canControl={false} />);
    const second = render(<NowPlaying {...npHarbour} variant="stage" canControl={false} />);
    expect(screen.getAllByRole('status')).toHaveLength(1);
    await userEvent.click(within(second.container).getByRole('button', { name: 'Skip' }));
    expect(screen.getByRole('status')).toHaveTextContent('Join harbour to control playback');
    first.unmount();
    expect(screen.getAllByRole('status')).toHaveLength(1);
    second.unmount();
    expect(screen.queryByRole('status')).toBeNull();
  });
});
