import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { FakeIdb } from '../../test/fake-idb.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { readTheme, writeTheme } from '../../prefs.ts';
import { applyPreference } from '../../theme.ts';
import { Appearance } from './Appearance.tsx';

afterEach(() => { vi.unstubAllGlobals(); applyPreference(window, 'signal'); });

function setup(stored: 'system' | 'signal' | 'light' | 'high-contrast' | null) {
  const idb = new FakeIdb();
  vi.stubGlobal('indexedDB', idb.factory);
  vi.stubGlobal('matchMedia', (q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {} }));
  const user = userEvent.setup();
  return { idb, user, ready: stored === null ? Promise.resolve() : writeTheme(stored, idb.factory) };
}
const theme = () => screen.getByRole('radiogroup', { name: 'Theme' });

describe('Appearance', () => {
  it('writes density and reduced motion preferences immediately through settings storage', async () => {
    const { user } = setup('system');
    const fake = new FakeClient();
    fake.set('settings', {});
    render(<CoreProvider client={fake}><Appearance /></CoreProvider>);
    await user.click(screen.getByRole('radio', { name: 'Compact' }));
    await user.click(screen.getByRole('radio', { name: 'On' }));
    expect(fake.callsOf('setSetting')).toEqual([
      { m: 'setSetting', key: 'density', value: 'compact' },
      { m: 'setSetting', key: 'reduceMotion', value: 'on' },
    ]);
  });
  it('names the theme control once (walk 23)', async () => {
    const { ready } = setup('light');
    await ready;
    render(<CoreProvider client={new FakeClient()}><Appearance /></CoreProvider>);
    expect(screen.queryAllByRole('group', { name: 'Theme' })).toHaveLength(0);
    expect(screen.getAllByRole('radiogroup', { name: 'Theme' })).toHaveLength(1);
  });
  it('shows the stored preference and applies and stores a new one', async () => {
    const { user, ready } = setup('light');
    await ready;
    const view = render(<CoreProvider client={new FakeClient()}><Appearance /></CoreProvider>);
    expect(screen.getByRole('heading', { level: 2, name: 'Appearance' })).toBeInTheDocument();
    expect(within(theme()).getAllByRole('radio')).toHaveLength(4);
    for (const name of ['System', 'Signal', 'Light', 'High contrast']) expect(within(theme()).getByRole('radio', { name })).toBeInTheDocument();
    await waitFor(() => expect(within(theme()).getByRole('radio', { name: 'Light' })).toHaveAttribute('aria-checked', 'true'));
    await user.click(within(theme()).getByRole('radio', { name: 'High contrast' }));
    expect(document.documentElement.dataset.theme).toBe('high-contrast');
    await waitFor(async () => expect(await readTheme()).toBe('high-contrast'));
    await expectNoAxeViolations(view.container);
  });
  it('starts at system when nothing is stored', async () => {
    setup(null);
    render(<CoreProvider client={new FakeClient()}><Appearance /></CoreProvider>);
    await waitFor(() => expect(within(theme()).getByRole('radio', { name: 'System' })).toHaveAttribute('aria-checked', 'true'));
  });
});

it('shows theme, density and reduced motion as settings rows', async () => {
  const { ready } = setup('light');
  await ready;
  const view = render(<CoreProvider client={new FakeClient()}><Appearance /></CoreProvider>);
  expect(view.container.querySelectorAll('.d-settings-row')).toHaveLength(3);
});
