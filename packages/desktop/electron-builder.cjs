const channel = require('./package.json').version.includes('-beta.') ? 'beta' : 'stable';
const { sparklePublicKey } = require('./release-trust.json');
const linuxHooks = require('./scripts/linux-appimage-launcher.cjs');
// macOS signs with the Developer ID certificate the release credentials carry (CSC_LINK and CSC_KEY_PASSWORD, the
// base64 .p12 and its password) and notarizes with the App Store Connect API key (APPLE_API_KEY, APPLE_API_KEY_ID,
// APPLE_API_ISSUER); the workflow sets them from repository secrets on the macOS build only. Without them, as in a
// pull request from a fork or a local build, the app is ad-hoc signed as before. A present certificate that cannot
// sign fails the build rather than quietly shipping an unsigned app.
const macSigning = Boolean(process.env.CSC_LINK);
const macNotarizing = Boolean(process.env.APPLE_API_KEY && process.env.APPLE_API_KEY_ID && process.env.APPLE_API_ISSUER);
module.exports = {
  appId: 'dev.dilla.desktop', productName: 'Dilla', executableName: 'dilla-desktop',
  forceCodeSigning: macSigning,
  toolsets: { nsis: '1.2.1' }, // Hash-pinned NSIS 3.12 bundle; avoid the legacy temporary-directory runtime.
  directories: { output: 'out', buildResources: 'assets' },
  files: ['dist/**', 'package.json'],
  asar: true,
  afterPack: linuxHooks,
  artifactBuildStarted: linuxHooks.artifactBuildStarted,
  beforePack: async context => {
    const arch = { x64: 1, arm64: 3 }[process.arch];
    if (context.electronPlatformName !== process.platform || context.arch !== arch) throw new Error('Build desktop native capabilities on the matching OS and architecture');
  },
  extraResources: [{ from: 'native-bin', to: 'native', filter: ['dilla-audio*', 'dilla-sparkle.node'] }],
  artifactName: 'dilla-${version}-${os}-${arch}.${ext}',
  protocols: [{ name: 'Dilla invite', schemes: ['dilla'] }],
  publish: [{ provider: 'github', owner: 'dilla-chat', repo: 'dilla', releaseType: 'draft' }],
  // AppImage expands ${arch} to x86_64; release descriptors use canonical x64.
  linux: { artifactName: 'dilla-${version}-linux-x64.${ext}', syncDesktopName: true, target: [{ target: 'AppImage', arch: ['x64'] }], category: 'Network', icon: 'assets/icon.png' },
  // The pinned legacy AppImage target otherwise adds --no-sandbox to Exec.
  appImage: { executableArgs: [] },
  win: { target: [{ target: 'nsis', arch: ['x64'] }], icon: 'assets/icon.png', verifyUpdateCodeSignature: false, forceCodeSigning: false },
  nsis: { perMachine: false, oneClick: true, allowElevation: false, packElevateHelper: false, differentialPackage: false, deleteAppDataOnUninstall: false },
  mac: {
    target: [{ target: 'dmg', arch: [process.arch] }, { target: 'zip', arch: [process.arch] }],
    extraFiles: [{ from: 'native-bin/Sparkle.framework', to: 'Frameworks/Sparkle.framework' }],
    // The hardened runtime's entitlements are assets/entitlements.mac.plist and, for the helper processes,
    // assets/entitlements.mac.inherit.plist, which electron-builder reads from the build resources by those names.
    icon: 'assets/icon.png', ...(macSigning ? {} : { identity: '-' }), notarize: macNotarizing,
    hardenedRuntime: true, gatekeeperAssess: false,
    extendInfo: {
      CFBundleVersion: require('./package.json').version.replace('-beta.', 'b'),
      NSUserNotificationAlertStyle: 'alert',
      DillaDesktopChannel: channel,
      SUFeedURL: `https://dilla-chat.github.io/dilla/${channel}/appcast-${process.arch}.xml`,
      SUPublicEDKey: sparklePublicKey, SURequireSignedFeed: true, SUVerifyUpdateBeforeExtraction: true,
      SUEnableAutomaticChecks: false, SUAutomaticallyUpdate: false, SUEnableInstallerLauncherService: true,
      SUEnableDownloaderService: false,
      NSLocalNetworkUsageDescription: 'Dilla connects to your chosen instance and encrypted calls on your local network.',
      NSMicrophoneUsageDescription: 'Dilla uses your microphone in encrypted calls.',
      NSCameraUsageDescription: 'Dilla uses your camera when you enable video in a call.',
      NSScreenCaptureUsageDescription: 'Dilla shares the screen or application you choose in a call.',
    },
  },
  electronFuses: {
    runAsNode: false, enableCookieEncryption: true, enableNodeOptionsEnvironmentVariable: false,
    enableNodeCliInspectArguments: false, enableEmbeddedAsarIntegrityValidation: true,
    onlyLoadAppFromAsar: true, grantFileProtocolExtraPrivileges: false,
  },
};
