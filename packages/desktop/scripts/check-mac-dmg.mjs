import { spawnSync } from 'node:child_process';
import { mkdtempSync, readdirSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { macRotationOf } from './feed-format.mjs';
import { isMain } from './is-main.mjs';

/**
 * The macOS CI job's disk image check, run on every build after the final `dist`:
 *
 * - the DMG's app declares main's SUPublicEDKey with SURequireSignedFeed and SUVerifyUpdateBeforeExtraction (so a
 *   rotation release's DMG carries the new key);
 * - while a macOS key change is pending (signing.macRotation), the DMG is the rotation release's update, so it must
 *   pass the checks Sparkle's key-change path makes and Gatekeeper's: a valid, strict Developer ID signature by the
 *   same team as the app inside, a stapled notarization ticket, and spctl acceptance. Without credentials (a fork's
 *   pull request) that part is skipped with a notice; on any other event an unsigned DMG fails the build.
 *
 * `run(command, args)` returns stdout (with stderr for codesign and spctl) and throws on a non-zero exit.
 */
export function checkMacDiskImage({ out, version, trust, env = process.env, run = defaultRun, log = console.log, list = readdirSync }) {
 const name = `dilla-${version}-mac-arm64.dmg`;
 const images = readdirSync(out).filter(file => file.endsWith('.dmg'));
 if (images.length !== 1 || images[0] !== name) throw new Error(`Expected exactly the disk image ${name}, found ${images.join(', ') || 'none'}`);
 const image = join(out, name);
 const rotation = macRotationOf(trust);
 const mount = mkdtempSync(join(env.RUNNER_TEMP || tmpdir(), 'dilla-dmg-'));
 let appTeam;
 // hdiutil reports where it really mounted the image: an image an earlier step left attached is not mounted again at
 // -mountpoint, so the empty requested directory would hide the app (PR #143: "Info.plist does not exist").
 const attached = run('hdiutil', ['attach', '-nobrowse', '-readonly', '-noautoopen', '-plist', '-mountpoint', mount, image]);
 const mounted = /<key>mount-point<\/key>\s*<string>([^<]+)<\/string>/.exec(attached)?.[1] ?? mount;
 try {
  // The image's single app bundle, found rather than assumed, so a layout change fails with what the image holds.
  const entries = list(mounted), apps = entries.filter(entry => entry.endsWith('.app'));
  if (apps.length !== 1) throw new Error(`Expected exactly one .app in ${name}, found: ${entries.join(', ') || 'nothing'}`);
  const app = join(mounted, apps[0]), plist = join(app, 'Contents', 'Info.plist');
  const value = key => run('plutil', ['-extract', key, 'raw', '-o', '-', plist]).trim();
  if (value('SUPublicEDKey') !== trust.sparklePublicKey) throw new Error('The disk image\'s app does not carry main\'s Sparkle key (SUPublicEDKey)');
  for (const key of ['SURequireSignedFeed', 'SUVerifyUpdateBeforeExtraction']) if (value(key) !== 'true') throw new Error(`The disk image's app does not set ${key}`);
  if (rotation) appTeam = /^TeamIdentifier=(.+)$/m.exec(run('codesign', ['-dvv', app]))?.[1];
 } finally {
  run('hdiutil', ['detach', mounted]);
  rmSync(mount, { recursive: true, force: true });
 }
 if (!rotation) { log(`${name}: carries main's Sparkle key; no macOS key change is pending, so the zip is the update.`); return { rotation: false }; }

 const details = (() => { try { return run('codesign', ['-dvv', image]); } catch { return ''; } })();
 if (!/^Authority=Developer ID Application: /m.test(details)) {
  if (env.GITHUB_EVENT_NAME === 'pull_request') { log(`${name}: a macOS key change is pending, but this pull request build has no Developer ID credentials; the rotation disk image is checked on main.`); return { rotation: true, skipped: true }; }
  throw new Error('A macOS key change is pending, so the disk image is the rotation update and must be Developer ID signed');
 }
 const team = /^TeamIdentifier=(.+)$/m.exec(details)?.[1];
 if (!team || team === 'not set' || team !== appTeam) throw new Error('The disk image must be signed by the same Developer ID team as the app (Sparkle checks the team)');
 run('codesign', ['--verify', '--strict', '--verbose=2', image]);
 run('xcrun', ['stapler', 'validate', image]);
 const assessment = run('spctl', ['--assess', '--type', 'open', '--context', 'context:primary-signature', '--verbose=2', image]);
 if (!/source=Notarized Developer ID/.test(assessment)) throw new Error('The rotation disk image is not accepted as notarized Developer ID content');
 log(`${name}: Developer ID signed by team ${team}, notarized and stapled, carrying the new Sparkle key: the rotation update.`);
 return { rotation: true, skipped: false };
}

/** Returns stdout, plus stderr for codesign and spctl (they report there); throws on a non-zero exit. */
function defaultRun(command, args) {
 const result = spawnSync(command, args, { encoding: 'utf8' });
 if (result.error || result.status !== 0) throw new Error(`${command} ${args.join(' ')} failed: ${(result.stderr || result.stdout || result.error?.message || '').trim()}`);
 return command === 'codesign' || command === 'spctl' ? `${result.stdout}${result.stderr}` : result.stdout;
}

if (isMain(import.meta.url)) {
 if (process.argv.length !== 3) throw new Error('Usage: check-mac-dmg.mjs OUT_DIR');
 const desktop = resolve(import.meta.dirname, '..');
 const trust = JSON.parse(readFileSync(join(desktop, 'release-trust.json'), 'utf8'));
 const { version } = JSON.parse(readFileSync(join(desktop, 'package.json'), 'utf8'));
 checkMacDiskImage({ out: resolve(process.argv[2]), version, trust });
}
