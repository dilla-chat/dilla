import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { CoreClient, MemberSummary, VoiceMusicState } from '@dilla/client-core';
import { t } from '../../strings/index.ts';

const adapter = vi.hoisted(() => {
  let state: VoiceMusicState = { status: 'unavailable', reason: 'bot-unavailable' };
  const listeners = new Set<() => void>();
  return {
    set(next: VoiceMusicState) { state = next; for (const l of listeners) l(); },
    state: vi.fn(() => state),
    subscribe: vi.fn((_: string, on: () => void) => { listeners.add(on); return () => { listeners.delete(on); }; }),
    refresh: vi.fn(() => Promise.resolve()),
    pause: vi.fn(() => Promise.resolve()),
    skip: vi.fn(() => Promise.resolve()),
    stop: vi.fn(() => Promise.resolve()),
    reset() { state = { status: 'unavailable', reason: 'bot-unavailable' }; listeners.clear(); },
  };
});
vi.mock('@dilla/client-core', async importOriginal => ({ ...await importOriginal<object>(), createVoiceMusicAdapter: () => adapter }));

const { heldMusicRefreshers, isCommunityBot, MusicPlayer, musicMarker, useBotRoomLine, useMusicCard, useMusicPlayer } = await import('./music.tsx');

const COMMUNITY = 'ab'.repeat(16);
const CLIENT = {} as CoreClient;
const CHANNEL = 'cd'.repeat(16);
const ADA = '0a'.repeat(16);
const BJORN = '0b'.repeat(16);
const members: MemberSummary[] = [
  { userId: ADA, username: 'ada', display: 'Ada', kind: 0, roleIds: [] },
  { userId: BJORN, username: 'bjorn', display: 'Björn', kind: 0, roleIds: [] },
];
const track = (title: string, requesterId: string | undefined, durationMs: number) =>
  ({ id: 'ef'.repeat(16), title, artist: 'Artist', durationMs, provider: 'youtube' as const, ...(requesterId === undefined ? {} : { requesterId }) });
const playing = (control = true): VoiceMusicState => ({
  status: 'available', track: track('Sea shanties for rowing', ADA, 214_000),
  queue: [{ id: '12'.repeat(16), track: track('Fjord ambience, 4 hours', BJORN, 252_000) }],
  positionMs: 37_000, volume: 60, paused: false, shuffle: false, repeat: 'off', permissions: { control, manageQueue: control },
});

function Harness({ bot = 'Glenn', variant = 'card' as const }: { bot?: string | null; variant?: 'card' | 'compact' | 'stage' }) {
  const view = useMusicPlayer({ client: CLIENT, communityId: COMMUNITY, channelId: CHANNEL, room: 'harbour', bot, members, trust: undefined });
  return view === null ? <p>no player</p> : <MusicPlayer view={view} variant={variant} />;
}

afterEach(() => { adapter.reset(); vi.clearAllMocks(); });

describe('useMusicPlayer', () => {
  it('reads nothing while the Bot is not in the call', () => {
    render(<Harness bot={null} />);
    expect(adapter.subscribe).not.toHaveBeenCalled();
    expect(screen.getByText('no player')).toBeInTheDocument();
  });

  it('maps the Bot snapshot: title, requester, provider, room, position, volume and the queue', async () => {
    render(<Harness />);
    expect(adapter.subscribe).toHaveBeenCalledWith(CHANNEL, expect.any(Function));
    act(() => { adapter.set(playing()); });
    const card = await screen.findByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Now playing');
    expect(card).toHaveTextContent('Sea shanties for rowing');
    expect(card).toHaveTextContent('YouTube · requested by Ada · harbour');
    expect(within(card).getByRole('progressbar')).toHaveAttribute('aria-valuetext', '0:37 of 3:34');
    expect(card).toHaveTextContent('60%');
    await userEvent.click(within(card).getByRole('button', { name: 'Up next · 1' }));
    const rows = within(within(card).getByRole('list', { name: 'Queue' })).getAllByRole('listitem');
    expect(rows[0]).toHaveAttribute('aria-current', 'true');
    expect(rows[1]).toHaveTextContent('Fjord ambience, 4 hours');
    expect(rows[1]).toHaveTextContent('Björn');
    expect(rows[1]).toHaveTextContent('4:12');
  });

  it('controls act on the call channel through the adapter', async () => {
    render(<Harness variant="stage" />);
    act(() => { adapter.set(playing()); });
    const bar = await screen.findByRole('group', { name: 'Music player' });
    await userEvent.click(within(bar).getByRole('button', { name: 'Pause' }));
    await userEvent.click(within(bar).getByRole('button', { name: 'Skip' }));
    await userEvent.click(within(bar).getByRole('button', { name: 'Stop' }));
    await waitFor(() => expect(adapter.pause).toHaveBeenCalledWith(CHANNEL, true));
    expect(adapter.skip).toHaveBeenCalledWith(CHANNEL);
    expect(adapter.stop).toHaveBeenCalledWith(CHANNEL);
  });

  it('without the Bot\'s control grant the controls are inert and say the DJ rule', async () => {
    render(<Harness variant="compact" />);
    act(() => { adapter.set(playing(false)); });
    const row = await screen.findByRole('group', { name: 'Music player' });
    const pause = within(row).getByRole('button', { name: 'Pause' });
    expect(pause).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(pause);
    expect(adapter.pause).not.toHaveBeenCalled();
    expect(screen.getByRole('status')).toHaveTextContent('A DJ, owner, or administrator must control music in this voice channel.');
  });

  it('a refused control says why in the toast', async () => {
    adapter.skip.mockImplementationOnce(() => Promise.reject(new Error('You do not have permission to control this Bot.')));
    render(<Harness variant="stage" />);
    act(() => { adapter.set(playing()); });
    await userEvent.click(await screen.findByRole('button', { name: 'Skip' }));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('You do not have permission to control this Bot.'));
  });

  it('idle: the card says the Bot is idle in the room; the compact row draws nothing', async () => {
    const idle: VoiceMusicState = { ...playing(), track: null, queue: [], positionMs: 0, paused: true } as VoiceMusicState;
    const { unmount } = render(<Harness />);
    act(() => { adapter.set(idle); });
    const card = await screen.findByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Nothing playing');
    expect(card).toHaveTextContent('Glenn is idle in harbour. /play starts the next one.');
    unmount();
    render(<Harness variant="compact" />);
    act(() => { adapter.set(idle); });
    expect(screen.queryByRole('group', { name: 'Music player' })).toBeNull();
  });

  it('re-reads the snapshot every 25 s while open, not while the page is hidden, and once when it shows again', () => {
    vi.useFakeTimers();
    const hiddenProp = Object.getOwnPropertyDescriptor(Document.prototype, 'hidden');
    let pageHidden = false;
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => pageHidden });
    try {
      const { unmount } = render(<Harness />);
      act(() => { vi.advanceTimersByTime(25_000); });
      expect(adapter.refresh).toHaveBeenCalledWith(CHANNEL);
      adapter.refresh.mockClear();
      pageHidden = true;
      act(() => { vi.advanceTimersByTime(50_000); });
      expect(adapter.refresh).not.toHaveBeenCalled();
      pageHidden = false;
      act(() => { document.dispatchEvent(new Event('visibilitychange')); });
      expect(adapter.refresh).toHaveBeenCalledTimes(1);
      unmount();
      adapter.refresh.mockClear();
      act(() => { vi.advanceTimersByTime(60_000); });
      expect(adapter.refresh).not.toHaveBeenCalled();
    } finally {
      Reflect.deleteProperty(document, 'hidden');
      if (hiddenProp !== undefined) Object.defineProperty(Document.prototype, 'hidden', hiddenProp);
      vi.useRealTimers();
    }
  });

  // Fix round 2: the Bot's member row ("playing in {room}") reads the cached state and holds no refresher of its own.
  it('the Bot member line reads the cached state, follows it, and adds no polling', () => {
    function Line() { return <p>{useBotRoomLine(CLIENT, COMMUNITY, CHANNEL, 'harbour') ?? 'none'}</p>; }
    adapter.set(playing());
    render(<Line />);
    expect(screen.getByText(t('members.playingIn', { room: 'harbour' }))).toBeInTheDocument();
    expect(heldMusicRefreshers()).toBe(0);
    expect(adapter.refresh).not.toHaveBeenCalled();
    act(() => { adapter.set({ ...(playing() as Extract<VoiceMusicState, { status: 'available' }>), track: null }); });
    expect(screen.getByText(t('members.idleIn', { room: 'harbour' }))).toBeInTheDocument();
  });

  it('two views of one channel share one schedule, cleared when both have gone', () => {
    const one = render(<Harness variant="stage" />);
    const two = render(<Harness variant="compact" />);
    expect(heldMusicRefreshers()).toBe(1);
    one.unmount();
    expect(heldMusicRefreshers()).toBe(1);
    two.unmount();
    expect(heldMusicRefreshers()).toBe(0);
  });

  it('re-reads a second after the playing track should end', () => {
    vi.useFakeTimers();
    try {
      render(<Harness variant="stage" />);
      act(() => { adapter.set({ ...playing(), clock: { elapsedMs: 0, receivedAtMs: Date.now() } } as VoiceMusicState); });
      adapter.refresh.mockClear();
      // 3:34 track at 0:37: 177 s left, then a second.
      act(() => { vi.advanceTimersByTime(177_000 + 999); });
      const before = adapter.refresh.mock.calls.length;
      act(() => { vi.advanceTimersByTime(2); });
      expect(adapter.refresh.mock.calls.length).toBe(before + 1);
    } finally { vi.useRealTimers(); }
  });
});

const ITEM = 'ef'.repeat(16);
function CardHarness({ botInRoom, inCall, item = ITEM, visible = true }: { botInRoom: boolean; inCall: boolean; item?: string; visible?: boolean }) {
  const view = useMusicCard({ client: CLIENT, communityId: COMMUNITY, channelId: CHANNEL, room: 'harbour', bot: 'Glenn', item, botInRoom, visible, inCall, members, trust: undefined });
  return <MusicPlayer view={view} variant="card" />;
}

describe('the "started music" card', () => {
  it('parses the player marker only at the end of the body', () => {
    expect(musicMarker(`Music started.\n[dilla-music:v1:${CHANNEL}:${ADA}:${ITEM}]`)).toEqual({ voiceChannelId: CHANNEL, requesterId: ADA, item: ITEM });
    expect(musicMarker(`Music started.\n[dilla-music:v1:${CHANNEL}:${ADA}]`)).toBeNull();
    expect(musicMarker(`[dilla-music:v1:${CHANNEL}:${ADA}:${ITEM}] and more`)).toBeNull();
    expect(musicMarker('[dilla-music:v1:xyz:abc]')).toBeNull();
  });

  it('reads the room\'s player while the Bot is there; out of the call a press says to join the room', async () => {
    render(<CardHarness botInRoom inCall={false} />);
    act(() => { adapter.set(playing(false)); });
    const card = await screen.findByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Sea shanties for rowing');
    await userEvent.click(within(card).getByRole('button', { name: 'Pause' }));
    expect(adapter.pause).not.toHaveBeenCalled();
    expect(screen.getByRole('status')).toHaveTextContent('Join harbour to control playback');
  });

  it('once the Bot left the room the card says so and reads nothing', () => {
    render(<CardHarness botInRoom={false} inCall={false} />);
    expect(adapter.subscribe).not.toHaveBeenCalled();
    const card = screen.getByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Nothing playing');
    expect(card).toHaveTextContent('Glenn left voice');
  });

  it('Ruling AI: the playhead runs on from the Bot stamp while playing', () => {
    vi.useFakeTimers({ shouldAdvanceTime: false });
    try {
      render(<Harness variant="stage" />);
      act(() => { adapter.set({ ...playing(), clock: { elapsedMs: 3_000, receivedAtMs: Date.now() } } as VoiceMusicState); });
      expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '40');
      act(() => { vi.advanceTimersByTime(5_000); });
      expect(screen.getByRole('progressbar')).toHaveAttribute('aria-valuenow', '45');
    } finally { vi.useRealTimers(); }
  });
});

describe('the card after its track (fix round 2)', () => {
  it('shows the finished state once another track plays, and reads nothing off screen', async () => {
    render(<CardHarness botInRoom inCall item={'77'.repeat(16)} />);
    act(() => { adapter.set(playing()); });
    const card = await screen.findByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Finished');
    expect(card).toHaveTextContent('Glenn played this in harbour');
    expect(card).not.toHaveTextContent('Sea shanties for rowing');
  });
  it('Task S1: shows the finished state once the queue ran out after the announced track', async () => {
    render(<CardHarness botInRoom inCall />);
    act(() => { adapter.set({ ...playing(), track: null, queue: [] } as VoiceMusicState); });
    const card = await screen.findByRole('group', { name: 'Music player' });
    expect(card).toHaveTextContent('Finished');
    expect(card).toHaveTextContent('Glenn played this in harbour');
    expect(card).not.toHaveTextContent('is idle in');
  });
  it('Task S1: before the card is on screen (nothing read yet) it is neutral, never "left voice"', () => {
    render(<CardHarness botInRoom inCall={false} visible={false} />);
    const card = screen.getByRole('group', { name: 'Music player' });
    expect(card).not.toHaveTextContent('left voice');
    expect(card).not.toHaveTextContent('Nothing playing');
    expect(card).toHaveTextContent('harbour');
  });
  it('an off-screen card does not subscribe', () => {
    render(<CardHarness botInRoom inCall={false} visible={false} />);
    expect(adapter.subscribe).not.toHaveBeenCalled();
  });
  it('the Bot account is the community member of kind 1', () => {
    expect(isCommunityBot([{ userId: ADA, username: 'glenn', display: 'Glenn', kind: 1, roleIds: [] }], ADA)).toBe(true);
    expect(isCommunityBot(members, ADA)).toBe(false);
    expect(isCommunityBot(members, null)).toBe(false);
  });
});
