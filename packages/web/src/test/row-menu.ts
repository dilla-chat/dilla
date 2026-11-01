import { screen, within } from '@testing-library/react';
import type { UserEvent } from '@testing-library/user-event';
import { t } from '../strings/index.ts';

/**
 * Ruling X (Jonas 2026-10-27, pixel parity): the channel header has no "⋯"; a text channel's pins and edit channel are in its
 * row's context menu. This right-clicks the row in the server's channel list and returns the menu.
 */
export async function openRowMenu(user: UserEvent, channel: string, list: 'channels' | 'dms' = 'channels'): Promise<HTMLElement> {
  const nav = await screen.findByRole('navigation', { name: t(list === 'dms' ? 'shell.dms.label' : 'shell.channels.label') });
  await user.pointer({ keys: '[MouseRight]', target: within(nav).getByRole('button', { name: new RegExp(`^${channel}`) }) });
  return screen.getByRole('menu', { name: t('nav.channel.menuOf', { name: channel }) });
}

/** Opens the row's context menu and picks one item by its visible label. */
export async function rowMenuItem(user: UserEvent, channel: string, label: string, list: 'channels' | 'dms' = 'channels'): Promise<void> {
  const menu = await openRowMenu(user, channel, list);
  await user.click(within(menu).getByRole('menuitem', { name: label }));
}

/** Ruling Z: a DM's call is its row's menu item (`call` or `join call`); this starts it. */
export async function startDmCall(user: UserEvent, dm: string, label = t('call.start')): Promise<void> {
  await rowMenuItem(user, dm, label, 'dms');
}

/** The DM row menu's item labels, the menu closed again (Escape) afterwards. */
export async function dmMenuItems(user: UserEvent, dm: string): Promise<string[]> {
  const menu = await openRowMenu(user, dm, 'dms');
  const labels = within(menu).getAllByRole('menuitem').map(item => item.textContent ?? '');
  await user.keyboard('{Escape}');
  return labels;
}
