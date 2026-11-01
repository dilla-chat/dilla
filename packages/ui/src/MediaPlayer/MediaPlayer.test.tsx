import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { MediaPlayer, type MediaPlayerProps } from './MediaPlayer.tsx';
import meta from './MediaPlayer.stories.tsx';

const mediaLabels: MediaPlayerProps['labels'] = {
  play: 'play {name}', loading: 'loading', error: 'This browser cannot play this file.', save: 'save',
};

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('MediaPlayer', () => {
  it('MediaPlayer never autoplays and offers save on error', () => {
    const { container, rerender } = render(<MediaPlayer kind="video" src="blob:x" name="clip.webm" state="ready" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={() => {}} />);
    const v = container.querySelector('video')!;
    expect(v.autoplay).toBe(false); expect(v.hasAttribute('controls')).toBe(true); expect(v).toHaveAccessibleName('clip.webm');
    rerender(<MediaPlayer kind="video" src="blob:x" name="clip.webm" state="error" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={() => {}} />);
    expect(screen.getByText('This browser cannot play this file.')).toBeInTheDocument();
  });

  it('a ready player is the native element with its source, inline, without autoplay or loop', () => {
    const { container, rerender } = render(<MediaPlayer kind="video" src="blob:x" name="clip.webm" state="ready" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={() => {}} />);
    const v = container.querySelector('video')!;
    expect(v).toHaveAttribute('src', 'blob:x');
    expect(v).toHaveAttribute('playsinline');
    expect(v).toHaveAttribute('preload', 'metadata');
    expect(v.hasAttribute('autoplay')).toBe(false);
    expect(v.hasAttribute('loop')).toBe(false);
    rerender(<MediaPlayer kind="audio" src="blob:y" name="voice.ogg" state="ready" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={() => {}} />);
    const a = container.querySelector('audio')!;
    expect(a).toHaveAttribute('controls');
    expect(a).toHaveAttribute('preload', 'metadata');
    expect(a.autoplay).toBe(false);
    expect(a.hasAttribute('loop')).toBe(false);
    expect(a).toHaveAccessibleName('voice.ogg');
    expect(container.querySelector('video')).toBeNull();
  });

  it('idle is one play button named for the file; loading is state text; neither has a media element', async () => {
    const onPlay = vi.fn();
    const { container, rerender } = render(<MediaPlayer kind="audio" src={null} name="voice.ogg" state="idle" labels={mediaLabels} onPlay={onPlay} onSave={() => {}} onError={() => {}} />);
    expect(screen.getAllByRole('button')).toHaveLength(1);
    await userEvent.click(screen.getByRole('button', { name: 'play voice.ogg' }));
    expect(onPlay).toHaveBeenCalledTimes(1);
    expect(container.querySelector('audio, video')).toBeNull();
    rerender(<MediaPlayer kind="audio" src={null} name="voice.ogg" state="loading" labels={mediaLabels} onPlay={onPlay} onSave={() => {}} onError={() => {}} />);
    expect(screen.getByText('loading')).toBeInTheDocument();
    expect(container.querySelector('audio, video')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('the error state saves the file and plays nothing', async () => {
    const onSave = vi.fn();
    const { container } = render(<MediaPlayer kind="video" src="blob:x" name="clip.webm" state="error" labels={mediaLabels} onPlay={() => {}} onSave={onSave} onError={() => {}} />);
    expect(container.querySelector('audio, video')).toBeNull();
    expect(screen.getByRole('status')).toHaveTextContent('This browser cannot play this file.');
    await userEvent.click(screen.getByRole('button', { name: 'save' }));
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it('MediaPlayer reports a media element error through its error state', () => {
    const onError = vi.fn();
    const { container } = render(<MediaPlayer kind="audio" src="blob:y" name="voice.ogg" state="ready" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={onError} />);
    const audio = container.querySelector('audio')!;
    expect(() => fireEvent.error(audio)).not.toThrow();
    expect(onError).toHaveBeenCalledTimes(1);
  });

  it('keeps focus in the player after the explicit play: the loading text, then the native player (I1)', async () => {
    const p = { kind: 'video' as const, labels: mediaLabels, onPlay: () => {}, onSave: () => {}, onError: () => {} };
    const { rerender } = render(<><MediaPlayer {...p} src={null} name="clip.webm" state="idle" /><MediaPlayer {...p} src={null} name="other.webm" state="idle" /></>);
    await userEvent.keyboard('{Tab}{Enter}');
    rerender(<><MediaPlayer {...p} src={null} name="clip.webm" state="loading" /><MediaPlayer {...p} src="blob:o" name="other.webm" state="ready" /></>);
    expect(screen.getByText('loading')).toHaveFocus();
    // jsdom treats no media element as focusable (a browser does, through its controls), so the call is what is checked.
    const focus = vi.spyOn(HTMLMediaElement.prototype, 'focus');
    rerender(<><MediaPlayer {...p} src="blob:x" name="clip.webm" state="ready" /><MediaPlayer {...p} src="blob:o" name="other.webm" state="ready" /></>);
    expect(focus.mock.contexts).toEqual([screen.getByLabelText('clip.webm')]);
    focus.mockRestore();
  });

  it('an error after play focuses save; a player whose play was not pressed never takes focus (I1)', async () => {
    const p = { kind: 'audio' as const, name: 'voice.ogg', labels: mediaLabels, onPlay: () => {}, onSave: () => {}, onError: () => {} };
    const { rerender } = render(<MediaPlayer {...p} src={null} state="idle" />);
    await userEvent.keyboard('{Tab}{Enter}');
    rerender(<MediaPlayer {...p} src={null} state="error" saveName="save voice.ogg" />);
    expect(screen.getByRole('button', { name: 'save voice.ogg' })).toHaveFocus();

    const other = render(<><input aria-label="message" /><MediaPlayer {...p} name="other.ogg" src={null} state="idle" /></>);
    const field = other.getByRole('textbox', { name: 'message' });
    field.focus();
    other.rerender(<><input aria-label="message" /><MediaPlayer {...p} name="other.ogg" src={null} state="loading" /></>);
    other.rerender(<><input aria-label="message" /><MediaPlayer {...p} name="other.ogg" src="blob:y" state="ready" /></>);
    expect(field).toHaveFocus();
  });

  it('the error save shows media.save and is named for the file (I2)', () => {
    render(<MediaPlayer kind="video" src={null} name="clip.webm" state="error" saveName="save clip.webm" labels={mediaLabels} onPlay={() => {}} onSave={() => {}} onError={() => {}} />);
    const save = screen.getByRole('button', { name: 'save clip.webm' });
    expect(save).toHaveTextContent(/^save$/);
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><MediaPlayer {...(meta.args as MediaPlayerProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });
});
