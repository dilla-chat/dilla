import { BrandMark } from '../TopBar/TopBar.tsx';
import { Button } from '../Button/Button.tsx';
import './Splash.css';

export interface SplashProps {
  status: string;
  detail?: string;
  action?: { label: string; onAction(): void };
}

/**
 * The full-viewport boot surface: the brand, one atomic status region holding
 * the status line and its detail, and an optional action outside that region.
 */
export function Splash({ status, detail, action }: SplashProps) {
  return (
    <main className="d-splash">
      <BrandMark />
      <div className="d-splash__status" role="status" aria-atomic="true">
        <p className="d-splash__line">{status}</p>
        {detail ? <p className="d-splash__detail">{detail}</p> : null}
      </div>
      {action ? <Button variant="accent" onClick={action.onAction}>{action.label}</Button> : null}
    </main>
  );
}
