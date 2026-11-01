import type { MouseEvent } from 'react';
import { Icon, type IconName } from '../Icon/Icon.tsx';
import './TabBar.css';

export interface TabBarItem {
  id: string;
  label: string;
  href: string;
  icon: IconName;
  current: boolean;
  /** Above 0, an 8px dot on the tab (Signal §5.10) and the count in the link's name ("99+" above 99); 0 or absent shows none. */
  count?: number;
  /** The link's accessible name while count > 0 (the caller fills nav.tab.unread). */
  countLabel?: string;
}

export interface TabBarProps {
  label: string;
  items: readonly TabBarItem[];
  onSelect(id: string): void;
}

/** A click that asks the browser for something else (a new tab or window, a download): left to the browser. */
const isModified = (e: MouseEvent) => e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey;

/**
 * The phone's 56px bottom navigation (Signal §5.10): one link per place, a 19px icon over its 10px label, the
 * current one in --fg under a 2px accent bar. Real links, so a long-press or a middle click works; a plain click is handled in place.
 * A count is an 8px --mention dot, decoration: the link is named by `countLabel` (or label and count) while there is one.
 */
export function TabBar({ label, items, onSelect }: TabBarProps) {
  return (
    <nav className="d-tab-bar" aria-label={label}>
      <ul className="d-tab-bar__list" role="list">
        {items.map(item => {
          const count = item.count ?? 0;
          return (
            <li key={item.id} className="d-tab-bar__item">
              <a className="d-tab-bar__link" href={item.href} aria-current={item.current ? 'page' : undefined}
                aria-label={count > 0 ? item.countLabel ?? `${item.label}, ${count > 99 ? '99+' : count}` : undefined}
                onClick={e => {
                  if (isModified(e)) return;
                  e.preventDefault();
                  onSelect(item.id);
                }}>
                <span className="d-tab-bar__icon">
                  <Icon name={item.icon} size={19} />
                </span>
                {count > 0 ? <span className="d-tab-bar__dot" aria-hidden="true" /> : null}
                <span className="d-tab-bar__label">{item.label}</span>
              </a>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
