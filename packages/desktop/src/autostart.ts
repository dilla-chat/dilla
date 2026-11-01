import { mkdir, rm, writeFile } from 'node:fs/promises';
import { isAbsolute, join } from 'node:path';

/** Desktop Entry Exec quoting (not a shell command). Backslashes are escaped again for the key-value layer. */
export function desktopEntry(executable: string): string {
  if (!isAbsolute(executable) || /[\r\n\u0000]/.test(executable)) throw new Error('Invalid autostart executable');
  const quoted = executable.replaceAll('%', '%%').replace(/[\\"`$]/g, ch => '\\' + ch).replaceAll('\\', '\\\\');
  return `[Desktop Entry]\nType=Application\nName=Dilla\nExec="${quoted}" --hidden\nTerminal=false\nStartupNotify=false\nX-GNOME-Autostart-enabled=true\n`;
}

export async function linuxAutostart(configDirectory: string, executable: string, enabled: boolean): Promise<void> {
  const dir = join(configDirectory, 'autostart');
  const path = join(dir, 'dilla-desktop.desktop');
  if (!enabled) { await rm(path, { force: true }); return; }
  await mkdir(dir, { recursive: true, mode: 0o700 });
  await writeFile(path, desktopEntry(executable), { mode: 0o600 });
}
