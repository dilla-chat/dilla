import { render } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { Icon } from './Icon.tsx';
import { ICONS, type IconName } from './icons.ts';

const NAMES = ['mic', 'mic-off', 'headphones', 'headphone-off', 'video', 'video-off', 'monitor-up', 'monitor-x', 'phone-off'] as const;

const SHELL_NAMES = [
  'server', 'message-square', 'activity', 'user', 'chevron-left', 'chevron-right', 'chevron-down', 'x', 'check',
  'triangle-alert', 'pause', 'ellipsis-vertical', 'hash', 'volume-2', 'volume-x', 'volume-off', 'megaphone', 'messages-square', 'file', 'play', 'eye', 'settings', 'plus', 'maximize', 'minimize', 'search',
  'lock', 'arrow-up', 'arrow-down', 'log-out', 'shield', 'keyboard', 'monitor-smartphone', 'phone', 'square-pen', 'at-sign', 'pin', 'users', 'ellipsis', 'crown',
] as const;

/** Signal wave A: the Lucide 1.57.0 geometry of each added icon (lucide-static@1.57.0/icons/<name>.svg). */
const SIGNAL_ICONS = {
  'smile': ['M15 10V9', 'M16.472 15a6 6 0 01-8.943 0', 'M9 10V9', 'circle'],
  'reply': ['M20 18v-2a4 4 0 0 0-4-4H4', 'm9 17-5-5 5-5'],
  'paperclip': ['m16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551'],
  'bell': ['M10.268 21a2 2 0 0 0 3.464 0', 'M3.262 15.326A1 1 0 0 0 4 17h16a1 1 0 0 0 .74-1.673C19.41 13.956 18 12.499 18 8A6 6 0 0 0 6 8c0 4.499-1.411 5.956-2.738 7.326'],
  'inbox': ['polyline', 'M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z'],
  'copy': ['rect', 'M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2'],
  'download': ['M12 15V3', 'M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4', 'm7 10 5 5 5-5'],
  'loader': ['M12 2v4', 'm16.2 7.8 2.9-2.9', 'M18 12h4', 'm16.2 16.2 2.9 2.9', 'M12 18v4', 'm4.9 19.1 2.9-2.9', 'M2 12h4', 'm4.9 4.9 2.9 2.9'],
  'moon': ['M20.985 12.486a9 9 0 1 1-9.473-9.472c.405-.022.617.46.402.803a6 6 0 0 0 8.268 8.268c.344-.215.825-.004.803.401'],
  'link': ['M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71', 'M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71'],
  'trash-2': ['M10 11v6', 'M14 11v6', 'M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6', 'M3 6h18', 'M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2'],
  'chevron-up': ['m18 15-6-6-6 6'],
  'fingerprint': ['M12 10a2 2 0 0 0-2 2c0 1.02-.1 2.51-.26 4', 'M14 13.12c0 2.38 0 6.38-1 8.88', 'M17.29 21.02c.12-.6.43-2.3.5-3.02', 'M2 12a10 10 0 0 1 18-6', 'M2 16h.01', 'M21.8 16c.2-2 .131-5.354 0-6', 'M5 19.5C5.5 18 6 15 6 12a6 6 0 0 1 .34-2', 'M8.65 22c.21-.66.45-1.32.57-2', 'M9 6.8a6 6 0 0 1 9 5.2v2'],
  'smartphone': ['rect', 'M12 18h.01'],
  'key': ['m15.5 7.5 2.3 2.3a1 1 0 0 0 1.4 0l2.1-2.1a1 1 0 0 0 0-1.4L19 4', 'm21 2-9.6 9.6', 'circle'],
  // Fix round 3: 'key' and 'youtube' are the prototype's (lucide-static 0.460.0), not 1.57.0.
  'youtube': ['M2.5 17a24.12 24.12 0 0 1 0-10 2 2 0 0 1 1.4-1.4 49.56 49.56 0 0 1 16.2 0A2 2 0 0 1 21.5 7a24.12 24.12 0 0 1 0 10 2 2 0 0 1-1.4 1.4 49.55 49.55 0 0 1-16.2 0A2 2 0 0 1 2.5 17', 'm10 15 5-3-5-3z'],
  'square': ['rect'],
  'arrow-right': ['M5 12h14', 'm12 5 7 7-7 7'],
  // The prototype loader's pinned Lucide 0.460.0 Home = House alias.
  'house': ['M15 21v-8a1 1 0 0 0-1-1h-4a1 1 0 0 0-1 1v8', 'M3 10a2 2 0 0 1 .709-1.528l7-5.999a2 2 0 0 1 2.582 0l7 5.999A2 2 0 0 1 21 10v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z'],
  // Task 6b fix round 1: the reminders popover's row icon (the prototype's alarm-clock).
  'alarm-clock': ['circle', 'M12 9v4l2 2', 'M5 3 2 6', 'm22 6-3-3', 'M6.38 18.7 4 21', 'M17.64 18.67 20 21'],
  // Task 7: the reaction bar's add button and the composer's </> button.
  'smile-plus': ['M13.267 2.08a10 10 0 108.653 8.653', 'M15 10V9', 'M16 5h6', 'M16.472 15a6 6 0 01-8.943 0', 'M19 2v6', 'M9 10V9'],
  'code-xml': ['m18 16 4-4-4-4', 'm6 8-4 4 4 4', 'm14.5 4-5 16'],
  // Task 8: the focused voice tile's Grid button (the prototype's layout-grid).
  'layout-grid': ['rect', 'rect', 'rect', 'rect'],
  // Task 8b: the NowPlaying player's skip, queue toggle and art placeholder.
  'skip-forward': ['M21 4v16', 'M6.029 4.285A2 2 0 0 0 3 6v12a2 2 0 0 0 3.029 1.715l9.997-5.998a2 2 0 0 0 .003-3.432z'],
  'list-music': ['M16 5H3', 'M11 12H3', 'M11 19H3', 'M21 16V5', 'circle'],
  'music': ['M9 18V5l12-2v13', 'circle', 'circle'],
  // Task 10 fix round 1: the settings nav's Bot, Appearance, Bans, Moderation log and Join roles.
  'bot': ['M12 8V4H8', 'rect', 'M2 14h2', 'M20 14h2', 'M15 13v2', 'M9 13v2'],
  'palette': ['M12 22a1 1 0 0 1 0-20 10 9 0 0 1 10 9 5 5 0 0 1-5 5h-2.25a1.75 1.75 0 0 0-1.4 2.8l.3.4a1.75 1.75 0 0 1-1.4 2.8z', 'circle', 'circle', 'circle', 'circle'],
  'ban': ['circle', 'M4.929 4.929 19.07 19.071'],
  'scroll-text': ['M15 12h-5', 'M15 8h-5', 'M19 17V5a2 2 0 0 0-2-2H4', 'M8 21h12a2 2 0 0 0 2-2v-1a1 1 0 0 0-1-1H11a1 1 0 0 0-1 1v1a2 2 0 1 1-4 0V5a2 2 0 1 0-4 0v2a1 1 0 0 0 1 1h3'],
  'user-plus': ['M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2', 'circle', 'line', 'line'],
  // Task 8c: the share picker's sources.
  'monitor': ['rect', 'line', 'line'],
  'app-window': ['rect', 'M10 4v4', 'M2 8h20', 'M6 4v4'],
  'globe': ['circle', 'M12 2a14.5 14.5 0 0 0 0 20 14.5 14.5 0 0 0 0-20', 'M2 12h20'],
  'vote': ['m9 12 2 2 4-4', 'M5 7c0-1.1.9-2 2-2h10a2 2 0 0 1 2 2v12H5V7Z', 'M22 19H2'],
  'disc-3': ['circle', 'M6 12c0-1.7.7-3.2 1.8-4.2', 'circle', 'M18 12c0 1.7-.7 3.2-1.8 4.2'],
  'timer': ['line', 'line', 'circle'],
  'hand': ['M18 11V6a2 2 0 0 0-2-2a2 2 0 0 0-2 2', 'M14 10V4a2 2 0 0 0-2-2a2 2 0 0 0-2 2v2', 'M10 10.5V6a2 2 0 0 0-2-2a2 2 0 0 0-2 2v8', 'M18 8a2 2 0 1 1 4 0v6a8 8 0 0 1-8 8h-2c-2.8 0-4.5-.86-5.99-2.34l-3.6-3.6a2 2 0 0 1 2.83-2.82L7 15'],
  'user-x': ['M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2', 'circle', 'line', 'line'],
  'message-square-x': ['M22 17a2 2 0 0 1-2 2H6.828a2 2 0 0 0-1.414.586l-2.202 2.202A.71.71 0 0 1 2 21.286V5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2z', 'm14.5 8.5-5 5', 'm9.5 8.5 5 5'],
  // Wave B Task 2: the command picker's icons (prototype CMDS and the plan's Q2 table).
  'circle-help': ['circle', 'M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3', 'M12 17h.01'],
  'sparkles': ['M11.017 2.814a1 1 0 0 1 1.966 0l1.051 5.558a2 2 0 0 0 1.594 1.594l5.558 1.051a1 1 0 0 1 0 1.966l-5.558 1.051a2 2 0 0 0-1.594 1.594l-1.051 5.558a1 1 0 0 1-1.966 0l-1.051-5.558a2 2 0 0 0-1.594-1.594l-5.558-1.051a1 1 0 0 1 0-1.966l5.558-1.051a2 2 0 0 0 1.594-1.594z', 'M20 2v4', 'M22 4h-4', 'circle'],
  'table': ['M12 3v18', 'rect', 'M3 9h18', 'M3 15h18'],
  'code': ['m16 18 6-6-6-6', 'm8 6-6 6 6 6'],
  'image': ['rect', 'circle', 'm21 15-3.086-3.086a2 2 0 0 0-2.828 0L6 21'],
  'pencil': ['M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z', 'm15 5 4 4'],
  'fast-forward': ['M12 6a2 2 0 0 1 3.414-1.414l6 6a2 2 0 0 1 0 2.828l-6 6A2 2 0 0 1 12 18z', 'M2 6a2 2 0 0 1 3.414-1.414l6 6a2 2 0 0 1 0 2.828l-6 6A2 2 0 0 1 2 18z'],
  'shuffle': ['m18 14 4 4-4 4', 'm18 2 4 4-4 4', 'M2 18h1.973a4 4 0 0 0 3.3-1.7l5.454-8.6a4 4 0 0 1 3.3-1.7H22', 'M2 6h1.972a4 4 0 0 1 3.6 2.2', 'M22 18h-6.041a4 4 0 0 1-3.3-1.8l-.359-.45'],
  'repeat': ['m17 2 4 4-4 4', 'M3 11v-1a4 4 0 0 1 4-4h14', 'm7 22-4-4 4-4', 'M21 13v1a4 4 0 0 1-4 4H3'],
  'timer-off': ['M10 2h4', 'M4.6 11a8 8 0 0 0 1.7 8.7 8 8 0 0 0 8.7 1.7', 'M7.4 7.4a8 8 0 0 1 10.3 1 8 8 0 0 1 .9 10.2', 'm2 2 20 20', 'M12 12v-2'],
  // Wave B task 3b: the recovery key's Print (Lucide 1.57.0 printer).
  'printer': ['M6 18H4a2 2 0 0 1-2-2v-5a2 2 0 0 1 2-2h16a2 2 0 0 1 2 2v5a2 2 0 0 1-2 2h-2', 'M6 9V3a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v6', 'rect'],
} as const;
const SIGNAL_NAMES = Object.keys(SIGNAL_ICONS) as (keyof typeof SIGNAL_ICONS)[];

describe('Icon', () => {
  it('vendors exactly the voice icons, the shell icons and the Signal icons', () => {
    expect(Object.keys(ICONS).sort()).toEqual([...NAMES, ...SHELL_NAMES, ...SIGNAL_NAMES].sort());
  });

  it.each(SIGNAL_NAMES)('%s renders an svg with its pinned Lucide geometry', (name) => {
    const { container } = render(<Icon name={name} />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveAttribute('data-icon', name);
    const want = SIGNAL_ICONS[name];
    expect(svg?.children.length).toBe(want.length);
    const paths = Array.from(svg?.querySelectorAll('path') ?? []).map(p => p.getAttribute('d'));
    expect(paths).toEqual(want.filter(w => w.length > 8 || /^[Mm]/.test(w)).filter(w => !['circle', 'rect', 'polyline'].includes(w)));
    for (const tag of ['circle', 'rect', 'polyline'] as const) {
      expect(svg?.querySelectorAll(tag).length).toBe(want.filter(w => w === tag).length);
    }
  });

  it.each(SHELL_NAMES)('%s renders its geometry', (name: IconName) => {
    const { container } = render(<Icon name={name} />);
    const svg = container.querySelector('svg');
    expect(svg).toHaveAttribute('data-icon', name);
    expect(svg?.children.length).toBeGreaterThan(0);
  });

  it.each(NAMES)('%s renders one hidden, unfocusable, monoline svg with its geometry', (name: IconName) => {
    const { container } = render(<Icon name={name} />);
    const svgs = container.querySelectorAll('svg');
    expect(svgs).toHaveLength(1);
    const svg = svgs[0];
    expect(svg).toHaveAttribute('aria-hidden', 'true');
    expect(svg).toHaveAttribute('focusable', 'false');
    expect(svg).toHaveAttribute('viewBox', '0 0 24 24');
    expect(svg).toHaveAttribute('fill', 'none');
    expect(svg).toHaveAttribute('stroke', 'currentColor');
    expect(svg).toHaveAttribute('stroke-width', '2');
    expect(svg).toHaveAttribute('stroke-linecap', 'round');
    expect(svg).toHaveAttribute('stroke-linejoin', 'round');
    expect(svg).toHaveAttribute('width', '20');
    expect(svg.children.length).toBe(ICONS[name].length);
    for (const p of Array.from(svg.querySelectorAll('path'))) expect(p.getAttribute('d')).toMatch(/^[Mm]/);
  });

  it('the element count of each icon stays as vendored', () => {
    const counts = Object.fromEntries(NAMES.map(n => [n, render(<Icon name={n} />).container.querySelector('svg')?.children.length]));
    expect(counts).toMatchInlineSnapshot(`
      {
        "headphone-off": 5,
        "headphones": 1,
        "mic": 3,
        "mic-off": 6,
        "monitor-up": 5,
        "monitor-x": 5,
        "phone-off": 3,
        "video": 2,
        "video-off": 3,
      }
    `);
  });

  it('takes a size in CSS px', () => {
    const { container } = render(<Icon name="mic" size={16} />);
    expect(container.querySelector('svg')).toHaveAttribute('width', '16');
    expect(container.querySelector('svg')).toHaveAttribute('height', '16');
  });
});
