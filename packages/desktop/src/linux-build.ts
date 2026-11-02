import type { LinuxInstallKind } from './linux-installation.ts';

declare const __DILLA_LINUX_INSTALL_KIND__: 'appimage' | 'system-package';
/** Esbuild replaces this literal in production builds. Unmarked source/fixtures
 * never acquire installation authority from APPIMAGE, argv or a writable file. */
export const LINUX_INSTALL_KIND: LinuxInstallKind = typeof __DILLA_LINUX_INSTALL_KIND__ === 'undefined' ? 'unknown' : __DILLA_LINUX_INSTALL_KIND__;
export function assertLinuxSelfUpdate(): void {
 if (LINUX_INSTALL_KIND !== 'appimage') throw new Error(LINUX_INSTALL_KIND === 'system-package' ? 'Update using your package manager' : 'This build has no recognized Linux installation authority');
}
