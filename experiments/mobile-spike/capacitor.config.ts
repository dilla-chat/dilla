import type { CapacitorConfig } from '@capacitor/cli';

// M0 spike only. chat.dilla.spike never collides with the real app id chat.dilla.app.
const config: CapacitorConfig = {
  appId: 'chat.dilla.spike',
  appName: 'dilla spike',
  webDir: 'dist',
  // Logcat carries console.log as Capacitor/Console; keep it on in the release variant too.
  loggingBehavior: 'production',
};

export default config;
