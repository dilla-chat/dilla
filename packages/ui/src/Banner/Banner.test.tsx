import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi } from 'vitest';
import { Banner } from './Banner.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-01: onboarding.error.network, onboarding.keys.registering, onboarding.error.rateLimitedNoWait, onboarding.error.reload.
const NETWORK = 'The connection dropped while creating your account. Reload the page to finish.';
const REGISTERING = 'Creating your account on dilla.thim.dev…';

describe('Banner', () => {
  it('is a status for info and an alert for warn and danger', () => {
    render(<>
      <Banner tone="info">{REGISTERING}</Banner>
      <Banner tone="warn">Too many attempts from this network. Wait a few minutes, then try again.</Banner>
      <Banner tone="danger">{NETWORK}</Banner>
    </>);
    expect(screen.getByRole('status')).toHaveTextContent(REGISTERING);
    expect(screen.getByRole('status')).toHaveAttribute('data-tone', 'info');
    const alerts = screen.getAllByRole('alert');
    expect(alerts).toHaveLength(2);
    expect(alerts[0]).toHaveAttribute('data-tone', 'warn');
    expect(alerts[1]).toHaveAttribute('data-tone', 'danger');
    expect(alerts[1]).toHaveTextContent(NETWORK);
  });

  it('carries a tone glyph that assistive tech skips, so tone is never colour alone', () => {
    const { container } = render(<>
      <Banner tone="info">a</Banner><Banner tone="warn">b</Banner><Banner tone="danger">c</Banner>
    </>);
    const glyphs = Array.from(container.querySelectorAll('.d-banner__glyph'));
    expect(glyphs[0]).toHaveTextContent('›');
    expect(glyphs[1].querySelector('[data-icon="triangle-alert"]')).not.toBeNull();
    expect(glyphs[2].querySelector('[data-icon="x"]')).not.toBeNull();
    for (const g of glyphs) expect(g).toHaveAttribute('aria-hidden', 'true');
  });

  it('renders the action as a button that calls onAction', async () => {
    const user = userEvent.setup();
    const onAction = vi.fn();
    render(<Banner tone="danger" action={{ label: 'Reload', onAction }}>{NETWORK}</Banner>);
    await user.click(screen.getByRole('button', { name: 'Reload' }));
    expect(onAction).toHaveBeenCalledTimes(1);
  });

  it('renders no button without an action and carries the d-banner root class', () => {
    const { container } = render(<Banner tone="info">{REGISTERING}</Banner>);
    expect(screen.queryByRole('button')).toBeNull();
    expect(container.firstElementChild).toHaveClass('d-banner');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root"><Banner tone="danger" action={{ label: 'Reload', onAction: () => {} }}>{NETWORK}</Banner></div>);
    await expectNoAxeViolations(container);
  });
});
