import type { DesktopBridge, DesktopInfo } from '../../desktop/src/contract.ts';
import './DesktopTitlebar.css';

/** Native window controls occupy the overlay's reserved area. Only the menu
 * button opts out of dragging; the renderer receives no native menu arguments. */
export function DesktopTitlebar({ bridge, info }: { bridge: DesktopBridge; info: DesktopInfo }) {
 const instance = new URL(info.instance);
 return <header className="dw-desktop-titlebar" data-platform={info.platform} aria-label="Dilla window">
  <button type="button" className="dw-desktop-titlebar__menu" aria-label="Dilla menu" aria-haspopup="menu" title="Dilla menu" onClick={() => { void bridge.send({ type: 'window-menu' }).catch(() => {}); }}>
   <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true"><path d="M2 4h12M2 8h12M2 12h12" /></svg>
   <span>dilla</span>
  </button>
  <span className="dw-desktop-titlebar__instance" title={info.instance}>{instance.host}{instance.pathname === '/' ? '' : instance.pathname}</span>
 </header>;
}
