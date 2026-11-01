import { useEffect, useRef } from 'react';
import type { ReactNode } from 'react';
import { BrandMark } from '../TopBar/TopBar.tsx';
import './OnboardingFrame.css';

export interface OnboardingFrameProps {
  title: string;
  stepLabel: string;
  /** The prototype's progress dots (Signal task 10 fix round 1): step `n` (1-based) of `total`; none when omitted. */
  progress?: { n: number; total: number };
  children: ReactNode;
  footer: ReactNode;
}

/**
 * The onboarding step frame: a top bar with the brand, then a main column with
 * the heading, the step line, the step's content and its buttons. The heading
 * takes focus on mount and whenever the title changes, and at no other time.
 */
export function OnboardingFrame({ title, stepLabel, progress, children, footer }: OnboardingFrameProps) {
  const titleRef = useRef<HTMLHeadingElement>(null);
  useEffect(() => { titleRef.current?.focus(); }, [title]);
  return (
    <div className="d-onboarding-frame">
      <header className="d-onboarding-frame__bar"><BrandMark /></header>
      <main className="d-onboarding-frame__card">
        <h1 ref={titleRef} tabIndex={-1} className="d-onboarding-frame__title">{title}</h1>
        {stepLabel || progress ? (
          <div className="d-onboarding-frame__head">
            {stepLabel ? <p className="d-onboarding-frame__step d-label">{stepLabel}</p> : <span />}
            {progress ? (
              <span className="d-onboarding-frame__dots" aria-hidden="true">
                {Array.from({ length: progress.total }, (_, i) => (
                  <span key={i} className="d-onboarding-frame__dot" data-state={i + 1 === progress.n ? 'current' : i + 1 < progress.n ? 'done' : undefined} />
                ))}
              </span>
            ) : null}
          </div>
        ) : null}
        <div className="d-onboarding-frame__body">{children}</div>
        {footer === null || footer === undefined || footer === false ? null : <div className="d-onboarding-frame__footer">{footer}</div>}
      </main>
    </div>
  );
}
