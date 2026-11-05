import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { Splash } from './Splash.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

const STATUS = 'dilla is open in another tab';
const DETAIL = 'Close it, or switch to it. This tab takes over when the other one closes.';

describe('Splash', () => {
  it('is the main landmark with the brand mark and one status region holding status and detail', () => {
    const { container } = render(<Splash status={STATUS} detail={DETAIL} />);
    const main = screen.getByRole('main');
    expect(main).toHaveClass('d-splash');
    expect(container.firstElementChild).toBe(main);
    expect(screen.getByText('DILLA')).toBeInTheDocument();
    const status = screen.getByRole('status');
    expect(status).toHaveAttribute('aria-atomic', 'true');
    expect(status).toHaveTextContent(STATUS);
    expect(status).toHaveTextContent(DETAIL);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('renders no detail paragraph without a detail', () => {
    const { container } = render(<Splash status="starting" />);
    expect(screen.getByRole('status')).toHaveTextContent('starting');
    expect(container.querySelector('.d-splash__detail')).toBeNull();
  });

  it('renders the action outside the status region and calls onAction', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    render(<Splash status="This browser lost the data for this device" action={{ label: 'start over', onAction }} />);
    const button = screen.getByRole('button', { name: 'start over' });
    expect(screen.getByRole('status')).not.toContainElement(button);
    await user.click(button);
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it('renders an optional second action beside the first, outside the status region', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    const onSecondary = vi.fn();
    render(<Splash status="dilla could not start" action={{ label: 'Try again', onAction }} secondary={{ label: 'Switch instance', onAction: onSecondary }} />);
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Try again', 'Switch instance']);
    const second = screen.getByRole('button', { name: 'Switch instance' });
    expect(screen.getByRole('status')).not.toContainElement(second);
    await user.click(second);
    expect(onSecondary).toHaveBeenCalledTimes(1);
    expect(onAction).not.toHaveBeenCalled();
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Splash status={STATUS} detail={DETAIL} action={{ label: 'try again', onAction: () => {} }} /></div>);
    await expectNoAxeViolations(container);
  });
});
