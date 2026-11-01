import type { ButtonHTMLAttributes } from 'react';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { ariaKeyshortcuts } from '../internal/ariaKeys.ts';
import { Tooltip } from '../Tooltip/Tooltip.tsx';
import './IconButton.css';

export type IconButtonProps = Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'children' | 'aria-label'> & {
  icon: IconName;
  /** The accessible name and the tooltip text (R15: the name starts with it). */
  label: string;
  /** The box in CSS px (default 32). On a phone layout the hit area is 44 whatever the size (base.css). */
  size?: 28 | 32 | 40 | 44;
  /** A toggle: sets aria-pressed; pressed draws the accent glyph and a 2px bottom bar (R27), never colour alone. */
  pressed?: boolean;
  /** The chord shown in the tooltip, and `aria-keyshortcuts` (display names mapped to ARIA key names, joined with +). */
  keys?: string[];
  /** False for a control whose label is visible beside it (default true). */
  tooltip?: boolean;
};

const GLYPH: Record<NonNullable<IconButtonProps['size']>, number> = { 28: 15, 32: 18, 40: 20, 44: 20 };

export function IconButton({ icon, label, size = 32, pressed, keys, tooltip = true, type = 'button', className, ...rest }: IconButtonProps) {
  const button = (
    <button type={type} className={['d-icon-button', className].filter(Boolean).join(' ')} data-size={size} aria-label={label}
      aria-pressed={pressed} aria-keyshortcuts={keys?.length ? ariaKeyshortcuts(keys) : undefined} {...rest}>
      <Icon name={icon} size={GLYPH[size]} />
    </button>
  );
  return tooltip ? <Tooltip label={label} keys={keys}>{button}</Tooltip> : button;
}
