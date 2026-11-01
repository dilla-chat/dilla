import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { AttachmentCard, type AttachmentCardProps } from './AttachmentCard.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';
import meta from './AttachmentCard.stories.tsx';

const PLAYER_LABELS = { play: 'play {name}', loading: 'loading', error: 'This browser cannot play this file.', save: 'save' };
const player = (state: 'idle' | 'loading' | 'ready' | 'error', src: string | null = null): NonNullable<AttachmentCardProps['player']> =>
  ({ state, src, labels: PLAYER_LABELS, onPlay: () => {}, onError: () => {} });

afterEach(() => { delete document.documentElement.dataset.theme; });

const BASE: AttachmentCardProps = {
  kind: 'image', name: 'drawn.png', size: '48 KB', thumbUrl: 'blob:http://127.0.0.1/thumb', w: 640, h: 480,
  openLabel: 'open drawn.png', onOpen: () => {}, saveLabel: 'save drawn.png', onSave: () => {}, state: 'idle', tabbable: true,
};

describe('AttachmentCard', () => {
  it('an image card is one button named open, holding the thumbnail with the name as alt text', async () => {
    const onOpen = vi.fn();
    const { container } = render(<AttachmentCard {...BASE} onOpen={onOpen} />);
    const card = container.firstElementChild!;
    expect(card).toHaveClass('d-attachment-card');
    expect(card).toHaveAttribute('data-kind', 'image');
    expect(card).toHaveAttribute('data-state', 'idle');
    const open = screen.getByRole('button', { name: 'open drawn.png' });
    expect(screen.getAllByRole('button')).toEqual([open]);
    const img = screen.getByRole('img', { name: 'drawn.png' });
    expect(img).toHaveAttribute('src', 'blob:http://127.0.0.1/thumb');
    expect(img).toHaveAttribute('data-thumb', 'ready');
    expect(open.style.getPropertyValue('--d-card-ratio')).toBe('640 / 480');
    expect(open.style.getPropertyValue('--d-card-w')).toBe('17.5rem');
    await userEvent.click(open);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('draws a neutral box until the thumbnail is there, and sizes a small image by its own size', () => {
    const { container } = render(<AttachmentCard {...BASE} thumbUrl={null} w={100} h={50} />);
    expect(screen.queryByRole('img')).toBeNull();
    expect(container.querySelector('.d-attachment-card__box')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: 'open drawn.png' }).style.getPropertyValue('--d-card-w')).toBe('6.25rem');
  });

  it('a file card shows its name and size and is one save button', async () => {
    const onSave = vi.fn();
    render(<AttachmentCard {...BASE} kind="file" name="route.gpx" size="12.4 KB" thumbUrl={null} w={null} h={null}
      openLabel="open route.gpx" onOpen={undefined} saveLabel="save route.gpx" onSave={onSave} />);
    expect(screen.getByText('route.gpx')).toBeInTheDocument();
    // The prototype's meta line (§6.8): the size and the extension.
    expect(screen.getByText('12.4 KB · GPX')).toBeInTheDocument();
    const save = screen.getByRole('button', { name: 'save route.gpx' });
    expect(save.querySelector('svg')).toHaveAttribute('data-icon', 'download');
    expect(screen.getAllByRole('button')).toEqual([save]);
    await userEvent.click(save);
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it('a too-large card has no button and says why', () => {
    render(<AttachmentCard {...BASE} state="too-large" stateText="too large to open in a browser" thumbUrl={null} />);
    expect(screen.queryByRole('button')).toBeNull();
    expect(screen.getByText('too large to open in a browser')).toHaveClass('d-attachment-card__state');
  });

  it('a loading card is busy; a failed card shows its state; tabbable decides the tab stop', () => {
    const { rerender } = render(<AttachmentCard {...BASE} state="loading" stateText="opening…" />);
    expect(screen.getByRole('button', { name: 'open drawn.png' })).toHaveAttribute('aria-busy', 'true');
    rerender(<AttachmentCard {...BASE} state="failed" stateText="could not open (E_BLOB_OPEN)" tabbable={false} />);
    expect(screen.getByText('could not open (E_BLOB_OPEN)')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'open drawn.png' })).not.toHaveAttribute('aria-busy');
    expect(screen.getByRole('button', { name: 'open drawn.png' }).tabIndex).toBe(-1);
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><AttachmentCard {...BASE} />
      <AttachmentCard {...BASE} kind="file" name="route.gpx" size="12.4 KB" thumbUrl={null} openLabel="open route.gpx" saveLabel="save route.gpx" /></div>);
    await expectNoAxeViolations(container);
  });

  it('renders no save button without onSave (SLICE-UX-14)', () => {
    render(<AttachmentCard kind="file" name="notes.txt" size="2 KB" thumbUrl={null} w={null} h={null} openLabel="open notes.txt" saveLabel="save notes.txt" state="idle" tabbable />);
    expect(screen.queryByRole('button', { name: 'save notes.txt' })).toBeNull();
  });

  it('AttachmentCard renders a player for audio and video kinds', () => {
    const { container, rerender } = render(<AttachmentCard {...BASE} kind="audio" name="voice.ogg" size="210 KB" thumbUrl={null} w={null} h={null}
      openLabel="open voice.ogg" saveLabel="save voice.ogg" player={player('idle')} />);
    expect(screen.getAllByRole('button', { name: 'play voice.ogg' })).toHaveLength(1);
    expect(container.querySelector('audio')).toBeNull();
    expect(container.querySelector('.d-media-player')).not.toBeNull();
    expect(screen.getByText('voice.ogg')).toBeInTheDocument();
    // I2: the media card's save shows media.save and is named for the file, in every player state.
    expect(screen.getByRole('button', { name: 'save voice.ogg' })).toHaveTextContent(/^save$/);

    rerender(<AttachmentCard {...BASE} kind="video" name="clip.webm" size="2.1 MB" thumbUrl={null} w={null} h={null}
      openLabel="open clip.webm" saveLabel="save clip.webm" player={player('ready', 'blob:http://127.0.0.1/clip')} />);
    const videos = container.querySelectorAll('video');
    expect(videos).toHaveLength(1);
    expect(videos[0]).toHaveAttribute('controls');
    expect(videos[0]).toHaveAttribute('playsinline');
    expect(videos[0].autoplay).toBe(false);
    expect(videos[0].hasAttribute('autoplay')).toBe(false);
    expect(container.firstElementChild).toHaveAttribute('data-kind', 'video');

    rerender(<AttachmentCard {...BASE} />);
    expect(container.querySelector('.d-media-player')).toBeNull();
    expect(screen.getByRole('button', { name: 'open drawn.png' })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: 'drawn.png' })).toBeInTheDocument();

    rerender(<AttachmentCard {...BASE} kind="file" name="route.gpx" size="12.4 KB" thumbUrl={null} w={null} h={null}
      openLabel="open route.gpx" onOpen={undefined} saveLabel="save route.gpx" />);
    expect(container.querySelector('.d-media-player')).toBeNull();
    expect(container.querySelector('.d-attachment-card__file')).not.toBeNull();
    expect(screen.getAllByRole('button')).toEqual([screen.getByRole('button', { name: 'save route.gpx' })]);
  });

  it('a player in its error state offers its own save, and the card does not repeat it', () => {
    render(<AttachmentCard {...BASE} kind="video" name="clip.webm" size="2.1 MB" thumbUrl={null} w={null} h={null}
      openLabel="open clip.webm" saveLabel="save clip.webm" player={player('error')} />);
    expect(screen.getByText('This browser cannot play this file.')).toBeInTheDocument();
    const save = screen.getByRole('button', { name: 'save clip.webm' });
    expect(save).toHaveTextContent(/^save$/);
    expect(screen.getAllByRole('button')).toEqual([save]);
  });

  it('an audio or video card too large for a browser keeps the file body, with no player', () => {
    const { container } = render(<AttachmentCard {...BASE} kind="video" name="harbour.mov" size="310 MB" thumbUrl={null} w={null} h={null}
      openLabel="open harbour.mov" saveLabel="save harbour.mov" player={player('idle')} state="too-large" stateText="too large to open in a browser" />);
    expect(container.querySelector('.d-media-player')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><AttachmentCard {...(meta.args as AttachmentCardProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
  it('a badge is visual only: it is inside the open button and leaves its name alone', () => {
    render(<AttachmentCard {...BASE} badge="GIF" />);
    const badge = screen.getByText('GIF');
    expect(badge).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByRole('button', { name: 'open drawn.png' })).toContainElement(badge);
  });
  it('shows no badge unless one is given', () => {
    render(<AttachmentCard {...BASE} />);
    expect(screen.queryByText('GIF')).toBeNull();
  });
});
