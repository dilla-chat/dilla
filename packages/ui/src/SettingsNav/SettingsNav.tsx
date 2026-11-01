import { useLayoutEffect } from 'react';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import { useRovingFocus } from '../internal/roving.ts';
import './SettingsNav.css';

export interface SettingsNavProps {
  label: string;
  /** `icon` draws a 15px glyph; `group` adds a heading before its group. */
  items: readonly { id: string; label: string; icon?: IconName; group?: string }[];
  activeId: string;
  onSelect(id: string): void;
}

/** The section list of Settings: one tab stop with roving keyboard focus. */
export function SettingsNav({ label, items, activeId, onSelect }: SettingsNavProps) {
  const activeIndex = items.findIndex(i => i.id === activeId);
  const roving = useRovingFocus(activeIndex);
  const glyphs = items.some(i => i.icon);
  useLayoutEffect(() => {
    const active = Array.from(roving.ref.current?.querySelectorAll<HTMLButtonElement>('.d-settings-nav__item') ?? [])
      .find(item => item.getAttribute('aria-current') === 'page');
    active?.scrollIntoView?.({ block: 'nearest' });
  }, [activeId, items, roving.ref]);

  let previousGroup: string | undefined;
  return (
    <nav className="d-settings-nav" aria-label={label}>
      <ul role="list" className="d-settings-nav__list" ref={roving.ref} onKeyDown={roving.onKeyDown} onFocus={roving.onFocus}>
        {items.flatMap(item => {
          const showGroup = item.group !== undefined && item.group !== previousGroup;
          previousGroup = item.group;
          return [
            showGroup ? <li key={`heading-${item.id}`} className="d-settings-nav__group-entry"><h2 className="d-settings-nav__group">{item.group}</h2></li> : null,
            <li key={item.id} className="d-settings-nav__entry">
              <button type="button" className="d-settings-nav__item" aria-current={item.id === activeId ? 'page' : undefined} onClick={() => onSelect(item.id)}>
                {glyphs ? <span className="d-settings-nav__glyph">{item.icon ? <Icon name={item.icon} size={15} /> : null}</span> : null}
                {item.label}
              </button>
            </li>,
          ];
        })}
      </ul>
    </nav>
  );
}
