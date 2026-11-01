import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { ReplyChip } from './ReplyChip.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('ReplyChip', () => {
  it('shows who is replied to and the excerpt, and cancels from its named button', async () => {
    const onCancel = vi.fn();
    const { container } = render(<ReplyChip label="Replying to björn" name="björn" excerpt="after nine, still on the boat" cancelLabel="cancel reply" onCancel={onCancel} />);
    expect(container.firstElementChild).toHaveClass('d-reply-chip');
    // The prototype's "Replying to **Ada** · excerpt" (§5.5).
    expect(container.querySelector('.d-reply-chip__label')).toHaveTextContent('Replying to björn');
    expect(container.querySelector('.d-reply-chip__label b')).toHaveTextContent('björn');
    expect(container.querySelector('.d-reply-chip__excerpt')).toHaveTextContent('· after nine, still on the boat');
    const cancel = screen.getByRole('button', { name: 'cancel reply' });
    expect(cancel.querySelector('svg')).not.toBeNull();
    await userEvent.click(cancel);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><ReplyChip label="replying to björn" excerpt="after nine" cancelLabel="cancel reply" onCancel={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
});
