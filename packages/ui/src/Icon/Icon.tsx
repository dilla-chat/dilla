import { ICONS, type IconElement, type IconName } from './icons.ts';
import './Icon.css';

export type { IconName } from './icons.ts';

export interface IconProps {
  name: IconName;
  /** The rendered size in CSS px (default 20); the 24-unit stroke of 2 scales with it. */
  size?: number;
  /** The stroke width in the 24-unit box (default 2); the person menu's check is 3, as the prototype draws it. */
  stroke?: number;
}

/**
 * A monoline icon (Lucide, vendored in icons.ts): stroke `currentColor`, so it takes its control's colour and follows
 * forced colours. Always decorative: `aria-hidden` and unfocusable, so the control around it carries the name.
 */
export function Icon({ name, size = 20, stroke = 2 }: IconProps) {
  const nodes: readonly IconElement[] = ICONS[name];
  return (
    <svg className="d-icon" data-icon={name} xmlns="http://www.w3.org/2000/svg" width={size} height={size} viewBox="0 0 24 24" fill="none"
      stroke="currentColor" strokeWidth={stroke} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      {nodes.map(([tag, attrs], i) => {
        switch (tag) {
          case 'path': return <path key={i} {...attrs} />;
          case 'polyline': return <polyline key={i} {...attrs} />;
          case 'rect': return <rect key={i} {...attrs} />;
          case 'circle': return <circle key={i} {...attrs} />;
          case 'line': return <line key={i} {...attrs} />;
        }
      })}
    </svg>
  );
}
