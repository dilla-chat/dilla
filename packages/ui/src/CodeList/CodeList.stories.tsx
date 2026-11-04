import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState } from 'react';
import { CodeList } from './CodeList.tsx';

// The server's backup codes: 26 base32 characters in hyphenated groups of five (internal/auth/recovery.go).
const CODES = [
  'JBSWY-3DPEH-PK3PX-PJBSW-Y3DPE-H', '5ICO4-DTHPV-D23TY-J7KQ2-MRB6E-A', 'K3M7Q-W2DX2-RT5PN-AHV5C-J2ZEM-T',
  'YF2GK-DR3WP-N7HZ2-CQ6XA-B5TV7-L', 'N7HZ2-CQ6XA-B5TV7-LYF2G-KDR3W-P', 'QW9DX-2RT0P-NAHV5-CJ8ZE-M4TBS-6',
  'XK2MN-7PQ3R-TV5WY-ZA6BC-D4EFG-H', 'R3WPZ-N7HC9-QXA5T-VJ2KD-MBE6F-Y',
];

/** Every code stays inside its cell, inside the list, and clear of its neighbours. */
async function assertNoOverflow(canvasElement: HTMLElement): Promise<void> {
  await document.fonts.ready;
  const grid = canvasElement.querySelector<HTMLElement>('.d-code-list__grid');
  const items = Array.from(canvasElement.querySelectorAll<HTMLElement>('.d-code-list__code'));
  if (grid === null || items.length === 0) throw new Error('the code list did not render');
  if (grid.scrollWidth > grid.clientWidth) throw new Error(`the list overflows (${grid.scrollWidth} > ${grid.clientWidth})`);
  const boxes = items.map(li => li.getBoundingClientRect());
  items.forEach((li, i) => {
    if (li.scrollWidth > li.clientWidth) throw new Error(`code ${i + 1} overflows its cell (${li.scrollWidth} > ${li.clientWidth})`);
    const text = li.querySelector<HTMLElement>('.d-code-list__text')!;
    if (text.scrollWidth > text.clientWidth) throw new Error(`code ${i + 1} text overflows (${text.scrollWidth} > ${text.clientWidth})`);
    li.querySelectorAll<HTMLElement>('.d-code-list__group').forEach(g => {
      const r = g.getBoundingClientRect();
      if (r.left < boxes[i]!.left - 0.5 || r.right > boxes[i]!.right + 0.5) throw new Error(`a group of code ${i + 1} sticks out of its cell`);
    });
    boxes.forEach((b, j) => {
      const a = boxes[i]!;
      if (j > i && a.left < b.right - 0.5 && b.left < a.right - 0.5 && a.top < b.bottom - 0.5 && b.top < a.bottom - 0.5) {
        throw new Error(`code ${i + 1} overlaps code ${j + 1}`);
      }
    });
  });
}

const meta = {
  title: 'Ceremony/CodeList', component: CodeList,
  args: { codes: CODES, label: 'Backup codes', onCopy: () => {}, onPrint: () => {}, labels: { copy: 'Copy codes', print: 'Print codes' } },
  play: ({ canvasElement }) => assertNoOverflow(canvasElement),
} satisfies Meta<typeof CodeList>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Wide: Story = { decorators: [Story => <div style={{ width: '100%', maxWidth: 1280 }}><Story /></div>] };
export const Narrow: Story = { decorators: [Story => <div style={{ width: 360 }}><Story /></div>] };
// 200 % zoom of a 360 px viewport leaves 180 CSS px.
export const Zoom200: Story = { decorators: [Story => <div style={{ width: 180 }}><Story /></div>] };

// The signup done step uses this same in-memory disclosure at desktop and phone widths.
export const CollapsedDesktop: Story = {
  render: function Collapsed(args) {
    const [expanded, setExpanded] = useState(false);
    return <div style={{ width: '100%', maxWidth: 400 }}><CodeList {...args} disclosure={{
      expanded, onToggle: () => { setExpanded(value => !value); },
      count: `${args.codes.length} backup codes`, show: 'Show', hide: 'Hide',
    }} /></div>;
  },
  play: async ({ canvasElement }) => {
    const codes = () => canvasElement.querySelectorAll('.d-code-list__code');
    const toggle = () => canvasElement.querySelector<HTMLButtonElement>('button[aria-expanded]')!;
    if (codes().length !== 0 || toggle().getAttribute('aria-expanded') !== 'false') throw new Error('Collapsed desktop codes must be absent from the DOM');
    toggle().click();
    await new Promise<void>(resolve => { requestAnimationFrame(() => resolve()); });
    if (codes().length !== CODES.length || toggle().getAttribute('aria-expanded') !== 'true') throw new Error('Show must mount every desktop backup code');
    await assertNoOverflow(canvasElement);
    toggle().click();
    await new Promise<void>(resolve => { requestAnimationFrame(() => resolve()); });
    if (codes().length !== 0 || toggle().getAttribute('aria-expanded') !== 'false') throw new Error('Hide must remove desktop backup codes from the DOM');
  },
};
