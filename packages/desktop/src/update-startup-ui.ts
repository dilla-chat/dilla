/** During update validation, only immutable bundled bootstrap may talk to main.
 * OS windows remain hidden/disabled and no tray exists until durable activation. */
export function pendingStartupCommand(type: string): boolean {
 return ['boot-ready', 'window-mode', 'desktop-theme', 'guard'].includes(type);
}
