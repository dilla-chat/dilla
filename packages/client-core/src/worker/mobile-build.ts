// The mobile app's build flag. packages/mobile builds the web client with __DILLA_MOBILE__ defined true; the web and
// desktop builds define it false (packages/web/vite.config.ts), so every `if (MOBILE_BUILD …)` branch folds away there.
// A bundler without the define (tests, the core-worker harness) reads the global at run time and finds nothing: false.
declare const __DILLA_MOBILE__: boolean | undefined;
export const MOBILE_BUILD: boolean = typeof __DILLA_MOBILE__ === 'boolean' ? __DILLA_MOBILE__ : false;

/** The mobile app's own origins (iOS, Android). Only a mobile-build worker running at one of them is a mobile worker. */
export function isMobileAppOrigin(origin: string): boolean {
  return origin === 'capacitor://localhost' || origin === 'https://localhost';
}
