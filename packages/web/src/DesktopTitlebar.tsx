import { useEffect, useLayoutEffect, useState } from 'react';
import { createPortal } from 'react-dom';
import type { DesktopBridge, DesktopInfo } from '../../desktop/src/contract.ts';
import './DesktopTitlebar.css';

/** The workspace header and compact card share a bounded window capability. */
export function DesktopTitlebar({ bridge, info }: { bridge: DesktopBridge; info: DesktopInfo }) {
 useEffect(() => {
  const update = () => {
   const theme = document.documentElement.dataset.theme;
   if (theme === 'signal') void bridge.send({ type: 'desktop-theme', theme }).catch(() => {});
  };
  update();
  const observer = new MutationObserver(update);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  return () => observer.disconnect();
 }, [bridge]);
 const [host, setHost] = useState<HTMLElement | null>(null);
 useLayoutEffect(() => {
  const update = () => {
   // Modal dialogs make the rest of the document inert. Keep native window
   // actions inside the active dialog while it is open, at the same position.
   const dialogs = document.querySelectorAll<HTMLDialogElement>('dialog[open]');
   setHost(dialogs.item(dialogs.length - 1) ?? document.querySelector<HTMLElement>('.d-topbar'));
  };
  update();
  const observer = new MutationObserver(update);
  observer.observe(document.body, { childList: true, subtree: true, attributes: true, attributeFilter: ['open'] });
  return () => observer.disconnect();
 }, []);
 const control = (action: 'minimize' | 'toggle-maximize' | 'close') => { void bridge.send({ type: 'window-control', action }).catch(() => {}); };
 return <>{createPortal(<div className="dw-desktop-titlebar" data-platform={info.platform} role="group" aria-label="Window controls">
  <button type="button" className="dw-desktop-titlebar__menu" aria-label="Dilla menu" aria-haspopup="menu" title={info.instance} onClick={() => { void bridge.send({ type: 'window-menu' }).catch(() => {}); }}>
   <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M2 4h12M2 8h12M2 12h12" /></svg>
  </button>
  {info.platform !== 'darwin' && <div className="dw-desktop-titlebar__controls">
   <button type="button" aria-label="Minimize window" onClick={() => control('minimize')}><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3 8h10" /></svg></button>
   <button type="button" aria-label="Maximize or restore window" onClick={() => control('toggle-maximize')}><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.5 3.5h9v9h-9z" /></svg></button>
   <button type="button" aria-label="Close window" onClick={() => control('close')}><svg viewBox="0 0 16 16" aria-hidden="true"><path d="m4 4 8 8M4 12l8-8" /></svg></button>
  </div>}
 </div>, host ?? document.body)}
 <div className="dw-desktop-card-drag" aria-hidden="true" />
 <button type="button" className="dw-desktop-card-close" aria-label="Close login window" onClick={() => control('close')}><svg viewBox="0 0 16 16" aria-hidden="true"><path d="m4 4 8 8M4 12l8-8" /></svg></button>
 </>;
}
