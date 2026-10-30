import type { Meta, StoryObj } from '@storybook/react-vite';
import { Banner } from './Banner.tsx';

// Every string is L-COPY-01's (onboarding.keys.registering, onboarding.error.*).
const meta = { title: 'Form/Banner', component: Banner, args: { tone: 'info', children: 'Creating your account on dilla.thim.dev…' } } satisfies Meta<typeof Banner>;
export default meta;
type Story = StoryObj<typeof meta>;
export const Info: Story = {};
export const Warn: Story = { args: { tone: 'warn', children: 'Too many attempts from this network. Try again in 40 s.' } };
export const Danger: Story = { args: { tone: 'danger', children: 'The account could not be created (E_CORE_STATE). Try again.' } };
export const WithAction: Story = { args: { tone: 'danger', children: 'The connection dropped while creating your account. Reload the page to finish.', action: { label: 'Reload', onAction: () => {} } } };
