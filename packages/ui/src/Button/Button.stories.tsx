import type { Meta, StoryObj } from '@storybook/react-vite';
import { Button } from './Button.tsx';
import { Icon } from '../Icon/Icon.tsx';
import { phonePrototypeCases } from './phonePrototypeCases.ts';

const meta = { title: 'Primitives/Button', component: Button, args: { children: 'Save changes' } } satisfies Meta<typeof Button>;
export default meta;
type Story = StoryObj<typeof meta>;

export const Default: Story = {};
export const Accent: Story = { args: { variant: 'accent', keyHint: '⌘↵' } };
export const Danger: Story = { args: { variant: 'danger', children: 'Leave voice', keyHint: 'X' } };
export const DangerWithIcon: Story = {
  args: { variant: 'danger', children: <><Icon name="log-out" size={14} />Leave</> },
  play: ({ canvasElement }) => {
    const label = canvasElement.querySelector('.d-btn__label')!;
    const icon = label.querySelector('svg')!.getBoundingClientRect();
    const text = document.createRange();
    text.selectNode(label.lastChild!);
    const word = text.getBoundingClientRect();
    const gap = word.left - icon.right;
    const centerOffset = (word.top + word.bottom - icon.top - icon.bottom) / 2;
    if (Math.abs(gap - 8) > .1 || Math.abs(centerOffset) > 1) {
      throw new Error(`Danger button icon must precede the label on one centered line: gap=${gap}, centerOffset=${centerOffset}`);
    }
  },
};
export const Ghost: Story = { args: { variant: 'ghost', children: 'Cancel', keyHint: 'esc' } };
// Pressed state must never be colour-only: aria-pressed, a `[x]`/`[ ]` mark and an inset underline bar accompany the
// colour change, under one label in both states (the WAI-ARIA toggle pattern; WB a11y I4, ruling 2026-10-14). Two
// stories show both states so the non-colour cues are visible side by side.
export const Unpressed: Story = { args: { pressed: false, children: 'Mute', keyHint: 'M' } };
export const Pressed: Story = { args: { pressed: true, children: 'Mute', keyHint: 'M' } };
export const Small: Story = { args: { size: 'sm', children: 'Verify' } };

// Contexts share the real Button and auth frame; exact geometry also runs at the declared phone viewport.
const prototypeHeights: Story = {
  render: () => <div data-phone-button-audit="">
    <style>{'[data-phone-button-audit] * { animation: none !important; transition: none !important; }'}</style>
    {phonePrototypeCases.map(({ name, content, fixtureIndex }, i) => fixtureIndex === undefined ? <section key={name}>
      <h2 style={{ fontSize: 14 }}>{name}</h2>
      <div data-button-case={i}>{content}</div>
    </section> : null)}
  </div>,
  play: ({ canvasElement }) => {
    phonePrototypeCases.forEach(({ name, selector, height, fixtureIndex }, i) => {
      const control = canvasElement.querySelector(`[data-button-case="${fixtureIndex ?? i}"] ${selector}`);
      const actual = control?.getBoundingClientRect().height;
      if (actual !== height) throw new Error(`${name}: expected ${height}px, got ${actual}px`);
    });
  },
};
export const DesktopPrototypeHeights: Story = { ...prototypeHeights };
export const PhonePrototypeHeights: Story = { ...prototypeHeights, parameters: { phoneViewport: true } };
