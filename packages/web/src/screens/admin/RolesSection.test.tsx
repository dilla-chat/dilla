import { describe, expect, it } from 'vitest';
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { refusal } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { ADMINS, CREW, EVERY, MODS, OWNER_ACCESS, ROLES, SERVER, harness, role, type HarnessOptions } from '../../test/roles-harness.tsx';
import { en } from '../../strings/index.ts';
import { RolesSection, openRoleOnNextVisit } from './RolesSection.tsx';

function setup(options: HarnessOptions = {}) {
  const h = harness(options);
  const view = h.mount(<RolesSection communityId={SERVER} serverName="Björn" announce={h.announce} />);
  return { ...h, view };
}
const list = () => within(screen.getByRole('list', { name: en['serverSettings.roles.list'] }));
const roleButton = (name: RegExp | string) => list().getByRole('button', { name });
const toggle = (label: string) => screen.getByRole('switch', { name: label });
const MUSIC = 1 << 25;

describe('RolesSection (Signal task 10c: the inline editor)', () => {
  it('lists @everyone first, then the roles from the highest down, each with its member count', () => {
    setup();
    expect(list().getAllByRole('button').map(b => b.getAttribute('aria-label') ?? b.textContent))
      .toEqual(['@everyone, 5 members', 'admins, 1 member', 'mods, 2 members', 'crew, 1 member', en['serverSettings.roles.new']]);
  });
  it('opens on the highest role and shows its name, colours and the prototype\'s four permission groups', () => {
    setup();
    expect(roleButton('admins, 1 member')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByRole('textbox', { name: en['roles.editor.name'] })).toHaveValue('admins');
    expect(screen.getByRole('radiogroup', { name: en['roles.editor.color'] })).toBeInTheDocument();
    // Ruling AS: the prototype's General, Moderation, Voice and Music; Control music is under Music.
    for (const group of ['perm.group.general', 'perm.group.moderation', 'perm.group.voice', 'perm.group.music'] as const) {
      expect(screen.getByRole('heading', { name: en[group] })).toBeInTheDocument();
    }
    expect(screen.queryByRole('heading', { name: en['perm.group.members'] })).toBeNull();
    expect(toggle(en['perm.music'])).toHaveAttribute('aria-checked', 'false');
  });
  it('@everyone has a fixed name and no colours', async () => {
    const { user } = setup();
    await user.click(roleButton('@everyone, 5 members'));
    expect(screen.getByRole('textbox', { name: en['roles.editor.name'] })).toBeDisabled();
    expect(screen.queryByRole('radiogroup', { name: en['roles.editor.color'] })).toBeNull();
    expect(toggle(en['perm.view'])).toHaveAttribute('aria-checked', 'true');
  });
  it('a toggle saves the role with the bit at once and announces it', async () => {
    const { user, fake, announce } = setup();
    await user.click(roleButton('crew, 1 member'));
    await user.click(toggle(en['perm.music']));
    await waitFor(() => expect(fake.callsOf('updateRole')).toEqual([{ m: 'updateRole', communityId: SERVER, roleId: CREW,
      name: null, color: null, hoist: null, mentionable: null, allow: MUSIC }]));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['roles.editor.saved'].replace('{name}', 'crew')));
  });
  it('a colour saves at once; a name saves on blur and refuses an empty one', async () => {
    const { user, fake } = setup();
    await user.click(roleButton('crew, 1 member'));
    await user.click(screen.getByRole('radio', { name: en['roles.color.green'] }));
    await waitFor(() => expect(fake.callsOf('updateRole').at(-1)).toMatchObject({ roleId: CREW, color: 0x7CFF8E, allow: null }));
    const name = screen.getByRole('textbox', { name: en['roles.editor.name'] });
    await user.clear(name);
    fireEvent.blur(name);
    expect(await screen.findByRole('alert')).toHaveTextContent(en['roles.error.empty']);
    await user.type(name, 'crew two');
    fireEvent.blur(name);
    await waitFor(() => expect(fake.callsOf('updateRole').at(-1)).toMatchObject({ roleId: CREW, name: 'crew two', allow: null }));
  });
  it('a refused save says why and puts the toggle back', async () => {
    const { user } = setup({ handler: c => (c.m === 'updateRole' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : Promise.resolve(null)) });
    await user.click(roleButton('crew, 1 member'));
    await user.click(toggle(en['perm.music']));
    expect(await screen.findByRole('alert')).toHaveTextContent(en['roles.error.forbidden']);
    expect(toggle(en['perm.music'])).toHaveAttribute('aria-checked', 'false');
  });
  it('locks a role at or above my top: no toggle, no name change', async () => {
    const { user, fake } = setup({ access: { ...OWNER_ACCESS, owner: false, top: 5 } });
    expect(roleButton(/^admins, 1 members?, above your highest role$/)).toBeInTheDocument();
    await user.click(roleButton(/^mods/));
    expect(screen.getByRole('textbox', { name: en['roles.editor.name'] })).toBeDisabled();
    await user.click(toggle(en['perm.view']));
    expect(fake.callsOf('updateRole')).toHaveLength(0);
  });
  it('a permission I do not hold cannot be given', async () => {
    const { user, fake } = setup({ access: { ...OWNER_ACCESS, owner: false, top: 9 },
      roles: [role(EVERY, '@everyone', 0, { allow: 3, members: 5 }), role(CREW, 'crew', 2, { members: 1 }), role(ADMINS, 'admins', 9, { members: 1 })] });
    await user.click(roleButton('crew, 1 member'));
    expect(screen.getAllByText(en['roles.editor.notHeld']).length).toBeGreaterThan(0);
    await user.click(toggle(en['perm.music']));
    expect(fake.callsOf('updateRole')).toHaveLength(0);
  });
  it('New role makes "new role" with View channels and Send messages and selects it', async () => {
    const NEW = 'e9'.repeat(16);
    const { user, fake } = setup({ handler: (c, f) => {
      if (c.m === 'createRole') { f.set(`roles:${SERVER}`, [...ROLES, role(NEW, 'new role', 1, { color: 0xB0BEC5, allow: 3 })]); return Promise.resolve({ roleId: NEW }); }
      return Promise.resolve(null);
    } });
    await user.click(roleButton(en['serverSettings.roles.new']));
    await waitFor(() => expect(fake.callsOf('createRole')).toEqual([{ m: 'createRole', communityId: SERVER, name: 'new role', color: 0xB0BEC5,
      hoist: false, mentionable: false, allow: 3 }]));
    await waitFor(() => expect(screen.getByRole('textbox', { name: en['roles.editor.name'] })).toHaveValue('new role'));
  });
  it('a new role the list does not carry yet: the editor waits read-only, then selects it when it arrives', async () => {
    const NEW = 'e8'.repeat(16);
    const { user, fake } = setup({ handler: c => c.m === 'createRole' ? Promise.resolve({ roleId: NEW }) : Promise.resolve(null) });
    expect(toggle(en['perm.music'])).not.toHaveAttribute('aria-disabled', 'true');
    await user.click(roleButton(en['serverSettings.roles.new']));
    await waitFor(() => expect(fake.callsOf('createRole')).toHaveLength(1));
    // The fallback role shows, but nothing can be changed on it while the new role is on its way.
    await waitFor(() => expect(toggle(en['perm.music'])).toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(toggle(en['perm.music']));
    expect(fake.callsOf('updateRole')).toHaveLength(0);
    act(() => { fake.set(`roles:${SERVER}`, [...ROLES, role(NEW, 'new role', 1, { color: 0xB0BEC5, allow: 3 })]); });
    await waitFor(() => expect(screen.getByRole('textbox', { name: en['roles.editor.name'] })).toHaveValue('new role'));
    expect(roleButton('new role, 0 members')).toHaveAttribute('aria-current', 'true');
  });
  it('without Manage roles there is no New role', () => {
    setup({ access: { ...OWNER_ACCESS, owner: false, manageRoles: false, top: 0 } });
    expect(list().queryByRole('button', { name: en['serverSettings.roles.new'] })).toBeNull();
  });
  it('moves and deletes from the role menu (Shift+F10)', async () => {
    const { user, fake, announce } = setup({ handler: (c, f) => {
      if (c.m === 'deleteRole') f.set(`roles:${SERVER}`, ROLES.filter(r => r.id !== MODS));
      return Promise.resolve(null);
    } });
    roleButton('mods, 2 members').focus();
    await user.keyboard('{Shift>}{F10}{/Shift}');
    const menu = await screen.findByRole('menu', { name: 'Role mods' });
    expect([...menu.querySelectorAll('[role^="menuitem"]')].map(i => i.textContent)).toEqual([en['serverSettings.roles.menu.up'],
      en['serverSettings.roles.menu.down'], en['serverSettings.roles.menu.hoist'], en['serverSettings.roles.menu.mentionable'], en['serverSettings.roles.menu.delete']]);
    await user.click(within(menu).getByRole('menuitem', { name: en['serverSettings.roles.menu.up'] }));
    await waitFor(() => expect(fake.callsOf('moveRole')).toEqual([{ m: 'moveRole', communityId: SERVER, roleId: MODS, direction: 'up' }]));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['roles.moved'].replace('{name}', 'mods')));
    roleButton('mods, 2 members').focus();
    await user.keyboard('{Shift>}{F10}{/Shift}');
    await user.click(within(await screen.findByRole('menu', { name: 'Role mods' })).getByRole('menuitem', { name: en['serverSettings.roles.menu.delete'] }));
    const confirm = await screen.findByRole('dialog', { name: en['roles.deleteTitle'].replace('{name}', 'mods') });
    await user.click(within(confirm).getByRole('button', { name: en['roles.deleteConfirm'] }));
    await waitFor(() => expect(fake.callsOf('deleteRole')).toEqual([{ m: 'deleteRole', communityId: SERVER, roleId: MODS }]));
  });
  it('@everyone has no role menu', async () => {
    const { user } = setup();
    roleButton('@everyone, 5 members').focus();
    await user.keyboard('{Shift>}{F10}{/Shift}');
    expect(screen.queryByRole('menu')).toBeNull();
  });
  it('the Music page can open it on a given role', () => {
    openRoleOnNextVisit(SERVER, CREW);
    setup();
    expect(roleButton('crew, 1 member')).toHaveAttribute('aria-current', 'true');
  });
  it('shows a loading line while the roles are absent and reads them once itself', async () => {
    const { fake } = setup({ roles: null });
    expect(screen.getByText(en['roles.loading'])).toBeInTheDocument();
    await waitFor(() => expect(fake.callsOf('selectCommunity')).toHaveLength(1));
  });
  it('has no axe violations', async () => {
    const { view } = setup();
    await expectNoAxeViolations(view.container);
  });
});
