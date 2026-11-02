import type { Meta, StoryObj } from '@storybook/react-vite';
import { SettingsRow } from './SettingsRow.tsx';
import { Segmented } from '../Segmented/Segmented.tsx';
import { Toggle } from '../Toggle/Toggle.tsx';
import { Button } from '../Button/Button.tsx';

// Every string is L-COPY-02's (notify.*; the per-channel row's name is notify.title.channel filled in).
const noop = () => {};
const BODY = 'Desktop notifications show while a dilla tab is open. Nothing is shown when every tab is closed.';
const NOTIFY = [{ value: 'dms-mentions', label: 'direct messages and mentions' }, { value: 'everything', label: 'every message' }, { value: 'nothing', label: 'nothing' }];
const MODES = [{ value: 'default', label: 'default' }, { value: 'all', label: 'all' }, { value: 'mentions', label: 'mentions' }, { value: 'nothing', label: 'nothing' }];
const GENERAL = '#general · Midgard Crew';
const meta: Meta = { title: 'Settings/SettingsRow', decorators: [Story => <div style={{ maxWidth: 640 }}><Story /></div>] };
export default meta;
type Story = StoryObj;
export const Permission: Story = {
  render: () => <SettingsRow id="notify-permission" label="desktop notifications" hint={BODY}><Button variant="accent">turn on</Button></SettingsRow>,
};
export const NotifyDefault: Story = {
  render: () => (
    <SettingsRow id="notify-default" label="notify me about">
      <Segmented id="notify-default-control" label="notify me about" options={NOTIFY} value="dms-mentions" onChange={noop} />
    </SettingsRow>
  ),
};
export const PerChannel: Story = {
  render: () => (
    <SettingsRow id="notify-general" label={GENERAL}>
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem' }}>
        <Segmented id="notify-general-mode" label={GENERAL} options={MODES} value="all" onChange={noop} />
        <Toggle id="mute-general" label="mute" checked={false} onChange={noop} showLabel />
      </div>
    </SettingsRow>
  ),
};
// A phone layout stacks the label over a full-width control (H5); `data-phone` is the hook for a surface outside the shell.
export const Stacked: Story = {
  render: () => (
    <div data-phone style={{ maxWidth: '22.5rem' }}>
      <SettingsRow id="notify-default" label="notify me about" hint="Rows left on default follow this choice.">
        <Segmented id="notify-default-control" label="notify me about" options={NOTIFY} value="dms-mentions" onChange={noop} />
      </SettingsRow>
      <SettingsRow id="notify-permission" label="desktop notifications" hint={BODY}><Button variant="accent">turn on</Button></SettingsRow>
    </div>
  ),
};
// The grammar: two rows one under the other, the controls start at one x.
export const Aligned: Story = {
  render: () => (
    <>
      <SettingsRow id="a-permission" label="desktop notifications" hint={BODY}><Button variant="accent">turn on</Button></SettingsRow>
      <SettingsRow id="a-mute" label="mute everything"><Toggle id="mute-all" label="mute everything" checked={false} onChange={noop} /></SettingsRow>
      <SettingsRow id="a-default" label="notify me about">
        <Segmented id="a-default-control" label="notify me about" options={NOTIFY} value="everything" onChange={noop} />
      </SettingsRow>
    </>
  ),
};
