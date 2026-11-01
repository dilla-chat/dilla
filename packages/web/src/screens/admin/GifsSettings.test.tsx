import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { Command } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient, refusal } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { en } from '../../strings/index.ts';
import { GifsSettings } from './GifsSettings.tsx';

const SERVER = 'a1'.repeat(16);
const KEY = 'abcdEFGH12345678';
const CONFIGURED = en['gif.settings.maskedLabel'];

function setup(initial: { on?: boolean; rating?: string } = {}, over: (c: Command, state: { on: boolean; rating: string }) => Promise<unknown> | undefined = () => undefined) {
  const state = { on: initial.on ?? false, rating: initial.rating ?? 'pg-13' };
  const fake = new FakeClient();
  fake.handler = (c: Command): Promise<unknown> => {
    const special = over(c, state);
    if (special !== undefined) return special;
    switch (c.m) {
      case 'gifStatus': return Promise.resolve({ on: state.on, rating: state.rating });
      case 'gifSetKey': state.on = true; return Promise.resolve(null);
      case 'gifClearKey': state.on = false; return Promise.resolve(null);
      case 'gifSetRating': state.rating = c.rating; return Promise.resolve(null);
      default: return Promise.resolve(null);
    }
  };
  const user = userEvent.setup();
  const announce = vi.fn();
  const view = render(<div className="d-root"><CoreProvider client={fake}><GifsSettings communityId={SERVER} announce={announce} /></CoreProvider></div>);
  return { fake, user, view, state, announce };
}

const field = () => screen.getByLabelText(en['gif.settings.key']);

describe('the GIFs row of server settings (E8)', () => {
  it('reads the server first, and with no key offers a password field, save, and a link to Giphy’s developer page', async () => {
    const { fake } = setup();
    await waitFor(() => expect(screen.getByLabelText(en['gif.settings.key'])).toBeInTheDocument());
    expect(fake.callsOf('gifStatus')).toEqual([{ m: 'gifStatus', communityId: SERVER }]);
    expect(screen.getByRole('group', { name: en['gif.settings.title'] })).toBeInTheDocument();
    expect(field()).toHaveAttribute('type', 'password');
    expect(field()).toHaveAttribute('autocomplete', 'off');
    expect(field()).toHaveValue('');
    expect(screen.getByRole('button', { name: en['gif.settings.save'] })).toBeInTheDocument();
    const link = screen.getByRole('link', { name: en['gif.settings.getKey'] });
    expect(link).toHaveAttribute('href', 'https://developers.giphy.com/');
    expect(link).toHaveAttribute('rel', expect.stringContaining('noopener'));
    expect(link).toHaveAttribute('target', '_blank');
    expect(screen.getByText(en['gif.settings.hint'])).toBeInTheDocument();
  });

  it('saves a typed key, clears the field, and shows its configured state with replace and remove: the key is never shown again', async () => {
    const { fake, user, announce } = setup();
    await user.type(await screen.findByLabelText(en['gif.settings.key']), KEY);
    await user.click(screen.getByRole('button', { name: en['gif.settings.save'] }));
    await waitFor(() => expect(fake.callsOf('gifSetKey')).toEqual([{ m: 'gifSetKey', communityId: SERVER, key: KEY }]));
    expect(await screen.findByText(CONFIGURED)).toBeVisible();
    expect(screen.getByRole('button', { name: en['gif.settings.replace'] })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: en['gif.settings.remove'] })).toBeInTheDocument();
    expect(screen.queryByLabelText(en['gif.settings.key'])).toBeNull();
    expect(announce).toHaveBeenCalledWith(en['gif.settings.saved']);
    expect(document.querySelector('[role="status"]')).toBeNull();
    expect(document.body.innerHTML).not.toContain(KEY);
    expect(Array.from(document.querySelectorAll('input')).every(i => !i.value.includes(KEY))).toBe(true);
    // Focus lands on the next control rather than disappearing with the field.
    expect(screen.getByRole('button', { name: en['gif.settings.replace'] })).toHaveFocus();
  });

  it('opens on the configured state when a key is already set, and the key never comes back from the instance', async () => {
    const { fake } = setup({ on: true });
    expect(await screen.findByText(CONFIGURED)).toBeVisible();
    expect(screen.getByText(en['gif.settings.maskedLabel'])).toBeInTheDocument();
    expect(screen.queryByLabelText(en['gif.settings.key'])).toBeNull();
    expect(fake.callsOf('gifSetKey')).toEqual([]);
  });

  it('replaces a key with an empty field, and cancel restores its configured state without a call', async () => {
    const { fake, user } = setup({ on: true });
    await user.click(await screen.findByRole('button', { name: en['gif.settings.replace'] }));
    expect(field()).toHaveValue('');
    expect(field()).toHaveFocus();
    await user.click(screen.getByRole('button', { name: en['gif.settings.cancel'] }));
    expect(await screen.findByText(CONFIGURED)).toBeVisible();
    expect(fake.callsOf('gifSetKey')).toEqual([]);
    await user.click(screen.getByRole('button', { name: en['gif.settings.replace'] }));
    await user.type(field(), 'newKEY123456');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(fake.callsOf('gifSetKey')).toEqual([{ m: 'gifSetKey', communityId: SERVER, key: 'newKEY123456' }]));
  });

  it('Escape in the replace field cancels it', async () => {
    const { user } = setup({ on: true });
    await user.click(await screen.findByRole('button', { name: en['gif.settings.replace'] }));
    await user.keyboard('{Escape}');
    expect(await screen.findByText(CONFIGURED)).toBeVisible();
  });

  it('removes the key and goes back to the field with focus on it', async () => {
    const { fake, user, announce } = setup({ on: true });
    await user.click(await screen.findByRole('button', { name: en['gif.settings.remove'] }));
    await waitFor(() => expect(fake.callsOf('gifClearKey')).toEqual([{ m: 'gifClearKey', communityId: SERVER }]));
    expect(await screen.findByLabelText(en['gif.settings.key'])).toHaveFocus();
    expect(announce).toHaveBeenCalledWith(en['gif.settings.removed']);
  });

  it('refuses a key that is not 8 to 64 letters and digits in the field, without a call', async () => {
    const { fake, user } = setup();
    for (const bad of ['short', 'has space 1234', 'abcdefgh&x=1']) {
      await user.clear(await screen.findByLabelText(en['gif.settings.key']));
      await user.type(field(), bad);
      await user.click(screen.getByRole('button', { name: en['gif.settings.save'] }));
      expect(await screen.findByText(en['gif.settings.invalid'])).toBeInTheDocument();
    }
    expect(fake.callsOf('gifSetKey')).toEqual([]);
    expect(field()).toHaveAttribute('aria-invalid', 'true');
  });

  it('a refusal says only that, never the key', async () => {
    const { user } = setup({}, c => c.m === 'gifSetKey' ? Promise.reject(refusal({ code: 'E_FORBIDDEN', status: 403 })) : undefined);
    await user.type(await screen.findByLabelText(en['gif.settings.key']), KEY);
    await user.click(screen.getByRole('button', { name: en['gif.settings.save'] }));
    expect(await screen.findByText(en['gif.settings.forbidden'])).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(KEY);
  });

  it('another failure shows its code, still not the key', async () => {
    const { user } = setup({}, c => c.m === 'gifSetKey' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : undefined);
    await user.type(await screen.findByLabelText(en['gif.settings.key']), KEY);
    await user.click(screen.getByRole('button', { name: en['gif.settings.save'] }));
    expect(await screen.findByText('could not save (E_UNAVAILABLE)')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(KEY);
  });

  it('sets the rating at once from a select, defaulting to pg-13', async () => {
    const { fake, user, announce } = setup();
    const select = await screen.findByLabelText(en['gif.settings.rating']);
    expect(select).toHaveValue('pg-13');
    expect(Array.from(select.querySelectorAll('option')).map(o => [o.value, o.textContent])).toEqual([
      ['g', en['gif.rating.g']], ['pg', en['gif.rating.pg']], ['pg-13', en['gif.rating.pg-13']], ['r', en['gif.rating.r']]]);
    await user.selectOptions(select, 'g');
    await waitFor(() => expect(fake.callsOf('gifSetRating')).toEqual([{ m: 'gifSetRating', communityId: SERVER, rating: 'g' }]));
    await waitFor(() => expect(announce).toHaveBeenCalledWith(en['gif.settings.ratingSaved']));
  });

  it('shows the stored rating', async () => {
    setup({ rating: 'pg' });
    await waitFor(() => expect(screen.getByLabelText(en['gif.settings.rating'])).toHaveValue('pg'));
  });

  it('says so when the setting cannot be read', async () => {
    setup({}, c => c.m === 'gifStatus' ? Promise.reject(refusal({ code: 'E_UNAVAILABLE', status: 503 })) : undefined);
    expect(await screen.findByText(en['gif.settings.loadError'])).toBeInTheDocument();
  });

  it('has no serious axe violations, with and without a key', async () => {
    const a = setup();
    await screen.findByLabelText(en['gif.settings.key']);
    await expectNoAxeViolations(a.view.container);
    a.view.unmount();
    const b = setup({ on: true });
    await screen.findByText(CONFIGURED);
    await expectNoAxeViolations(b.view.container);
  });
});
