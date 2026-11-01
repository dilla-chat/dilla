import { useLayoutEffect, useRef } from 'react';

/** All targets belong to this workspace, including when the covered shell has matching IDs. */
export function useSettingsFocus(section: string | null, anchor: string | null) {
  const contentRef = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    if (section === null) return;
    const root = contentRef.current;
    if (!root) return;
    const named = anchor === null ? null : root.querySelector<HTMLElement>(`[id="${anchor}"], [data-setting="${anchor}"]`);
    const target = named?.matches('input, select, textarea, button, [tabindex]') ? named
      : named?.querySelector<HTMLElement>('input, select, textarea, button, [tabindex]') ?? named ?? root.querySelector<HTMLElement>('h2');
    if (target) {
      if (!target.matches('input, select, textarea, button, [tabindex]')) target.tabIndex = -1;
      target.focus(); target.scrollIntoView?.({ block: 'nearest' });
    }
  }, [section, anchor]);
  return contentRef;
}
