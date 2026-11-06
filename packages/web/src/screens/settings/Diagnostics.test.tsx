import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { act } from '@testing-library/react';
import { it, expect, vi } from 'vitest';
import type { DeviceSummary } from '@dilla/client-core';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { Diagnostics } from './Diagnostics.tsx';

const dev = (id: string, revokedAt: number | null): DeviceSummary =>
  ({ id, tier: 1, signerTier: 1, lastSeen: 1, revokedAt, listed: true, own: false, label: null });
const withCore = (fake: FakeClient, ui: React.JSX.Element) => <CoreProvider client={fake}>{ui}</CoreProvider>;

it('copies only stable codes and counts listed devices', async () => {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- the copied value is asserted below
  const writeText = vi.fn(async (_text: string) => {});
  const data = { connection: 'online' as const, generation: '8', instance: 'home', devices: 2, errors: [{ code: 'E_CORE_INPUT', at: 1 }, { code: 'E_NOT_FOUND', at: 2 }],
    keyPackages: { count: 32, lastAttempt: null, lastError: null } };
  const fake = new FakeClient();
  fake.set('devices', [dev('a1', null), dev('b2', null), dev('c3', 5)]);
  const view = render(withCore(fake, <Diagnostics data={data} instanceName="dilla.test" writeText={writeText} />));
  expect(fake.callsOf('refreshDevices')).toEqual([{ m: 'refreshDevices' }]);
  // W-05: the field label is "devices" and the count its value; the instance is its name, not the hex id.
  expect(Array.from(view.container.querySelectorAll('.d-settings-row__label'), l => l.textContent).slice(0, 5)).toEqual(['Connection', 'Generation', 'Instance', 'Devices', 'Key packages']);
  expect(Array.from(view.container.querySelectorAll('[data-fact]'), d => d.textContent)).toEqual(['online', '8', 'dilla.test', '2', '32']);
  // I1: five fact rows and the copy row.
  expect(view.container.querySelectorAll('.d-settings-row')).toHaveLength(6);
  await userEvent.setup().click(screen.getByRole('button', { name: 'Copy codes' }));
  expect(writeText).toHaveBeenCalledExactlyOnceWith('E_CORE_INPUT\nE_NOT_FOUND');
  expect(document.body).not.toHaveTextContent('detail');
});

it('says when no error is recorded', () => {
  render(withCore(new FakeClient(), <Diagnostics data={{ connection: 'online', generation: '8', instance: 'ab'.repeat(16), devices: 1, errors: [], keyPackages: { count: null, lastAttempt: null, lastError: null } }} instanceName={null} writeText={vi.fn(async () => {})} />));
  expect(screen.getByText('No errors recorded')).toBeInTheDocument();
  expect(document.body).not.toHaveTextContent('ab'.repeat(16));
});

it('shows the devices as unknown until the list loads, then counts the unrevoked ones', () => {
  const fake = new FakeClient();
  const view = render(withCore(fake, <Diagnostics data={{ connection: 'online', generation: '8', instance: 'home', devices: 0, errors: [], keyPackages: { count: null, lastAttempt: null, lastError: null } }} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  const value = () => Array.from(view.container.querySelectorAll('[data-fact]'), d => d.textContent)[3];
  expect(value()).toBe('–');
  act(() => fake.set('devices', [dev('a1', null), dev('c3', 5)]));
  expect(value()).toBe('1');
});

it('shows the key package count, the last attempt and the last error code', () => {
  const at = Date.UTC(2026, 10, 5, 12, 3, 21);
  const data = { connection: 'online' as const, generation: '8', instance: 'home', devices: 1, errors: [],
    keyPackages: { count: 0, lastAttempt: at, lastError: 'E_FORBIDDEN' } };
  const view = render(withCore(new FakeClient(), <Diagnostics data={data} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  const row = view.container.querySelector('[data-fact="keyPackages"]')!;
  expect(row.textContent).toMatch(/^0 · \d\d:\d\d:\d\d.* · E_FORBIDDEN$/);
  expect(row.querySelector('time')).toHaveAttribute('datetime', new Date(at).toISOString());
  expect(row.querySelector('code')).toHaveTextContent('E_FORBIDDEN');
});

it('shows an unknown key package count as a dash and nothing else', () => {
  const view = render(withCore(new FakeClient(), <Diagnostics data={{ connection: 'online', generation: '8', instance: 'home', devices: 1, errors: [],
    keyPackages: { count: null, lastAttempt: null, lastError: null } }} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  expect(view.container.querySelector('[data-fact="keyPackages"]')).toHaveTextContent(/^–$/);
});

it('Diagnostics lists the five slowest requests', () => {
  const slowRoutes = [
    { pattern: 'POST /v1/groups/{id}/message', p50: 120, p95: 480, count: 40 },
    { pattern: 'GET /v1/communities', p50: 90, p95: 300, count: 12 },
    { pattern: 'PUT /v1/communities/{id}/afk/away', p50: 60, p95: 200, count: 3 },
    { pattern: 'GET /v1/accounts/me', p50: 40, p95: 120, count: 9 },
    { pattern: 'POST /v1/groups/{id}/cursor', p50: 30, p95: 80, count: 30 },
    { pattern: 'GET /v1/welcomes', p50: 10, p95: 20, count: 2 },
  ];
  const view = render(withCore(new FakeClient(), <Diagnostics data={{ connection: 'online', generation: '8', instance: 'home', devices: 1, errors: [],
    keyPackages: { count: null, lastAttempt: null, lastError: null }, slowRoutes }} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  const table = screen.getByRole('table', { name: 'Slowest requests' });
  expect(Array.from(table.querySelectorAll('thead th'), (th) => th.textContent)).toEqual(['Request', 'p50', 'p95', 'Count']);
  const rows = Array.from(table.querySelectorAll('tbody tr'), (tr) => Array.from(tr.children, (cell) => cell.textContent));
  expect(rows).toEqual([
    ['POST /v1/groups/{id}/message', '120 ms', '480 ms', '40'],
    ['GET /v1/communities', '90 ms', '300 ms', '12'],
    ['PUT /v1/communities/{id}/afk/away', '60 ms', '200 ms', '3'],
    ['GET /v1/accounts/me', '40 ms', '120 ms', '9'],
    ['POST /v1/groups/{id}/cursor', '30 ms', '80 ms', '30'],
  ]);
  expect(view.container).not.toHaveTextContent('/v1/welcomes');
});

it('shows no slowest-requests table before any request was timed', () => {
  render(withCore(new FakeClient(), <Diagnostics data={{ connection: 'online', generation: '8', instance: 'home', devices: 1, errors: [],
    keyPackages: { count: null, lastAttempt: null, lastError: null }, slowRoutes: [] }} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  expect(screen.queryByRole('table')).toBeNull();
});
