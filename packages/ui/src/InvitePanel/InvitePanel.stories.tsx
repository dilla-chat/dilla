import type { Meta, StoryObj } from '@storybook/react-vite';
import { InvitePanel } from './InvitePanel.tsx';
import { EXPIRY, INVITE_LABELS, LiveInvites, USES } from './InvitePanel.fixture.tsx';
import { t } from '../../../web/src/strings/index.ts';

const meta: Meta<typeof InvitePanel> = { title: 'Settings/InvitePanel', component: InvitePanel, parameters: { layout: 'padded' } };
export default meta;
type Story = StoryObj<typeof InvitePanel>;

export const Default: Story = { render: () => <LiveInvites /> };
export const Made: Story = { render: () => <LiveInvites made /> };
/** Someone with Create invite but not Manage server: only the finite options. */
export const Member: Story = { render: () => <LiveInvites manager={false} /> };
export const Refused: Story = {
  args: {
    expiry: { label: t('serverSettings.invites.expires'), options: EXPIRY, value: '0', onChange: () => {} },
    uses: { label: t('serverSettings.invites.uses'), options: USES, value: '0', onChange: () => {} },
    made: null, making: false, onMake: () => {}, onCopy: () => {}, message: { text: t('invite.error.forbidden'), alert: true, n: 1 },
    activeTitle: t('serverSettings.invites.active', { count: 0 }), rows: [], onRevoke: () => {}, labels: INVITE_LABELS,
  },
};
export const Phone: Story = { parameters: { phoneViewport: true }, render: () => <LiveInvites made /> };
