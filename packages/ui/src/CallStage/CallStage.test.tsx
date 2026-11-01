import { fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { axe } from 'vitest-axe';
import { CallStage, type CallStageProps } from './CallStage.tsx';
import type { CallTileProps } from '../CallTile/CallTile.tsx';
import meta from './CallStage.stories.tsx';

if (typeof globalThis.MediaStream === 'undefined') {
  (globalThis as { MediaStream?: unknown }).MediaStream = class MediaStream {};
}

const stateLabels: CallTileProps['labels']['state'] = {
  speaking: 'speaking', muted: 'muted', deafened: 'deafened', notHeard: 'not heard yet', unverified: 'unverified stream',
  connecting: 'connecting', video: 'camera on', screen: 'sharing screen', web: 'web', bot: 'bot', noSound: 'no sound',
};
function tile(name: string, media: MediaStream | null): CallTileProps {
  return {
    view: { deviceId: `d-${name}`, userId: `u-${name}`, name, state: 'ok', speaking: false, muted: false, deafened: false,
      video: media !== null, screen: false, web: true, bot: false },
    own: false, volume: 100, media, labels: { state: stateLabels, volume: `volume for ${name}`, volumeValue: '{n} %' },
  };
}
const labels: CallStageProps['labels'] = {
  code: 'call code', codeHelp: 'The code changes whenever someone joins or leaves.', empty: 'Nobody is in voice yet.', tiles: 'people in voice',
};
const base: CallStageProps = {
  title: 'voice harbour', tiles: [tile('ada', new MediaStream()), tile('bo', new MediaStream())], code: '012345678901234567890123456789',
  notice: null, controls: <button type="button">mute</button>, outgoing: null, labels,
};
const pausedText = 'ada’s identity changed, so this call is paused for you: nothing you say is sent and nothing is played.';

afterEach(() => { delete document.documentElement.dataset.theme; });

describe('CallStage', () => {
  it('CallStage with the pause notice (RV-3) shows its action, focuses it once, and renders no media element', async () => {
    const onAction = vi.fn();
    const notice = { kind: 'paused' as const, text: pausedText, action: { label: 'compare safety numbers', onAction }, focusOnce: true };
    const { container, rerender } = render(<CallStage {...base} notice={notice} />);
    expect(container.querySelector('video, audio')).toBeNull();
    const action = screen.getByRole('button', { name: 'compare safety numbers' });
    expect(action).toHaveFocus();
    expect(screen.getByRole('status')).toHaveTextContent(pausedText);
    screen.getByRole('button', { name: 'mute' }).focus();
    rerender(<CallStage {...base} notice={{ ...notice }} />);
    expect(action).not.toHaveFocus();
    expect(container.querySelector('video, audio')).toBeNull();
    await userEvent.click(action);
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it('without a notice the tiles show their verified video, as a list named for the people', () => {
    const { container } = render(<CallStage {...base} />);
    expect(container.querySelectorAll('video')).toHaveLength(2);
    expect(screen.getByRole('list', { name: 'people in voice' })).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getByRole('heading', { name: 'voice harbour' })).toBeInTheDocument();
  });

  it('keeps the call code behind a closed disclosure, in six groups of five; Escape closes it and returns focus', async () => {
    const { container } = render(<CallStage {...base} />);
    const details = container.querySelector('details')!;
    expect(details.open).toBe(false);
    const summary = container.querySelector('summary')!;
    expect(summary).toHaveTextContent('call code');
    await userEvent.click(summary);
    expect(details.open).toBe(true);
    expect(screen.getByText('01234 56789 01234 56789 01234 56789')).toBeInTheDocument();
    expect(screen.getByText('The code changes whenever someone joins or leaves.')).toBeInTheDocument();
    await userEvent.keyboard('{Escape}');
    expect(details.open).toBe(false);
    expect(summary).toHaveFocus();
  });

  it('says so when nobody is in voice, shows the outgoing ring, and a notice without an action takes focus once', () => {
    const { rerender } = render(<CallStage {...base} tiles={[]} code={null} outgoing={{ text: 'ringing ada' }} />);
    expect(screen.getByText('Nobody is in voice yet.')).toBeInTheDocument();
    expect(screen.getByText('ringing ada')).toBeInTheDocument();
    expect(document.querySelector('details')).toBeNull();
    rerender(<CallStage {...base} notice={{ kind: 'info', text: 'This voice is full.', action: null, focusOnce: true }} />);
    expect(screen.getByRole('status')).toHaveFocus();
  });

  it('hides media for the pause and blocked notices only; a transient notice keeps the video (ruling)', () => {
    const { container, rerender } = render(<CallStage {...base} notice={{ kind: 'info', text: 'This voice is full.', action: null, focusOnce: false }} />);
    expect(container.querySelectorAll('video')).toHaveLength(2);
    rerender(<CallStage {...base} notice={{ kind: 'blocked', text: 'A device in this voice failed the identity check, so you left.', action: null, focusOnce: false }} />);
    expect(container.querySelector('video, audio')).toBeNull();
    rerender(<CallStage {...base} notice={{ kind: 'paused', text: pausedText, action: null, focusOnce: false }} />);
    expect(container.querySelector('video, audio')).toBeNull();
  });

  it('never says nobody is in voice while a notice explains the stage (M1)', () => {
    const { rerender } = render(<CallStage {...base} tiles={[]} notice={{ kind: 'blocked', text: 'A device in this voice failed the identity check. Joining is paused until it is removed.', action: null, focusOnce: false }} />);
    expect(screen.queryByText('Nobody is in voice yet.')).toBeNull();
    rerender(<CallStage {...base} tiles={[]} notice={null} />);
    expect(screen.getByText('Nobody is in voice yet.')).toBeInTheDocument();
  });

  it.each(['signal', 'light', 'high-contrast'])('passes axe at its default story args in %s', async theme => {
    document.documentElement.dataset.theme = theme;
    const { container } = render(<div className="d-root"><CallStage {...(meta.args as CallStageProps)} /></div>);
    // jsdom has no layout or canvas, so contrast is left to the Storybook axe run in a real browser.
    expect((await axe(container, { rules: { 'color-contrast': { enabled: false } } })).violations).toEqual([]);
  });

  it('the header slot renders before the title and the tiles, as the stage\'s 48 bar (C4)', () => {
    const { container } = render(<CallStage {...base} header={<header data-testid="bar">voice harbour</header>} />);
    const bar = screen.getByTestId('bar');
    const section = container.querySelector('.d-call-stage')!;
    expect(bar.parentElement).toHaveClass('d-call-stage__bar');
    expect(section.firstElementChild).toBe(bar.parentElement);
    expect(bar.parentElement!.compareDocumentPosition(container.querySelector('.d-call-stage__tiles')!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The section keeps its name, from a visually hidden title: the slot's own heading is not a second visible title.
    expect(screen.getByRole('region', { name: 'voice harbour' })).toBeInTheDocument();
    expect(container.querySelector('h2.d-call-stage__title')).toBeNull();
  });

  it('without a header slot the stage keeps its visible title', () => {
    const { container } = render(<CallStage {...base} />);
    expect(container.querySelector('.d-call-stage__bar')).toBeNull();
    expect(container.querySelector('h2.d-call-stage__title')).not.toBeNull();
  });

  describe('stage (grid, focus, full screen)', () => {
    function src(name: string, source: 'camera' | 'screen' | 'avatar', media: MediaStream | null, more: Partial<CallTileProps> = {}): CallTileProps {
      const t = tile(name, media);
      return { ...t, id: `d-${name}:${source}`, source, title: source === 'screen' ? `${name} screen` : name, fullscreenLabel: `full screen ${source === 'screen' ? name + ' screen' : name}`, ...more };
    }
    const sA = new MediaStream();
    const sB = new MediaStream();
    function Harness({ initial = null, onFocus, ...rest }: Partial<CallStageProps> & { initial?: string | null }) {
      const [f, setF] = useState<string | null>(initial);
      return <CallStage {...base} tiles={[src('ada', 'camera', sA), src('bo', 'screen', sB), src('bo', 'camera', null)]} {...rest}
        focusedId={f} onFocus={id => { onFocus?.(id); setF(id); }} />;
    }
    const focusBtn = (name: string) => screen.getByRole('button', { name });
    function setFullscreen(enabled: boolean, element: Element | null = null) {
      Object.defineProperty(document, 'fullscreenEnabled', { configurable: true, value: enabled });
      Object.defineProperty(document, 'fullscreenElement', { configurable: true, get: () => element });
    }
    afterEach(() => {
      Reflect.deleteProperty(document, 'fullscreenEnabled');
      Reflect.deleteProperty(document, 'fullscreenElement');
      Reflect.deleteProperty(document, 'exitFullscreen');
      Reflect.deleteProperty(HTMLElement.prototype, 'requestFullscreen');
      Reflect.deleteProperty(HTMLMediaElement.prototype, 'srcObject');
      vi.restoreAllMocks();
    });

    it('the grid band follows the count: two tiles, up to four, more (the prototype\'s tile widths)', () => {
      const four = [src('ada', 'camera', sA), src('bo', 'camera', sB), src('cy', 'camera', null), src('di', 'camera', null)];
      const { container, rerender } = render(<CallStage {...base} tiles={four.slice(0, 2)} focusedId={null} onFocus={() => {}} />);
      const ul = () => container.querySelector<HTMLElement>('.d-call-stage__tiles')!;
      expect(ul()).toHaveAttribute('data-band', 'two');
      rerender(<CallStage {...base} tiles={four} focusedId={null} onFocus={() => {}} />);
      expect(ul()).toHaveAttribute('data-band', 'four');
      rerender(<CallStage {...base} tiles={[...four, src('ed', 'camera', null)]} focusedId={null} onFocus={() => {}} />);
      expect(ul()).toHaveAttribute('data-band', 'many');
    });

    it('the focused tile has Grid (labels.grid), which clears the focus; other tiles have none', async () => {
      const onFocus = vi.fn();
      const tiles = [src('ada', 'camera', sA), src('bo', 'camera', sB)];
      render(<CallStage {...base} tiles={tiles} focusedId={tiles[0].id} onFocus={onFocus} labels={{ ...base.labels, grid: 'Grid' }} />);
      const grids = screen.getAllByRole('button', { name: 'Grid' });
      expect(grids).toHaveLength(1);
      await userEvent.click(grids[0]);
      expect(onFocus).toHaveBeenCalledWith(null);
    });

    it('focus sets data-focused on the tile and data-focused-stage on the stage; a click on the focused tile unfocuses', async () => {
      const onFocus = vi.fn();
      const { container } = render(<Harness onFocus={onFocus} />);
      const stage = container.querySelector('.d-call-stage')!;
      expect(stage).not.toHaveAttribute('data-focused-stage');
      await userEvent.click(focusBtn('bo screen'));
      expect(onFocus).toHaveBeenLastCalledWith('d-bo:screen');
      expect(stage).toHaveAttribute('data-focused-stage');
      const lis = container.querySelectorAll('li.d-call-stage__tile');
      expect([...lis].map(l => l.getAttribute('data-focused'))).toEqual(['false', 'true', 'false']);
      expect(focusBtn('bo screen')).toHaveAttribute('aria-pressed', 'true');
      await userEvent.click(focusBtn('bo screen'));
      expect(onFocus).toHaveBeenLastCalledWith(null);
      expect(stage).not.toHaveAttribute('data-focused-stage');
    });

    it('Enter and Space on a focus button focus its tile', async () => {
      const onFocus = vi.fn();
      render(<Harness onFocus={onFocus} />);
      focusBtn('ada').focus();
      await userEvent.keyboard('{Enter}');
      expect(onFocus).toHaveBeenLastCalledWith('d-ada:camera');
      await userEvent.keyboard(' ');
      expect(onFocus).toHaveBeenLastCalledWith(null);
    });

    it('Escape unfocuses and is consumed; Escape in full screen, or with nothing focused, does nothing', async () => {
      const onFocus = vi.fn();
      const outer = vi.fn();
      const { container } = render(<div onKeyDown={outer}><Harness initial="d-ada:camera" onFocus={onFocus} /></div>);
      setFullscreen(true, container.querySelector('li'));
      focusBtn('ada').focus();
      await userEvent.keyboard('{Escape}');
      expect(onFocus).not.toHaveBeenCalled();
      setFullscreen(true, null);
      outer.mockClear();
      await userEvent.keyboard('{Escape}');
      expect(onFocus).toHaveBeenCalledWith(null);
      expect(outer).not.toHaveBeenCalled();
      onFocus.mockClear();
      outer.mockClear();
      await userEvent.keyboard('{Escape}');
      expect(onFocus).not.toHaveBeenCalled();
      expect(outer).toHaveBeenCalled();
    });

    it('a focusedId that names no tile renders a grid', () => {
      const { container } = render(<Harness initial="d-nobody:camera" />);
      expect(container.querySelector('.d-call-stage')).not.toHaveAttribute('data-focused-stage');
      expect(container.querySelector('[data-focused="true"]')).toBeNull();
    });

    it('the same video elements survive focus, swap and unfocus, and srcObject is set once per stream (R27)', async () => {
      const set = vi.fn();
      Object.defineProperty(HTMLMediaElement.prototype, 'srcObject', { configurable: true, get() { return null; }, set });
      const { container } = render(<Harness />);
      const before = [...container.querySelectorAll('video')];
      expect(before).toHaveLength(2);
      const items = [...container.querySelectorAll('li')];
      await userEvent.click(focusBtn('bo screen'));
      await userEvent.click(focusBtn('ada'));
      await userEvent.click(focusBtn('ada'));
      const after = [...container.querySelectorAll('video')];
      after.forEach((v, i) => expect(v).toBe(before[i]));
      [...container.querySelectorAll('li')].forEach((l, i) => expect(l).toBe(items[i]));
      expect(set).toHaveBeenCalledTimes(2);
    });

    it('the focus button holds no button, input or link (R28); a tile has no range of its own (fix round 1: the person menu)', () => {
      const { container } = render(<Harness />);
      const buttons = container.querySelectorAll('.d-call-tile__focus');
      expect(buttons).toHaveLength(3);
      buttons.forEach(b => expect(b.querySelector('button, input, a, [tabindex]')).toBeNull());
      expect(container.querySelector('input[type="range"]')).toBeNull();
    });

    it('data-source is on the li and on .d-call-tile, and one device with two sources has distinct keys', () => {
      const err = vi.spyOn(console, 'error').mockImplementation(() => {});
      const { container } = render(<Harness />);
      const lis = [...container.querySelectorAll('li.d-call-stage__tile')];
      expect(lis.map(l => l.getAttribute('data-source'))).toEqual(['camera', 'screen', 'camera']);
      expect(lis.map(l => l.querySelector('.d-call-tile')!.getAttribute('data-source'))).toEqual(['camera', 'screen', 'camera']);
      expect(err.mock.calls.flat().join(' ')).not.toMatch(/same key|unique "key"/);
    });

    it('a screen tile has no state glyphs; a person tile keeps them; neither has a range', () => {
      const t = src('bo', 'screen', sB, { view: { ...tile('bo', sB).view, muted: true, screen: true } });
      const { container } = render(<CallStage {...base} tiles={[t, src('bo', 'camera', null, { view: { ...tile('bo', null).view, muted: true } })]} focusedId={null} onFocus={() => {}} />);
      const [screenLi, personLi] = container.querySelectorAll('li');
      expect(screenLi.querySelector('input[type="range"]')).toBeNull();
      expect(screenLi.querySelector('[data-state]')).toBeNull();
      expect(personLi.querySelector('input[type="range"]')).toBeNull();
      expect(personLi.querySelector('[data-state="muted"]')).not.toBeNull();
    });

    it('a tile without media shows the avatar; an unverified screen tile renders no video', () => {
      const bad = src('bo', 'screen', sB, { view: { ...tile('bo', sB).view, state: 'unverified' } });
      const { container } = render(<CallStage {...base} tiles={[bad, src('cy', 'avatar', null)]} focusedId={null} onFocus={() => {}} />);
      expect(container.querySelector('video')).toBeNull();
      // Signal: a screen without its picture is the striped placeholder, a person without media the avatar.
      expect(container.querySelectorAll('.d-avatar')).toHaveLength(1);
      expect(container.querySelectorAll('.d-call-tile__placeholder')).toHaveLength(1);
    });

    it('with a pause notice no screen tile renders and onFocus(null) is called once', () => {
      const onFocus = vi.fn();
      const notice = { kind: 'paused' as const, text: pausedText, action: null, focusOnce: false };
      const { container, rerender } = render(<Harness initial="d-bo:screen" onFocus={onFocus} />);
      expect(container.querySelectorAll('li')).toHaveLength(3);
      rerender(<Harness initial="d-bo:screen" onFocus={onFocus} notice={notice} />);
      expect(container.querySelectorAll('li')).toHaveLength(2);
      expect(container.querySelector('[data-source="screen"]')).toBeNull();
      expect(onFocus).toHaveBeenCalledTimes(1);
      expect(onFocus).toHaveBeenCalledWith(null);
      expect(container.querySelector('.d-call-stage')).not.toHaveAttribute('data-focused-stage');
    });

    it('the full-screen button requests full screen on the tile li, exits it when that li is full screen, and follows fullscreenchange', async () => {
      const request = vi.fn(() => Promise.resolve());
      const exit = vi.fn(() => Promise.resolve());
      Object.defineProperty(HTMLElement.prototype, 'requestFullscreen', { configurable: true, value: request });
      Object.defineProperty(document, 'exitFullscreen', { configurable: true, value: exit });
      setFullscreen(true);
      const { container } = render(<Harness initial="d-bo:screen" />);
      const btn = screen.getByRole('button', { name: 'full screen bo screen' });
      expect(btn).toHaveAttribute('aria-pressed', 'false');
      const li = container.querySelector('li[data-source="screen"]')!;
      await userEvent.click(screen.getByRole('button', { name: 'full screen bo screen' }));
      expect(request.mock.contexts[0]).toBe(li);
      setFullscreen(true, li);
      fireEvent(document, new Event('fullscreenchange'));
      expect(screen.getByRole('button', { name: 'full screen bo screen' })).toHaveAttribute('aria-pressed', 'true');
      await userEvent.click(screen.getByRole('button', { name: 'full screen bo screen' }));
      expect(exit).toHaveBeenCalledTimes(1);
    });

    it('a double-click on the focus button toggles full screen', async () => {
      const request = vi.fn(() => Promise.resolve());
      Object.defineProperty(HTMLElement.prototype, 'requestFullscreen', { configurable: true, value: request });
      setFullscreen(true);
      render(<Harness />);
      await userEvent.dblClick(focusBtn('ada'));
      expect(request).toHaveBeenCalledTimes(1);
    });

    it('Signal: only the focused tile has the full-screen button (an avatar too, as the prototype); double-click needs media', async () => {
      const request = vi.fn(() => Promise.resolve());
      Object.defineProperty(HTMLElement.prototype, 'requestFullscreen', { configurable: true, value: request });
      setFullscreen(true);
      const tiles = [src('ada', 'camera', sA), src('bo', 'camera', null), src('cy', 'avatar', null)];
      const { rerender } = render(<CallStage {...base} tiles={tiles} focusedId={null} onFocus={() => {}} />);
      expect(screen.queryByRole('button', { name: /^full screen/ })).toBeNull();
      rerender(<CallStage {...base} tiles={tiles} focusedId={tiles[2].id} onFocus={() => {}} />);
      expect(screen.getByRole('button', { name: 'full screen cy' })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: 'full screen ada' })).toBeNull();
      await userEvent.dblClick(focusBtn('bo'));
      await userEvent.dblClick(focusBtn('cy'));
      expect(request).not.toHaveBeenCalled();
    });

    it('there is no full-screen button when the document cannot do full screen', () => {
      setFullscreen(false);
      render(<Harness />);
      expect(screen.queryByRole('button', { name: /^full screen/ })).toBeNull();
    });

    it('a refused full-screen request or exit is caught', async () => {
      const caught = vi.fn();
      Object.defineProperty(HTMLElement.prototype, 'requestFullscreen', { configurable: true, value: vi.fn(() => ({ catch: caught })) });
      Object.defineProperty(document, 'exitFullscreen', { configurable: true, value: vi.fn(() => ({ catch: caught })) });
      setFullscreen(true);
      const { container } = render(<Harness initial="d-ada:camera" />);
      await userEvent.click(screen.getByRole('button', { name: 'full screen ada' }));
      setFullscreen(true, container.querySelector('li'));
      await userEvent.click(screen.getByRole('button', { name: 'full screen ada' }));
      expect(caught).toHaveBeenCalledTimes(2);
    });

    it('the focus button is described by the states in the caption, the stream states included', () => {
      const bad = src('cy', 'camera', null, { view: { ...tile('cy', null).view, muted: true, deafened: true, state: 'unverified' } });
      render(<CallStage {...base} tiles={[bad]} focusedId={null} onFocus={() => {}} />);
      const button = focusBtn('cy');
      // jsdom does not fold a child image's aria-label into a description; a browser does, so the glyphs are pinned by id.
      const states = document.getElementById((button.getAttribute('aria-describedby') ?? '').split(' ')[0])!;
      expect(states).toContainElement(screen.getByRole('img', { name: 'muted' }));
      expect(states).toContainElement(screen.getByRole('img', { name: 'deafened' }));
      expect(button).toHaveAccessibleDescription(/unverified stream/);
    });

    it('focusing the only tile leaves the grid layout: no focused stage, so no empty filmstrip row', () => {
      const { container } = render(<CallStage {...base} tiles={[src('ada', 'camera', sA)]} focusedId="d-ada:camera" onFocus={() => {}} />);
      expect(container.querySelector('li')).toHaveAttribute('data-focused', 'true');
      expect(container.querySelector('.d-call-stage')).not.toHaveAttribute('data-focused-stage');
      expect(container.querySelector('.d-call-stage__tiles')).not.toHaveAttribute('style', expect.stringContaining('--d-strip-n'));
    });

    it('stage mode is on only when onFocus is given (pre-flight C-2)', () => {
      const { container, rerender } = render(<CallStage {...base} focusedId={null} onFocus={() => {}} />);
      expect(container.querySelector('.d-call-stage__tiles')).toHaveAttribute('data-tiles', 'stage');
      rerender(<CallStage {...base} />);
      expect(container.querySelector('.d-call-stage__tiles')).not.toHaveAttribute('data-tiles');
    });
  });

  describe('Signal §5.6: the not-joined preview and the call bar', () => {
    const preview = { people: [{ id: 'a', name: 'Ada', speaking: true }, { id: 'b', name: 'Björn', speaking: false }], name: 'harbour',
      text: 'Ada and Björn are here', join: { label: 'Join voice', onJoin: vi.fn() } };

    it('the preview stands for the tiles and the call bar: faces, the room, who is there and Join voice', async () => {
      const { container } = render(<CallStage {...base} preview={preview} controls={<button type="button">never</button>} />);
      expect(container.querySelector('.d-call-stage__tiles')).toBeNull();
      expect(screen.queryByRole('button', { name: 'never' })).toBeNull();
      expect(container.querySelectorAll('.d-call-stage__face')).toHaveLength(2);
      expect(container.querySelector('.d-call-stage__face[data-speaking="true"]')).not.toBeNull();
      expect(screen.getByText('harbour')).toBeInTheDocument();
      expect(screen.getByText('Ada and Björn are here')).toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: 'Join voice' }));
      expect(preview.join.onJoin).toHaveBeenCalledTimes(1);
    });

    it('a held join shows no Join voice', () => {
      render(<CallStage {...base} preview={{ ...preview, join: null }} />);
      expect(screen.queryByRole('button', { name: 'Join voice' })).toBeNull();
    });

    it('with bar the controls region is the call bar: the room, the stats and the controls; no call code (fix round 1: the voice row menu)', () => {
      const { container } = render(<CallStage {...base} code="284719306451873920465128390715" bar={{ name: 'harbour', stats: '24 ms · ↑ 38 ↓ 71 kb/s' }}
        controls={<button type="button">Mute</button>} />);
      const bar = container.querySelector('.d-call-stage__controls[data-bar]')!;
      expect(bar).not.toBeNull();
      expect(bar.querySelector('.d-call-stage__room')).toHaveTextContent('harbour');
      expect(bar.querySelector('.d-call-stage__stats')).toHaveTextContent('24 ms · ↑ 38 ↓ 71 kb/s');
      expect(container.querySelector('.d-call-stage__code')).toBeNull();
      expect(bar).toContainElement(screen.getByRole('button', { name: 'Mute' }));
    });
  });
});
