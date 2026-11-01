import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { DeviceRow, type DeviceRowProps } from './DeviceRow.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-02: devices.tier.web, devices.tier.native, devices.thisBrowser, devices.unlisted, devices.revoked,
// devices.lastSeen, devices.revoke. Row names are the first eight hex characters of a device id (flow 03).
function list(p: DeviceRowProps) {
  return <ul role="list"><DeviceRow {...p} /></ul>;
}

describe('DeviceRow', () => {
  it('is one list item holding the name, the tier tag, the state and the last-seen time as separate text', () => {
    render(list({ name: '0a1b2c3d', tier: 'web', tierLabel: 'browser', state: 'not yet in the device list', lastSeen: 'last seen 20:40' }));
    const item = screen.getByRole('listitem');
    expect(item).toHaveClass('d-device-row');
    expect(item).toHaveAttribute('data-tier', 'web');
    // Fix round 2: the name is always isolated (<bdi>); the rest are spans.
    expect(within(item).getByText('0a1b2c3d').tagName).toBe('BDI');
    for (const text of ['browser', 'not yet in the device list', 'last seen 20:40']) {
      expect(within(item).getByText(text).tagName).toBe('SPAN');
    }
    expect(item).not.toHaveAttribute('data-own');
    expect(within(item).queryByRole('button')).toBeNull();
  });

  it('marks this browser with its tag and offers no action without one', () => {
    render(list({ name: '3f9a2c1d', tier: 'web', tierLabel: 'browser', own: true, ownLabel: 'this browser', lastSeen: 'last seen 21:04' }));
    const item = screen.getByRole('listitem');
    expect(item).toHaveAttribute('data-own', 'true');
    expect(within(item).getByText('this browser').tagName).toBe('SPAN');
    expect(within(item).queryByRole('button')).toBeNull();
  });

  it('names its action by the label, describes it by the device name and calls it', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    render(list({ name: 'b2d4e6f8', tier: 'native', tierLabel: 'app', lastSeen: 'last seen 21:02', action: { label: 'remove', onAction } }));
    const button = screen.getByRole('button', { name: 'remove' });
    expect(button).toHaveAccessibleDescription('b2d4e6f8');
    expect(button).toHaveAttribute('data-variant', 'default');
    expect(screen.getByRole('listitem')).toHaveAttribute('data-tier', 'native');
    await user.click(button);
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it('draws a danger action in the danger variant', () => {
    render(list({ name: '7c01e5aa', tier: 'web', tierLabel: 'browser', action: { label: 'remove', onAction: () => {}, danger: true } }));
    expect(screen.getByRole('button', { name: 'remove' })).toHaveAttribute('data-variant', 'danger');
  });

  it('offers a second action beside the first, also described by the device name', async () => {
    const user = userEvent.setup();
    const onRename = vi.fn();
    const onRemove = vi.fn();
    render(list({ name: 'work laptop', tier: 'web', tierLabel: 'browser', action: { label: 'remove', onAction: onRemove },
      extraAction: { label: 'rename', onAction: onRename } }));
    const rename = screen.getByRole('button', { name: 'rename' });
    expect(rename).toHaveAccessibleDescription('work laptop');
    expect(rename).toHaveAttribute('data-variant', 'default');
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['rename', 'remove']);
    await user.click(rename);
    expect(onRename).toHaveBeenCalledTimes(1);
    expect(onRemove).not.toHaveBeenCalled();
  });

  it('offers the extra action alone, on a row with no remove (this browser)', () => {
    render(list({ name: '3f9a2c1d', tier: 'web', tierLabel: 'browser', own: true, ownLabel: 'this browser', extraAction: { label: 'rename', onAction: () => {} } }));
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['rename']);
  });

  it('keeps the id beside a name, isolates the name from the text around it, and describes the actions by both', () => {
    render(list({ name: 'work laptop', idHint: '7c01e5aa', tier: 'web', tierLabel: 'browser', extraAction: { label: 'rename', onAction: () => {} },
      action: { label: 'remove', onAction: () => {} } }));
    const item = screen.getByRole('listitem');
    const name = within(item).getByText('work laptop');
    expect(name.tagName).toBe('BDI');
    expect(within(item).getByText('7c01e5aa').tagName).toBe('SPAN');
    for (const b of within(item).getAllByRole('button')) expect(b).toHaveAccessibleDescription('work laptop 7c01e5aa');
  });

  it('shows no id hint when the name is the id', () => {
    render(list({ name: '7c01e5aa', tier: 'web', tierLabel: 'browser' }));
    expect(screen.getAllByText('7c01e5aa')).toHaveLength(1);
  });

  it('has no serious axe violations', async () => {
    const { container } = render(
      <div className="d-root">
        <ul role="list">
          <DeviceRow name="3f9a2c1d" tier="web" tierLabel="browser" own ownLabel="this browser" lastSeen="last seen 21:04" />
          <DeviceRow name="99ee0f11" tier="web" tierLabel="browser" state="removed" lastSeen="last seen 1 Oct 2026" />
          <DeviceRow name="b2d4e6f8" tier="native" tierLabel="app" lastSeen="last seen 21:02" action={{ label: 'remove', onAction: () => {} }} />
        </ul>
      </div>,
    );
    await expectNoAxeViolations(container);
  });
});

// Signal task 10 fix round 2 (Ruling X): a row's menu opens from Shift+F10 on the focused row, and from a right-click.
describe('DeviceRow menu', () => {
  it('opens from Shift+F10 and a right-click, picks an item and gives focus back to the row', async () => {
    const onSelect = vi.fn();
    render(<ul><DeviceRow name="b2d4e6f8" tier="web" menu={{ label: 'b2d4e6f8 menu', items: [{ id: 'rename', label: 'Rename' }], onSelect }} /></ul>);
    const row = screen.getByRole('listitem');
    expect(row).toHaveAttribute('tabindex', '0');
    row.focus();
    fireEvent.keyDown(row, { key: 'F10', shiftKey: true });
    await userEvent.setup().click(screen.getByRole('menuitem', { name: 'Rename' }));
    expect(onSelect).toHaveBeenCalledWith('rename');
    expect(row).toHaveFocus();
  });
  it('opens from a right-click', () => {
    render(<ul><DeviceRow name="b2d4e6f8" tier="web" menu={{ label: 'b2d4e6f8 menu', items: [{ id: 'rename', label: 'Rename' }], onSelect: vi.fn() }} /></ul>);
    fireEvent.contextMenu(screen.getByRole('listitem'), { clientX: 5, clientY: 5 });
    expect(screen.getByRole('menu', { name: 'b2d4e6f8 menu' })).toBeInTheDocument();
  });
  it('names Shift+F10 on the row, and a right-click on its own button opens no row menu', () => {
    render(<ul><DeviceRow name="b2d4e6f8" tier="web" action={{ label: 'Remove', onAction: vi.fn(), danger: true }}
      menu={{ label: 'b2d4e6f8 menu', items: [{ id: 'rename', label: 'Rename' }], onSelect: vi.fn() }} /></ul>);
    expect(screen.getByRole('listitem')).toHaveAttribute('aria-keyshortcuts', 'Shift+F10');
    fireEvent.contextMenu(screen.getByRole('button', { name: 'Remove' }), { clientX: 5, clientY: 5 });
    expect(screen.queryByRole('menu')).toBeNull();
  });
  it('has no menu and no tab stop without one', () => {
    render(<ul><DeviceRow name="b2d4e6f8" tier="web" /></ul>);
    expect(screen.getByRole('listitem')).not.toHaveAttribute('tabindex');
  });
});
