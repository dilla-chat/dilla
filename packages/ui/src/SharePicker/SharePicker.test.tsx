import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { SharePicker, type SharePickerProps, type SharePickerValue } from './SharePicker.tsx';
import { CallControls } from '../CallControls/CallControls.tsx';
import { BAR_CONTROL_LABELS, PICKER_DEFAULT, pickerProps } from '../CallStage/stage.fixture.tsx';

const base = (over: Partial<SharePickerProps> = {}): SharePickerProps => {
  const { open: _o, onOpenChange: _c, quality: _q, ...p } = pickerProps(PICKER_DEFAULT);
  return { ...p, sharing: false, onClose: vi.fn(), ...over };
};

describe('SharePicker', () => {
  it('is a non-modal dialog named by its title, with the prototype’s rows and Go live; focus starts on the picked source', () => {
    render(<SharePicker {...base()} />);
    const dialog = screen.getByRole('dialog', { name: 'Share your screen' });
    expect(dialog).toHaveAttribute('aria-modal', 'false');
    expect(screen.getByRole('radiogroup', { name: 'Source' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Resolution' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Frame rate' })).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: 'Also share system audio' })).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByRole('switch')).toHaveAccessibleDescription('Sound from the whole system');
    expect(screen.getByText('≈ 4.5 Mb/s up')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Go live' })).toBeInTheDocument();
    expect(document.activeElement).toBe(screen.getByRole('radio', { name: 'Screen' }));
    expect(screen.getByRole('radio', { name: 'Source' })).toBeInTheDocument(); // the native resolution's word
  });

  it('while sharing it is Stream quality with Apply', () => {
    render(<SharePicker {...base({ sharing: true })} />);
    expect(screen.getByRole('dialog', { name: 'Stream quality' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Apply' })).toBeInTheDocument();
  });

  it('every choice reports the whole value', async () => {
    const onChange = vi.fn();
    render(<SharePicker {...base({ onChange })} />);
    await userEvent.click(screen.getByRole('radio', { name: 'Tab' }));
    expect(onChange).toHaveBeenLastCalledWith({ ...PICKER_DEFAULT, source: 'tab' });
    await userEvent.click(screen.getByRole('radio', { name: '720p' }));
    expect(onChange).toHaveBeenLastCalledWith({ ...PICKER_DEFAULT, resolution: '720p' });
    await userEvent.click(screen.getByRole('radio', { name: '60' }));
    expect(onChange).toHaveBeenLastCalledWith({ ...PICKER_DEFAULT, fps: 60 });
    await userEvent.click(screen.getByRole('switch'));
    expect(onChange).toHaveBeenLastCalledWith({ ...PICKER_DEFAULT, audio: false });
  });

  it('draws no control the browser cannot act on: no source row without the hint, no switch without sound', () => {
    const { rerender } = render(<SharePicker {...base({ sources: [], audio: 'none' })} />);
    expect(screen.queryByRole('radiogroup', { name: 'Source' })).toBeNull();
    expect(screen.queryByText('Display · 2560×1440')).toBeNull();
    expect(screen.queryByText('Also share system audio')).toBeNull();
    rerender(<SharePicker {...base({ audio: 'note', labels: { ...base().labels, audioNote: 'Chrome on Linux can share sound only from a tab' } })} />);
    expect(screen.queryByRole('switch')).toBeNull();
    expect(screen.getByText('Chrome on Linux can share sound only from a tab')).toBeInTheDocument();
  });

  it('the heavy estimate is marked', () => {
    render(<SharePicker {...base({ estimate: { text: '≈ 19.2 Mb/s up · may drop frames on your link', heavy: true } })} />);
    expect(screen.getByText(/may drop frames/)).toHaveAttribute('data-heavy', 'true');
  });

  it('Escape, the × and a press outside close it; a press on the anchor does not', () => {
    const onClose = vi.fn();
    const anchor = document.createElement('button');
    document.body.append(anchor);
    render(<SharePicker {...base({ onClose, anchor: t => anchor.contains(t) })} />);
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Escape' });
    expect(onClose).toHaveBeenLastCalledWith('escape');
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenLastCalledWith('close');
    onClose.mockClear();
    fireEvent.pointerDown(anchor);
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.pointerDown(document.body);
    expect(onClose).toHaveBeenLastCalledWith('outside');
    anchor.remove();
  });
});

function Bar({ sharing = false, onShare = vi.fn(), onSubmit = vi.fn() }: { sharing?: boolean; onShare?: () => void; onSubmit?: () => void }) {
  const [open, setOpen] = useState(false);
  const [v, setV] = useState<SharePickerValue>(PICKER_DEFAULT);
  return (
    <CallControls muted={false} deafened={false} camera={false} sharing={sharing} canCamera canShare busy={false} labels={BAR_CONTROL_LABELS}
      picker={{ ...pickerProps(v, { onChange: setV }), open, onOpenChange: setOpen, onSubmit }}
      onMute={vi.fn()} onDeafen={vi.fn()} onCamera={vi.fn()} onShare={onShare} onLeave={vi.fn()} />
  );
}

describe('CallControls with the share picker', () => {
  it('Share opens the picker (aria-expanded), Go live submits and hands focus back to Share', async () => {
    const onShare = vi.fn();
    const onSubmit = vi.fn();
    render(<Bar onShare={onShare} onSubmit={onSubmit} />);
    const share = screen.getByRole('button', { name: 'Share' });
    expect(share).toHaveAttribute('aria-haspopup', 'dialog');
    expect(share).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(share);
    expect(share).toHaveAttribute('aria-expanded', 'true');
    expect(onShare).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('radio', { name: 'Window' }));
    await userEvent.click(screen.getByRole('button', { name: 'Go live' }));
    expect(onSubmit).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(share);
  });

  it('Share toggles the picker shut again; Escape closes it and focus goes back to Share', async () => {
    render(<Bar />);
    const share = screen.getByRole('button', { name: 'Share' });
    await userEvent.click(share);
    await userEvent.click(share);
    expect(screen.queryByRole('dialog')).toBeNull();
    await userEvent.click(share);
    await userEvent.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(document.activeElement).toBe(share);
  });

  it('while sharing, Stop sharing stops at once and the Stream quality chevron opens the picker with Apply', async () => {
    const onShare = vi.fn();
    render(<Bar sharing onShare={onShare} />);
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(onShare).toHaveBeenCalledTimes(1);
    const quality = screen.getByRole('button', { name: 'Stream quality' });
    expect(quality).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(quality);
    expect(screen.getByRole('dialog', { name: 'Stream quality' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Apply' })).toBeInTheDocument();
  });

  it('without a picker there is no chevron and Share acts at once (the DM call box)', async () => {
    const onShare = vi.fn();
    render(<CallControls muted={false} deafened={false} camera={false} sharing canCamera canShare busy={false} labels={BAR_CONTROL_LABELS}
      onMute={vi.fn()} onDeafen={vi.fn()} onCamera={vi.fn()} onShare={onShare} onLeave={vi.fn()} />);
    expect(screen.queryByRole('button', { name: 'Stream quality' })).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(onShare).toHaveBeenCalledTimes(1);
  });
});
