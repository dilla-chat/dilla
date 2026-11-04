import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { Button } from '../Button/Button.tsx';
import { ToastHost, newToastHostId, showToast } from './Toast.tsx';

function Over25MBToast() {
  const [host] = useState(newToastHostId);
  return <><Button onClick={() => showToast('Files over 25 MB can’t be sent from a browser')}>Show toast</Button><ToastHost id={host} /></>;
}

const meta = { title: 'Feedback/Toast', component: Over25MBToast, globals: { theme: 'signal' } } satisfies Meta<typeof Over25MBToast>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Over25MB: Story = {};
