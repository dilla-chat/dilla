import type { Meta, StoryObj } from '@storybook/react-vite';
import { useState } from 'react';
import { CodeBoxes } from './CodeBoxes.tsx';

// Wave B task 3: the prototype's code step boxes, at the auth card's 342px content width.
function Controlled(props: { initial?: string; invalid?: boolean; autoFocus?: boolean }) {
  const [value, setValue] = useState(props.initial ?? '');
  return <CodeBoxes id="code" label="Code" value={value} onChange={setValue} invalid={props.invalid} autoFocus={props.autoFocus} />;
}
const meta = {
  title: 'Form/CodeBoxes',
  decorators: [Story => <div style={{ width: 342 }}><Story /></div>],
} satisfies Meta;
export default meta;
type Story = StoryObj<typeof meta>;

export const Empty: Story = { render: () => <Controlled autoFocus /> };
export const Partial: Story = { render: () => <Controlled initial="482" autoFocus /> };
export const Full: Story = { render: () => <Controlled initial="482913" autoFocus /> };
export const Unfocused: Story = { render: () => <Controlled initial="48" /> };
export const Invalid: Story = { render: () => <Controlled initial="000000" invalid /> };
