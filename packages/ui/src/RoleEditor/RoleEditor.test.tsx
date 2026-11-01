import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { readFileSync } from 'node:fs';
import { RoleEditor, type RoleEditorProps } from './RoleEditor.tsx';

const ROLES = [
  { id: 'everyone', name: '@everyone', color: 0, count: 3, label: '@everyone, 3 members' },
  { id: 'raid', name: 'raid lead', color: 0xE9B6EC, count: 1, label: 'raid lead, 1 member' },
];
const GROUPS = [{ id: 'music', title: 'Music', rows: [{ id: 'music', label: 'Control music (DJ)', hint: 'Play and skip', disabled: false }] }];
const LABELS = { list: 'Roles', newRole: 'New role', name: 'Role name', color: 'Colour', permissions: 'Permissions', allow: 'Allow', inherit: 'Default', deny: 'Deny' };

function setup(over: Partial<RoleEditorProps> = {}) {
  const props: RoleEditorProps = { roles: ROLES, selectedId: 'raid', onSelect: vi.fn(), onNew: vi.fn(), name: 'raid lead', nameReadOnly: false,
    onName: vi.fn(), onNameCommit: vi.fn(), colors: [{ value: 0xE9B6EC, name: 'pink' }, { value: 0x5AA8E6, name: 'blue' }], color: 0xE9B6EC,
    onColor: vi.fn(), groups: GROUPS, values: { music: 'inherit' }, onPermission: vi.fn(), labels: LABELS, ...over };
  render(<RoleEditor {...props} />);
  return props;
}

describe('RoleEditor (Signal task 10c)', () => {
  it('lists the roles with their counts, marks the selected one and selects on click', async () => {
    const props = setup();
    const list = screen.getByRole('list', { name: 'Roles' });
    expect(within(list).getByRole('button', { name: 'raid lead, 1 member' })).toHaveAttribute('aria-current', 'true');
    await userEvent.click(within(list).getByRole('button', { name: '@everyone, 3 members' }));
    expect(props.onSelect).toHaveBeenCalledWith('everyone');
    await userEvent.click(within(list).getByRole('button', { name: 'New role' }));
    expect(props.onNew).toHaveBeenCalled();
  });
  it('reports a toggle as on/off and a colour as its value', async () => {
    const props = setup();
    await userEvent.click(screen.getByRole('switch', { name: 'Control music (DJ)' }));
    expect(props.onPermission).toHaveBeenCalledWith('music', true);
    await userEvent.click(screen.getByRole('radio', { name: 'blue' }));
    expect(props.onColor).toHaveBeenCalledWith(0x5AA8E6);
    expect(screen.queryByRole('radio', { name: /no colour/ })).toBeNull();
  });
  it('commits the name on blur and on Enter, not on each key', async () => {
    const props = setup();
    const field = screen.getByRole('textbox', { name: 'Role name' });
    await userEvent.type(field, 'x');
    expect(props.onName).toHaveBeenCalled();
    expect(props.onNameCommit).not.toHaveBeenCalled();
    await userEvent.keyboard('{Enter}');
    expect(props.onNameCommit).toHaveBeenCalledTimes(1);
    fireEvent.blur(field);
    expect(props.onNameCommit).toHaveBeenCalledTimes(2);
  });
  it('arrow keys move between the rows', async () => {
    setup();
    screen.getByRole('button', { name: '@everyone, 3 members' }).focus();
    await userEvent.keyboard('{ArrowDown}');
    expect(screen.getByRole('button', { name: 'raid lead, 1 member' })).toHaveFocus();
  });
  it('opens a row menu with Shift+F10 only when the row has items', async () => {
    const onSelect = vi.fn();
    setup({ menu: { label: r => `Role ${r.name}`, items: r => (r.id === 'everyone' ? [] : [{ id: 'delete', label: 'Delete role' }]), onSelect } });
    screen.getByRole('button', { name: '@everyone, 3 members' }).focus();
    await userEvent.keyboard('{Shift>}{F10}{/Shift}');
    expect(screen.queryByRole('menu')).toBeNull();
    screen.getByRole('button', { name: 'raid lead, 1 member' }).focus();
    await userEvent.keyboard('{Shift>}{F10}{/Shift}');
    await userEvent.click(within(await screen.findByRole('menu', { name: 'Role raid lead' })).getByRole('menuitem', { name: 'Delete role' }));
    expect(onSelect).toHaveBeenCalledWith('raid', 'delete');
  });
  it('the prototype grid: a 200px list beside the editor, one column at a phone width', () => {
    const css = readFileSync('src/RoleEditor/RoleEditor.css', 'utf8');
    expect(css).toMatch(/\.d-role-editor__grid \{[^}]*grid-template-columns: 200px minmax\(0, 1fr\); gap: 18px/);
    expect(css).toMatch(/@container \(max-width: 34rem\) \{ \.d-role-editor__grid \{ grid-template-columns: minmax\(0, 1fr\); \} \}/);
  });
});
