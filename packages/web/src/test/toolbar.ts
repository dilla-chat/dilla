// Task 7 (Signal §5.5, Ruling U): a message toolbar is icon buttons (react, reply, thread) and a "more" menu (edit, pin,
// delete, remove for everyone). These read and press its actions by name, wherever they sit.
import { fireEvent, within } from '@testing-library/react';

const moreOf = (toolbar: HTMLElement): HTMLElement | undefined =>
  within(toolbar).queryAllByRole('button').find(b => b.getAttribute('aria-haspopup') === 'menu');

/** The menu's items by text, opening it (and closing it again unless `keepOpen`). */
function menuItems(toolbar: HTMLElement, keepOpen = false): HTMLElement[] {
  const more = moreOf(toolbar);
  if (more === undefined) return [];
  if (more.getAttribute('aria-expanded') !== 'true') fireEvent.click(more);
  const items = within(toolbar).queryAllByRole('menuitem');
  if (!keepOpen) fireEvent.click(more);
  return items;
}

/** The toolbar's action names in order: its icon buttons, then the "more" menu's items. */
export function toolbarLabels(toolbar: HTMLElement): string[] {
  const more = moreOf(toolbar);
  const direct = within(toolbar).getAllByRole('button').filter(b => b !== more).map(b => b.getAttribute('aria-label') ?? b.textContent ?? '');
  return [...direct, ...menuItems(toolbar).map(m => m.textContent ?? '')];
}

/** The control for an action, or null: an icon button, else the menu's item (the menu is left open, so a click picks it). */
export function queryToolbarButton(toolbar: HTMLElement, name: string): HTMLElement | null {
  const direct = within(toolbar).queryAllByRole('button').find(b => b !== moreOf(toolbar) && b.getAttribute('aria-label') === name);
  if (direct !== undefined) return direct;
  const item = menuItems(toolbar, true).find(m => m.textContent === name) ?? null;
  if (item === null && moreOf(toolbar)?.getAttribute('aria-expanded') === 'true') fireEvent.click(moreOf(toolbar)!);
  return item;
}

export function toolbarButton(toolbar: HTMLElement, name: string): HTMLElement {
  const found = queryToolbarButton(toolbar, name);
  if (found === null) throw new Error(`no toolbar action "${name}" (has ${toolbarLabels(toolbar).join(', ')})`);
  return found;
}
