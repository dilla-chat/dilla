// The native desktop app's updater (GPUI release plan Task 14b): the tray and application-menu rows and the
// notifications after a check the person asked for. Rows and the two notification texts are beta.5's main-process copy
// (`packages/desktop/src/main.ts`, `update.ts` at tag desktop-v0.1.0-beta.5); the rest is new copy for Jonas's approval.
export const desktopUpdate = {
  'desktop.update.check': 'Check for updates',
  'desktop.update.checking': 'Checking for updates…',
  'desktop.update.retry': 'Update failed — retry',
  'desktop.update.restart': 'Restart to update',
  'desktop.update.available': 'Dilla {version} is available. Update using your package manager.',
  'desktop.update.packageManager': 'Update using your package manager',
  'desktop.update.upToDate': 'Dilla is up to date.',
  'desktop.update.failedTitle': 'Could not update Dilla',
  'desktop.update.error.install': 'The update could not be installed. Check for updates to retry.',
  // New copy (for Jonas's approval): a copy the updater does not replace, and why a check failed.
  'desktop.update.availableDownload': 'Dilla {version} is available. Download it from the release page.',
  'desktop.update.releasePage': 'Open the release page',
  'desktop.update.error.network': 'Dilla could not reach the update server. Try again later.',
  'desktop.update.error.invalid': 'The update did not pass its checks, so Dilla did not install it.',
};
