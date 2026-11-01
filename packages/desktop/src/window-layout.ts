export type WindowMode = 'starting' | 'authentication' | 'workspace';
export const WORKSPACE_MINIMUM = { width: 900, height: 600 };

/** All dimensions are native content dimensions in DIP, never an emulated page
 * viewport. Authentication remains a compact, fixed-width desktop dialog. */
export function windowShape(mode: WindowMode, contentHeight?: number | null, workAreaHeight = 1080) {
  if (mode === 'workspace') return { width: 1240, height: Math.max(600, Math.min(820, workAreaHeight - 80)), minWidth: WORKSPACE_MINIMUM.width, minHeight: WORKSPACE_MINIMUM.height, resizable: true };
  const wanted = mode === 'starting' ? 280 : contentHeight ?? 620;
  return { width: 400, height: Math.max(220, Math.min(wanted, workAreaHeight - 80)), minWidth: 400, minHeight: 220, resizable: false };
}
