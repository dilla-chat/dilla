import { render, screen, within } from '@testing-library/react';
import { describe, it, expect } from 'vitest';
import { OnboardingFrame } from './OnboardingFrame.tsx';
import { Button } from '../Button/Button.tsx';
import { expectNoAxeViolations } from '../test/setup.ts';

// L-COPY-01: onboarding.connect.body with {instance} = dilla.thim.dev.
const CONNECT = 'You need an invite from someone on dilla.thim.dev. Paste it below.';

function frame(title: string, stepLabel: string, body: string) {
  return (
    <OnboardingFrame title={title} stepLabel={stepLabel}
      footer={<><Button>Back</Button><Button variant="accent" type="submit">Continue</Button></>}>
      <p>{body}</p>
    </OnboardingFrame>
  );
}

describe('OnboardingFrame', () => {
  it('puts the brand in a top header and the heading, step line, children and footer in main, in that order', () => {
    const { container } = render(frame('Join dilla.thim.dev', 'Step 1 of 5', CONNECT));
    expect(container.firstElementChild).toHaveClass('d-onboarding-frame');
    expect(within(screen.getByRole('banner')).getByText('DILLA')).toBeInTheDocument();
    const main = screen.getByRole('main');
    expect(main).not.toContainElement(screen.getByText('DILLA'));
    const order = Array.from(main.children).map(el => el.className);
    // Signal task 10 fix round 1: the step line sits in the card's head row (with the dots), drawn above the title by CSS order.
    expect(order).toEqual(['d-onboarding-frame__title', 'd-onboarding-frame__head', 'd-onboarding-frame__body', 'd-onboarding-frame__footer']);
    expect(within(main).getByRole('heading', { level: 1, name: 'Join dilla.thim.dev' })).toBeInTheDocument();
    expect(within(main).getByText('Step 1 of 5')).toBeInTheDocument();
    expect(within(main).getByText(CONNECT)).toBeInTheDocument();
    expect(within(main).getByRole('button', { name: 'Continue' })).toBeInTheDocument();
  });

  it('draws the progress dots: done and current in the accent, the current one wide, and hides them from assistive technology', () => {
    const { container } = render(<OnboardingFrame title="Sign in" stepLabel="Sign in · step 2 of 4" progress={{ n: 2, total: 4 }} footer={null}>x</OnboardingFrame>);
    const dots = Array.from(container.querySelectorAll('.d-onboarding-frame__dot'));
    expect(dots.map(d => d.getAttribute('data-state'))).toEqual(['done', 'current', null, null]);
    expect(container.querySelector('.d-onboarding-frame__dots')).toHaveAttribute('aria-hidden', 'true');
  });

  it('moves focus to the heading on mount and whenever the title changes', () => {
    const { rerender } = render(frame('Join dilla.thim.dev', 'Step 1 of 5', 'one'));
    expect(screen.getByRole('heading', { level: 1, name: 'Join dilla.thim.dev' })).toHaveFocus();
    // Focused by script only, never in the tab order: no focus ring is drawn on it (OnboardingFrame.css).
    expect(screen.getByRole('heading', { level: 1, name: 'Join dilla.thim.dev' })).toHaveAttribute('tabindex', '-1');
    screen.getByRole('button', { name: 'Continue' }).focus();
    rerender(frame('Choose your name', 'Step 2 of 5', 'two'));
    expect(screen.getByRole('heading', { level: 1, name: 'Choose your name' })).toHaveFocus();
  });

  it('leaves focus alone when only the children change', () => {
    const { rerender } = render(frame('Choose your name', 'Step 2 of 5', 'one'));
    const cont = screen.getByRole('button', { name: 'Continue' });
    cont.focus();
    rerender(frame('Choose your name', 'Step 2 of 5', 'two'));
    expect(cont).toHaveFocus();
  });

  it('omits the step line for an empty stepLabel and the footer for a null footer', () => {
    const { container } = render(
      <OnboardingFrame title="dilla.thim.dev is not taking new accounts" stepLabel="" footer={null}>
        <p>Ask the host of dilla.thim.dev when sign-ups open again.</p>
      </OnboardingFrame>,
    );
    expect(container.querySelector('.d-onboarding-frame__step')).toBeNull();
    expect(container.querySelector('.d-onboarding-frame__footer')).toBeNull();
    expect(screen.getByRole('heading', { level: 1, name: 'dilla.thim.dev is not taking new accounts' })).toHaveAttribute('tabindex', '-1');
  });

  it('has no serious axe violations', async () => {
    const { container } = render(<div className="d-root">{frame('Join dilla.thim.dev', 'Step 1 of 5', CONNECT)}</div>);
    await expectNoAxeViolations(container);
  });
});
