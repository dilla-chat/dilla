import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { PhoneHeader } from './PhoneHeader.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

describe('PhoneHeader', () => {
  it('names the back button and calls onBack', async () => {
    const onBack = vi.fn();
    render(<PhoneHeader title="# general" backLabel="back to channels" onBack={onBack} />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'back to channels' }));
    expect(onBack).toHaveBeenCalledTimes(1);
  });
  it('has a level-two heading', () => {
    render(<PhoneHeader title="general" />);
    expect(screen.getByRole('heading', { level: 2, name: 'general' })).toHaveClass('d-phone-header__title');
  });
  it('shows a dot button after the title only when given a dot, and it opens the call', async () => {
    const onOpen = vi.fn();
    const { rerender } = render(<PhoneHeader title="general" />);
    expect(screen.queryByRole('button')).toBeNull();
    rerender(<PhoneHeader title="general" dot={{ label: 'open the call in room', onOpen }} />);
    const dot = screen.getByRole('button', { name: 'open the call in room' });
    expect(dot).toHaveClass('d-phone-header__dot');
    expect(screen.getByRole('heading', { level: 2 }).nextElementSibling).toBe(dot);
    await userEvent.setup().click(dot);
    expect(onOpen).toHaveBeenCalledTimes(1);
  });
  it('has no back button without onBack', () => {
    render(<PhoneHeader title="general" backLabel="back" actions={<button type="button">more</button>} />);
    expect(screen.queryByRole('button', { name: 'back' })).toBeNull();
    expect(screen.getByRole('button', { name: 'more' })).toBeInTheDocument();
  });
  it('has no axe violations', async () => {
    const { container } = render(<div className="d-root"><PhoneHeader title="general" backLabel="back" onBack={() => {}} /></div>);
    await expectNoAxeViolations(container);
  });
  it('shows the topic after the title when given (Signal §5.10), and none without one', () => {
    const { container, rerender } = render(<PhoneHeader title="general" topic="raids, plans and the bridge troll" />);
    expect(container.querySelector('.d-phone-header__topic')).toHaveTextContent('raids, plans and the bridge troll');
    rerender(<PhoneHeader title="general" />);
    expect(container.querySelector('.d-phone-header__topic')).toBeNull();
  });
});
