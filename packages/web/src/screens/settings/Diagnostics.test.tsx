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
  const data = { connection: 'online' as const, generation: '8', instance: 'home', devices: 2, errors: [{ code: 'E_CORE_INPUT', at: 1 }, { code: 'E_NOT_FOUND', at: 2 }] };
  const fake = new FakeClient();
  fake.set('devices', [dev('a1', null), dev('b2', null), dev('c3', 5)]);
  const view = render(withCore(fake, <Diagnostics data={data} instanceName="dilla.test" writeText={writeText} />));
  expect(fake.callsOf('refreshDevices')).toEqual([{ m: 'refreshDevices' }]);
  // W-05: the field label is "devices" and the count its value; the instance is its name, not the hex id.
  expect(Array.from(view.container.querySelectorAll('.d-settings-row__label'), l => l.textContent).slice(0, 4)).toEqual(['Connection', 'Generation', 'Instance', 'Devices']);
  expect(Array.from(view.container.querySelectorAll('[data-fact]'), d => d.textContent)).toEqual(['online', '8', 'dilla.test', '2']);
  // I1: four fact rows and the copy row.
  expect(view.container.querySelectorAll('.d-settings-row')).toHaveLength(5);
  await userEvent.setup().click(screen.getByRole('button', { name: 'Copy codes' }));
  expect(writeText).toHaveBeenCalledExactlyOnceWith('E_CORE_INPUT\nE_NOT_FOUND');
  expect(document.body).not.toHaveTextContent('detail');
});

it('says when no error is recorded', () => {
  render(withCore(new FakeClient(), <Diagnostics data={{ connection: 'online', generation: '8', instance: 'ab'.repeat(16), devices: 1, errors: [] }} instanceName={null} writeText={vi.fn(async () => {})} />));
  expect(screen.getByText('No errors recorded')).toBeInTheDocument();
  expect(document.body).not.toHaveTextContent('ab'.repeat(16));
});

it('shows the devices as unknown until the list loads, then counts the unrevoked ones', () => {
  const fake = new FakeClient();
  const view = render(withCore(fake, <Diagnostics data={{ connection: 'online', generation: '8', instance: 'home', devices: 0, errors: [] }} instanceName="dilla.test" writeText={vi.fn(async () => {})} />));
  const value = () => Array.from(view.container.querySelectorAll('[data-fact]'), d => d.textContent)[3];
  expect(value()).toBe('–');
  act(() => fake.set('devices', [dev('a1', null), dev('c3', 5)]));
  expect(value()).toBe('1');
});
