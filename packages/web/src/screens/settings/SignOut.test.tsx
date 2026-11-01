import { describe, it, expect } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { CoreProvider } from '../../core/context.tsx';
import { FakeClient } from '../../test/fake-client.ts';
import { expectNoAxeViolations } from '../../test/setup.ts';
import { SignOutAction } from './SignOut.tsx';

function setup() {
  const fake = new FakeClient();
  const user = userEvent.setup();
  const view = render(<div className="d-root"><CoreProvider client={fake}><SignOutAction /></CoreProvider></div>);
  return { fake, user, view };
}

describe('SignOutAction', () => {
  it('is the danger button that opens the recovery-key dialog', async () => {
    const { user, view } = setup();
    const button = screen.getByRole('button', { name: 'Sign out and remove this browser' });
    expect(screen.queryByRole('dialog')).toBeNull();
    await user.click(button);
    expect(screen.getByRole('dialog', { name: 'Sign out and remove this browser?' })).toBeInTheDocument();
    await expectNoAxeViolations(view.container);
  });

  it('returns focus to the button after the dialog is closed', async () => {
    const { user } = setup();
    const button = screen.getByRole('button', { name: 'Sign out and remove this browser' });
    await user.click(button);
    const dialog = screen.getByRole('dialog', { name: 'Sign out and remove this browser?' });
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(button).toHaveFocus();
  });
});
