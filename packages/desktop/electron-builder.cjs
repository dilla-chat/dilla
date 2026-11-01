const channel = require('./package.json').version.includes('-beta.') ? 'beta' : 'stable';
const { sparklePublicKey } = require('./release-trust.json');
module.exports = {
  appId: 'dev.dilla.desktop', productName: 'Dilla', executableName: 'dilla-desktop',
  directories: { output: 'out', buildResources: 'assets' },
  files: ['dist/**', 'package.json'],
  asar: true,
  extraResources: [{ from: 'native-bin', to: 'native', filter: ['dilla-audio*', 'dilla-sparkle.node'] }],
  artifactName: 'dilla-${version}-${os}-${arch}.${ext}',
  protocols: [{ name: 'Dilla invite', schemes: ['dilla'] }],
  publish: [{ provider: 'github', owner: 'jonasthim', repo: 'dilla', releaseType: 'draft' }],
  linux: { target: [{ target: 'AppImage', arch: ['x64'] }], category: 'Network', icon: 'assets/icon.png' },
  win: { target: [{ target: 'nsis', arch: ['x64'] }], icon: 'assets/icon.png' },
  nsis: { perMachine: false, oneClick: true, allowElevation: false, deleteAppDataOnUninstall: false },
  mac: {
    target: [{ target: 'dmg', arch: ['x64', 'arm64'] }, { target: 'zip', arch: ['x64', 'arm64'] }],
    extraFiles: [{ from: 'native-bin/Sparkle.framework', to: 'Frameworks/Sparkle.framework' }],
    icon: 'assets/icon.png', identity: '-', notarize: false,
    extendInfo: {
      DillaDesktopChannel: channel,
      SUFeedURL: `https://github.com/jonasthim/dilla/releases/download/desktop-${channel}/appcast.xml`,
      SUPublicEDKey: sparklePublicKey, SURequireSignedFeed: true, SUVerifyUpdateBeforeExtraction: true,
      SUEnableAutomaticChecks: false, SUAutomaticallyUpdate: false, SUEnableInstallerLauncherService: true,
      SUEnableDownloaderService: false,
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
