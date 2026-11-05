// Proves what a built APK trusts: node scripts/check-apk-network-config.mjs <apk> --release|--debug
// A release APK must trust system CAs only and refuse cleartext; a debug APK may add user CAs (the e2e's dev CA).
// Reads the compiled res/xml/network_security_config through aapt2 (release builds shorten resource paths).
import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { homedir } from 'node:os';

const [apk, mode] = process.argv.slice(2);
if (!apk || !['--release', '--debug'].includes(mode)) { console.error('usage: check-apk-network-config.mjs <apk> --release|--debug'); process.exit(2); }
const sdk = process.env.ANDROID_HOME ?? process.env.ANDROID_SDK_ROOT ?? join(homedir(), 'Android/Sdk');
const tools = join(sdk, 'build-tools');
const version = existsSync(tools) ? readdirSync(tools).sort().at(-1) : undefined;
if (!version) throw new Error(`no Android build-tools under ${tools}`);
const aapt2 = join(tools, version, 'aapt2');
const run = args => execFileSync(aapt2, args, { encoding: 'utf8' });

const resources = run(['dump', 'resources', apk]);
const entry = /xml\/network_security_config\n\s+\(\) \(file\) (\S+) type=XML/.exec(resources);
if (!entry) throw new Error('the APK has no xml/network_security_config');
const tree = run(['dump', 'xmltree', '--file', entry[1], apk]);
const sources = [...tree.matchAll(/A: src="(\w+)"/g)].map(match => match[1]).sort();
const cleartext = [...tree.matchAll(/cleartextTrafficPermitted=(\w+)/g)].map(match => match[1]);
const manifest = run(['dump', 'xmltree', '--file', 'AndroidManifest.xml', apk]);

const problems = [];
if (cleartext.length === 0 || cleartext.some(value => value !== 'false')) problems.push(`cleartextTrafficPermitted ${JSON.stringify(cleartext)}`);
if (/usesCleartextTraffic\([^)]*\)=true/.test(manifest)) problems.push('usesCleartextTraffic=true in the manifest');
if (/domain-config|debug-overrides/.test(tree)) problems.push('unexpected domain-config or debug-overrides');
const expected = mode === '--release' ? ['system'] : ['system', 'user'];
if (JSON.stringify(sources) !== JSON.stringify(expected)) problems.push(`trust anchors ${JSON.stringify(sources)}, expected ${JSON.stringify(expected)}`);
if (problems.length) { console.error(`${apk}: ${problems.join('; ')}`); process.exit(1); }
console.log(`${apk}: ${mode.slice(2)} network config OK (trust ${sources.join('+')}, no cleartext)`);
