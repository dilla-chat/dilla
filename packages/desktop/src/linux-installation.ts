export type LinuxInstallKind = 'appimage' | 'system-package' | 'unknown';
export interface LinuxInstallationContext {
 /** Caller supplies the compiled build enum, never environment or argv. */
 installKind: LinuxInstallKind; version: string; channel: 'stable' | 'beta'; arch: string;
 executable: string; resourcesPath: string;
}
export type LinuxInstallation = { installKind: 'system-package' } | { installKind: 'appimage' };
const ROOT = '/opt/Dilla';
export async function resolveLinuxInstallation(context: LinuxInstallationContext): Promise<LinuxInstallation> {
 if (context.installKind === 'appimage') {
  if (context.executable === ROOT || context.executable.startsWith(ROOT + '/') || context.resourcesPath === ROOT || context.resourcesPath.startsWith(ROOT + '/')) throw new Error('AppImage installation layout conflicts with native package');
  return { installKind: 'appimage' };
 }
 if (context.installKind !== 'system-package' || context.executable !== ROOT + '/dilla-desktop' || context.resourcesPath !== ROOT + '/resources') throw new Error('Unsupported Linux installation layout');
 return { installKind: 'system-package' };
}
export type LinuxInstallationRole = 'check' | 'download' | 'install' | 'recovery' | 'watchdog' | 'sandbox-setup';
export function assertLinuxInstallationRole(installation: LinuxInstallation, role: LinuxInstallationRole): void {
 if (installation.installKind === 'system-package' && role === 'check') return;
 if (installation.installKind !== 'appimage') throw new Error('Update using your package manager');
}
