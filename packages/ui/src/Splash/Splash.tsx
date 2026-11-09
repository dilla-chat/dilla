import { BrandMark } from '../TopBar/TopBar.tsx';
import { Button } from '../Button/Button.tsx';
import './Splash.css';

export interface SplashProps {
  status: string;
  detail?: string;
  action?: { label: string; onAction(): void };
  /** A second, quieter action after the first (the mobile app's Switch instance). */
  secondary?: { label: string; onAction(): void };
}

/**
 * The full-viewport boot surface: the brand, one atomic status region holding
 * the status line and its detail, and an optional action outside that region.
 */
export function Splash({ status, detail, action, secondary }: SplashProps) {
  return (
    <main className="d-splash">
      <BrandMark />
      <div className="d-splash__status" role="status" aria-atomic="true">
        <p className="d-splash__line">{status}</p>
        {detail ? <p className="d-splash__detail">{detail}</p> : null}
      </div>
      {action ? <Button variant="accent" onClick={action.onAction}>{action.label}</Button> : null}
      {secondary ? <Button variant="ghost" onClick={secondary.onAction}>{secondary.label}</Button> : null}
    </main>
  );
}
