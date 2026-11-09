import type { CapacitorConfig } from '@capacitor/cli';

// The client is bundled (webDir), never loaded from a server: there is deliberately no server.url.
// Instances are reached over HTTPS from capacitor://localhost (iOS) or https://localhost (Android),
// the two origins dillad accepts through http.mobile_origins.
const config: CapacitorConfig = {
  appId: 'chat.dilla.app',
  appName: 'Dilla',
  webDir: 'dist',
  // The web boot screen's --bg: the WebView paints it before the first page, between the launch screen and the boot screen.
  backgroundColor: '#08090B',
  android: { allowMixedContent: false },
  plugins: {
    // Desktop-auth goes through CapacitorHttp explicitly (src/browser-auth.ts); fetch and XHR stay the WebView's.
    CapacitorHttp: { enabled: false },
    LocalNotifications: { presentationOptions: ['banner', 'list', 'sound'] },
  },
};

export default config;
