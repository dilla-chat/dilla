import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { OWNER_ACCESS, SERVER } from '../../test/roles-harness.tsx';
import { OverviewSection } from './OverviewSection.tsx';

describe('Overview server name permissions', () => {
  it.each([true, false])('shows the Server name row only with Manage server (%s)', async manageCommunity => {
    const fake = new FakeClient();
    fake.set(`permissions:${SERVER}`, { ...OWNER_ACCESS, owner: false, manageCommunity });
    render(<CoreProvider client={fake}>
      <OverviewSection communityId={SERVER} serverName="Midgard Crew" announce={vi.fn()} />
    </CoreProvider>);
    if (manageCommunity) {
      expect(screen.getByRole('textbox', { name: 'Server name' })).toHaveValue('Midgard Crew');
      expect(screen.getByText(/Up to 255/)).toBeInTheDocument();
    } else {
      expect(screen.queryByRole('textbox', { name: 'Server name' })).toBeNull();
      expect(screen.queryByText(/Up to 255/)).toBeNull();
    }
    expect(screen.getByRole('radiogroup', { name: 'Inactive timeout' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Leave' })).toBeInTheDocument();
    // Wait for the independent AFK read so no state update escapes the test.
    expect(await screen.findByRole('radio', { name: '5m', checked: true })).toBeInTheDocument();
  });
});
