import { readFileSync } from 'node:fs';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { CallTile, type CallTileProps, type VoiceParticipantView } from './CallTile.tsx';
import meta from './CallTile.stories.tsx';

// jsdom has no MediaStream: a bare stand-in is enough, the tile only hands it to a <video>'s srcObject.
if (typeof globalThis.MediaStream === 'undefined') {
  (globalThis as { MediaStream?: unknown }).MediaStream = class MediaStream {};
}

const view: VoiceParticipantView = {
  deviceId: 'd1', userId: 'u1', name: 'ada', state: 'ok', speaking: false, muted: false, deafened: false,
  video: false, screen: false, web: true, bot: false,
};
const labels: CallTileProps['labels'] = {
  state: {
    speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
    connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
  },
  volume: 'volume for ada',
  volumeValue: '{n} %',
};
const css = readFileSync(`${process.cwd()}/src/CallTile/CallTile.css`, 'utf8');
const rest = { own: false, volume: 100, media: null, labels };

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('CallTile', () => {
  it('task 8c: a stage tile shows its stat; a screen only while focused; a compact tile and an empty stat show none', () => {
    const { container, rerender } = render(<CallTile view={view} {...rest} onFocusTile={() => {}} stat="opus 48k" />);
    expect(container.querySelector('.d-call-tile__stat')).toHaveTextContent('opus 48k');
    rerender(<CallTile view={view} {...rest} source="screen" onFocusTile={() => {}} stat="1440p · 30 fps" />);
    expect(container.querySelector('.d-call-tile__stat')).toBeNull();
    rerender(<CallTile view={view} {...rest} source="screen" focused onFocusTile={() => {}} stat="1440p · 30 fps" />);
    expect(container.querySelector('.d-call-tile__stat')).toHaveTextContent('1440p · 30 fps');
    rerender(<CallTile view={view} {...rest} stat="opus 48k" />);
    expect(container.querySelector('.d-call-tile__stat')).toBeNull();
    rerender(<CallTile view={view} {...rest} onFocusTile={() => {}} />);
    expect(container.querySelector('.d-call-tile__stat')).toBeNull();
  });

  it('CallTile never attaches media for an unverified stream', () => {
    const { container } = render(<CallTile view={{ ...view, state: 'unverified' }} own={false} volume={100} media={new MediaStream()} labels={labels} />);
    expect(container.querySelector('video, audio')).toBeNull();
    expect(screen.getByText('unverified stream')).toBeInTheDocument();
  });

  it('attaches a verified stream to a muted video that never plays sound', () => {
    const stream = new MediaStream();
    const { container } = render(<CallTile view={{ ...view, video: true }} {...rest} media={stream} />);
    const video = container.querySelector('video')!;
    expect(video).not.toBeNull();
    expect(video.srcObject).toBe(stream);
    expect(video.muted).toBe(true);
    expect(container.querySelector('audio')).toBeNull();
  });

  it('R25: speaking is a 2px accent ring on the tile edge and on the avatar, with no StateDot; the word stays for assistive tech', () => {
    const { container } = render(<CallTile view={{ ...view, speaking: true }} {...rest} onFocusTile={() => {}} />);
    expect(container.querySelector('.d-call-tile')).toHaveAttribute('data-speaking', 'true');
    expect(container.querySelector('.d-call-tile__avatar-ring')).toHaveAttribute('data-speaking', 'true');
    expect(container.querySelector('.d-state-dot')).toBeNull();
    expect(container.querySelector('[data-state="speaking"]')).toBeNull();
    expect(container.querySelector('.d-call-tile__more .d-sr-only')).toHaveTextContent('speaking');
    // Signal §5.6 (the prototype's tile): a 2px --ok edge, and the avatar's 5px --ok ring past a 3px tile-coloured gap with scale(1.06).
    expect(css).toMatch(/\.d-call-tile\[data-stage\]\[data-speaking="true"\] \{ box-shadow: inset 0 0 0 2px var\(--ok\); \}/);
    expect(css).toMatch(/\.d-call-tile__avatar-ring\[data-speaking="true"\] \{ box-shadow: 0 0 0 3px var\(--stage-tile\), 0 0 0 5px var\(--ok\); transform: scale\(1\.06\); \}/);
  });

  it('not speaking: no ring, no speaking word', () => {
    const { container } = render(<CallTile view={view} {...rest} onFocusTile={() => {}} />);
    expect(container.querySelector('.d-call-tile__avatar-ring')).toHaveAttribute('data-speaking', 'false');
    expect(container.querySelector('.d-call-tile')).toHaveAttribute('data-speaking', 'false');
    expect(container.querySelector('.d-call-tile__states .d-sr-only')).toBeNull();
  });

  it('fix round 1: a tile has no volume control of its own; a right-click, Shift+F10 or the ContextMenu key opens its person menu', async () => {
    const onSelect = vi.fn();
    const onChange = vi.fn();
    const menu = { person: { name: 'ada', where: 'in harbour' }, label: 'actions for ada', onSelect,
      volume: { label: 'User volume', value: 70, valueText: '70%', onChange },
      items: [{ id: 'mute', label: 'Mute for me', icon: 'volume-x' as const, check: false }, { id: 'copy', label: 'Copy username', icon: 'copy' as const }] };
    const { container } = render(<CallTile view={view} {...rest} onFocusTile={() => {}} menu={menu} />);
    expect(screen.queryByRole('slider')).toBeNull();
    expect(css).not.toMatch(/d-call-tile__volume|d-call-tile__stream/);
    fireEvent.contextMenu(container.querySelector('li')!, { clientX: 10, clientY: 10 });
    expect(screen.getByRole('menu', { name: 'actions for ada' })).toBeInTheDocument();
    expect(screen.getByRole('slider', { name: 'User volume' })).toHaveValue('70');
    fireEvent.change(screen.getByRole('slider', { name: 'User volume' }), { target: { value: '120' } });
    expect(onChange).toHaveBeenCalledWith(120);
    fireEvent.click(screen.getByRole('menuitemcheckbox', { name: 'Mute for me' }));
    expect(onSelect).toHaveBeenCalledWith('mute');
    expect(screen.getByRole('menu', { name: 'actions for ada' })).toBeInTheDocument(); // an on/off item keeps the menu open
    fireEvent.click(screen.getByRole('menuitem', { name: 'Copy username' }));
    expect(onSelect).toHaveBeenCalledWith('copy');
    expect(screen.queryByRole('menu')).toBeNull();
    fireEvent.keyDown(container.querySelector('li')!, { key: 'F10', shiftKey: true });
    expect(screen.getByRole('menu', { name: 'actions for ada' })).toBeInTheDocument();
  });

  it('a quiet chip shows the volume in the caption when it is not 100, and none at 100', () => {
    const { container, rerender } = render(<CallTile view={view} {...rest} volume={70} onFocusTile={() => {}} />);
    const chip = container.querySelector('.d-call-tile__caption .d-call-tile__chip')!;
    expect(chip).toHaveTextContent('70 %');
    expect(chip).toHaveAttribute('aria-hidden', 'true');
    rerender(<CallTile view={view} {...rest} volume={100} onFocusTile={() => {}} />);
    expect(container.querySelector('.d-call-tile__chip')).toBeNull();
    rerender(<CallTile view={view} {...rest} volume={150} />);
    expect(container.querySelector('.d-call-tile__chip')).toHaveTextContent('150 %');
    expect(css).toMatch(/\.d-call-tile__chip \{[^}]*background: var\(--surface-hi\)/);
  });

  it('#37: a screen tile marked silent shows the glyph-only no-sound state; a screen with sound, a camera or an avatar does not', () => {
    const { container, rerender } = render(<CallTile view={view} {...rest} source="screen" silent />);
    const mark = container.querySelector('[data-state="no-sound"]')!;
    expect(mark).toHaveAttribute('role', 'img');
    expect(mark).toHaveAttribute('aria-label', 'no sound');
    expect(mark).toHaveAttribute('title', 'no sound');
    expect(mark.textContent).toBe('');
    expect(mark.querySelector('[data-icon="volume-off"]')).not.toBeNull(); // Jonas: "no sound" is volume-off, "muted by you" is volume-x
    rerender(<CallTile view={view} {...rest} source="screen" />);
    expect(container.querySelector('[data-state="no-sound"]')).toBeNull();
    rerender(<CallTile view={view} {...rest} source="camera" silent />);
    expect(container.querySelector('[data-state="no-sound"]')).toBeNull();
    rerender(<CallTile view={view} {...rest} source="avatar" silent />);
    expect(container.querySelector('[data-state="no-sound"]')).toBeNull();
  });

  it('Signal §5.6: the caption is a 22px chip over the bottom left, 8px in, on the 0.8 scrim', () => {
    expect(css).toMatch(/\.d-call-tile\[data-stage\] \.d-call-tile__caption \{[^}]*position: absolute; left: 0\.5rem; bottom: 0\.5rem;[^}]*height: 1\.375rem;[^}]*background: var\(--scrim-caption\)/);
  });

  it('the own tile and a screen tile have no chip', () => {
    const { container, rerender } = render(<CallTile view={view} {...rest} own volume={70} />);
    expect(container.querySelector('.d-call-tile__chip')).toBeNull();
    rerender(<CallTile view={view} {...rest} source="screen" volume={70} />);
    expect(container.querySelector('.d-call-tile__chip')).toBeNull();
  });

  it('shows each glyph-only state as a named image, the stream states as glyph plus word, and the state hook', () => {
    const { container, rerender } = render(<CallTile view={{ ...view, muted: true, deafened: true, video: true, screen: true }} {...rest} />);
    for (const word of ['muted', 'deafened', 'camera on', 'sharing screen']) {
      const img = screen.getByRole('img', { name: word });
      expect(img).toHaveAttribute('title', word);
      expect(img).toHaveClass('d-call-tile__state');
      expect(img.textContent).toBe('');
    }
    expect([...container.querySelectorAll('svg')].map(i => i.getAttribute('data-icon'))).toEqual(['mic-off', 'headphone-off', 'video', 'monitor-up']);
    expect(screen.getByText('web')).toBeInTheDocument();
    rerender(<CallTile view={{ ...view, state: 'not-heard' }} {...rest} />);
    expect(screen.getByText('not heard yet')).toBeInTheDocument();
    expect(container.querySelector('.d-call-tile')).toHaveAttribute('data-voice-state', 'not-heard');
    rerender(<CallTile view={{ ...view, state: 'connecting' }} {...rest} />);
    expect(screen.getByText('connecting')).toBeInTheDocument();
  });

  it('web3.8 FX I2: a self-muted or deafened person who sends no camera or screen is shown muted, not "not heard yet"', () => {
    const { container, rerender } = render(<CallTile view={{ ...view, state: 'not-heard', muted: true }} {...rest} />);
    expect(screen.queryByText('not heard yet')).toBeNull();
    expect(screen.getByRole('img', { name: 'muted' })).toBeInTheDocument();
    // The stream state itself is kept as it is: nothing was verified.
    expect(container.querySelector('.d-call-tile')).toHaveAttribute('data-voice-state', 'not-heard');
    rerender(<CallTile view={{ ...view, state: 'not-heard', deafened: true }} {...rest} />);
    expect(screen.queryByText('not heard yet')).toBeNull();
    // Said unmuted (so expected to send), and nothing verifiable arrived: still "not heard yet".
    rerender(<CallTile view={{ ...view, state: 'not-heard' }} {...rest} />);
    expect(screen.getByText('not heard yet')).toBeInTheDocument();
    // Muted but sending a camera or a screen: those frames are expected, so the word stays until they verify.
    rerender(<CallTile view={{ ...view, state: 'not-heard', muted: true, video: true }} {...rest} />);
    expect(screen.getByText('not heard yet')).toBeInTheDocument();
    rerender(<CallTile view={{ ...view, state: 'not-heard', muted: true, screen: true }} {...rest} />);
    expect(screen.getByText('not heard yet')).toBeInTheDocument();
    // An unknown device stays unverified whatever it says about itself.
    rerender(<CallTile view={{ ...view, state: 'unverified', muted: true }} {...rest} />);
    expect(screen.getByText('unverified stream')).toBeInTheDocument();
  });

  it('the caption wraps the name and the states in one row: name, icons, then the stream-state words', () => {
    const { container } = render(<CallTile view={{ ...view, muted: true, video: true, state: 'unverified' }} {...rest} />);
    const caption = container.querySelector('.d-call-tile__caption')!;
    expect(caption.querySelector(':scope > .d-call-tile__name')).toHaveTextContent('ada');
    const states = [...caption.querySelectorAll<HTMLElement>('.d-call-tile__state')].map(s => s.dataset.state);
    expect(states).toEqual(['muted', 'video', 'unverified']);
    expect(caption.querySelectorAll('svg[data-icon]')).toHaveLength(2);
    expect(within(caption as HTMLElement).getByText('unverified stream')).toBeInTheDocument();
  });

  it('the old font glyphs are gone', () => {
    const { container } = render(<CallTile view={{ ...view, muted: true, deafened: true }} {...rest} />);
    expect(container.textContent).not.toMatch(/[⊘⊗]/);
  });

  it('writes the volume chip through the localised value label (M6)', () => {
    const { container } = render(<CallTile view={view} {...rest} volume={120} labels={{ ...labels, volumeValue: '{n} procent' }} />);
    expect(container.querySelector('.d-call-tile__chip')).toHaveTextContent('120 procent');
  });

  it('web3.8: a tile has no inset; a camera and a screen are two tiles (R27)', () => {
    const legacy = { inset: new MediaStream() } as Record<string, unknown>;
    const { container } = render(<CallTile view={{ ...view, video: true, screen: true }} {...rest} media={new MediaStream()} {...legacy} />);
    expect(container.querySelectorAll('video')).toHaveLength(1);
    expect(container.querySelector('.d-call-tile__inset')).toBeNull();
  });

  it('product walk I3: the own camera is mirrored, the own screen is not', () => {
    const cam = new MediaStream();
    const { container, rerender } = render(<CallTile view={{ ...view, video: true }} {...rest} own media={cam} mirror />);
    expect(container.querySelector('video')).toHaveAttribute('data-mirror', 'true');
    rerender(<CallTile view={{ ...view, video: true, screen: true }} {...rest} own source="screen" media={new MediaStream()} mirror={false} />);
    expect(container.querySelector('video')).not.toHaveAttribute('data-mirror');
  });


  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><ul><CallTile {...(meta.args as CallTileProps)} /></ul></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  it('given no onFocusTile it has its own li, no focus button and no full-screen button', () => {
    Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: true });
    const { container } = render(<CallTile view={{ ...view, video: true, screen: true }} {...rest} media={new MediaStream()} mirror />);
    Reflect.deleteProperty(document, 'fullscreenEnabled');
    const li = container.firstElementChild!;
    expect(li.tagName).toBe('LI');
    expect(li).toHaveClass('d-call-stage__tile');
    expect(li).toHaveAttribute('data-source', 'camera');
    expect(container.querySelectorAll('video')).toHaveLength(1);
    expect(container.querySelector('video')).toHaveAttribute('data-mirror', 'true');
    expect(container.querySelector('.d-call-tile__focus')).toBeNull();
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('mirror true mirrors the media element', () => {
    const { container } = render(<CallTile view={{ ...view, video: true }} {...rest} own media={new MediaStream()} mirror />);
    expect(container.querySelector('video')).toHaveAttribute('data-mirror', 'true');
  });
});
