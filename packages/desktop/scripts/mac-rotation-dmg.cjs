const { execFileSync } = require('node:child_process');

/**
 * The macOS update of a key-rotation release is the disk image (feed-format.mjs, RELEASING.md "Staged key
 * rotation"): Sparkle accepts a new EdDSA key, with SUVerifyUpdateBeforeExtraction, only from "a Developer ID code
 * signed disk image (dmg)". While release-trust.json records a pending macOS key change (signing.macRotation, set by
 * `release-keys.mjs rotate --promote`), electron-builder signs the DMG with the app's Developer ID identity
 * (`dmg.sign`, electron-builder.cjs) and this hook notarizes and staples it, as the app itself is. Every other
 * build leaves the DMG as before. Without credentials (a fork's pull request) nothing is signed here; the macOS CI
 * job's disk image check (check-mac-dmg.mjs) refuses that outside pull requests.
 */
function macRotationPending(trust) {
 return Boolean(trust?.signing?.macRotation);
}

function createAfterAllArtifactBuild({ trust, env = process.env, platform = process.platform, run = execFileSync, log = console.log } = {}) {
 return async function afterAllArtifactBuild(result) {
  if (platform !== 'darwin' || !macRotationPending(trust)) return [];
  const notarizing = Boolean(env.APPLE_API_KEY && env.APPLE_API_KEY_ID && env.APPLE_API_ISSUER && env.CSC_LINK);
  const images = (result?.artifactPaths ?? []).filter(path => path.endsWith('.dmg'));
  if (!notarizing) { log(`macOS key change pending, but no Developer ID credentials: ${images.length} disk image(s) left unnotarized`); return []; }
  for (const image of images) {
   run('xcrun', ['notarytool', 'submit', image, '--key', env.APPLE_API_KEY, '--key-id', env.APPLE_API_KEY_ID, '--issuer', env.APPLE_API_ISSUER, '--wait'], { stdio: 'inherit' });
   run('xcrun', ['stapler', 'staple', image], { stdio: 'inherit' });
   run('xcrun', ['stapler', 'validate', image], { stdio: 'inherit' });
   log(`Notarized and stapled the rotation release disk image ${image}`);
  }
  return [];
 };
}

module.exports = { macRotationPending, createAfterAllArtifactBuild };
