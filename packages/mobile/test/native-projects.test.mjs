import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('..', import.meta.url));
const read = path => readFileSync(join(root, path), 'utf8');

test('iOS: bundle id chat.dilla.app, iOS 17, no armv7, microphone and camera usage strings', () => {
  const pbx = read('ios/App/App.xcodeproj/project.pbxproj');
  const targets = [...pbx.matchAll(/IPHONEOS_DEPLOYMENT_TARGET = ([\d.]+);/g)].map(match => match[1]);
  assert.ok(targets.length >= 2);
  for (const target of targets) assert.equal(target, '17.0');
  const ids = [...pbx.matchAll(/PRODUCT_BUNDLE_IDENTIFIER = ([^;]+);/g)].map(match => match[1]);
  assert.ok(ids.length >= 2);
  for (const id of ids) assert.equal(id, 'chat.dilla.app');
  assert.match(read('ios/App/CapApp-SPM/Package.swift'), /\.iOS\(\.v17\)/);
  const plist = read('ios/App/App/Info.plist');
  assert.doesNotMatch(plist, /armv7/);
  assert.match(plist, /<key>NSMicrophoneUsageDescription<\/key>\s*<string>[^<]{10,}<\/string>/);
  assert.match(plist, /<key>NSCameraUsageDescription<\/key>\s*<string>[^<]{10,}<\/string>/);
  assert.match(plist, /<key>UIApplicationSceneManifest<\/key>/);
});

test('iOS: one source for the root view controller (the storyboard), the scene delegate does not build one in code', () => {
  const plist = read('ios/App/App/Info.plist');
  assert.match(plist, /<key>UISceneStoryboardFile<\/key>\s*<string>Main<\/string>/);
  const scene = read('ios/App/App/SceneDelegate.swift');
  assert.doesNotMatch(scene, /rootViewController\s*=/);
  assert.doesNotMatch(scene, /UIWindow\(windowScene/);
  assert.match(read('ios/App/App/Base.lproj/Main.storyboard'), /customClass="CAPBridgeViewController"/);
});

test('iOS: the template gitignore of Capacitor 8.6 is kept (generated config and public/ stay out of git)', () => {
  const ignore = read('ios/.gitignore');
  for (const line of ['App/App/public', 'App/App/capacitor.config.json', 'App/App/config.xml']) assert.ok(ignore.split('\n').includes(line), line);
});

test('Android: application id chat.dilla.app, minSdk 29, the four permissions', () => {
  assert.match(read('android/app/build.gradle'), /applicationId "chat\.dilla\.app"/);
  assert.match(read('android/variables.gradle'), /minSdkVersion = 29\b/);
  const manifest = read('android/app/src/main/AndroidManifest.xml');
  for (const permission of ['INTERNET', 'RECORD_AUDIO', 'MODIFY_AUDIO_SETTINGS', 'POST_NOTIFICATIONS'])
    assert.match(manifest, new RegExp(`<uses-permission android:name="android\\.permission\\.${permission}"`), permission);
  assert.doesNotMatch(manifest, /usesCleartextTraffic="true"/);
  assert.match(manifest, /android:networkSecurityConfig="@xml\/network_security_config"/);
});

test('Android: the release network config trusts system CAs only; user CAs are a debug-only override', () => {
  const release = read('android/app/src/main/res/xml/network_security_config.xml');
  assert.match(release, /<base-config cleartextTrafficPermitted="false">/);
  assert.match(release, /<certificates src="system"\s*\/>/);
  assert.doesNotMatch(release, /src="user"|debug-overrides|domain-config/);
  const debug = read('android/app/src/debug/res/xml/network_security_config.xml');
  assert.match(debug, /<certificates src="user"\s*\/>/);
  assert.match(debug, /cleartextTrafficPermitted="false"/);
  // No other source set (release, a flavor) may carry a network config of its own.
  const sets = readdirSync(join(root, 'android/app/src')).filter(set => existsSync(join(root, 'android/app/src', set, 'res/xml/network_security_config.xml')));
  assert.deepEqual(sets.sort(), ['debug', 'main']);
});

test('iOS: the shared App scheme (CI builds -scheme App) builds the App target of the project', () => {
  const pbx = read('ios/App/App.xcodeproj/project.pbxproj');
  const target = /^\t\t([0-9A-F]{24}) \/\* App \*\/ = \{\n\t\t\tisa = PBXNativeTarget;/m.exec(pbx)?.[1];
  assert.ok(target, 'the App native target');
  const scheme = read('ios/App/App.xcodeproj/xcshareddata/xcschemes/App.xcscheme');
  const refs = [...scheme.matchAll(/BlueprintIdentifier = "([0-9A-F]{24})"\s+BuildableName = "App\.app"\s+BlueprintName = "App"\s+ReferencedContainer = "container:App\.xcodeproj"/g)].map(match => match[1]);
  assert.ok(refs.length >= 2, 'build and launch reference the target');
  for (const ref of refs) assert.equal(ref, target);
  assert.match(scheme, /buildForRunning = "YES"/);
});

// Final review ruling (iOS backup parity): Android sets allowBackup="false", so app data never reaches a backup and a new
// phone signs in with the recovery key. iOS matches it: at launch, before any WebView exists, the app marks the WebView's
// data store (Library/WebKit, Library/Cookies) and Library/Application Support as excluded from iCloud and Finder backup.
test('Android: app data is not backed up (allowBackup="false")', () => {
  assert.match(read('android/app/src/main/AndroidManifest.xml'), /android:allowBackup="false"/);
});

test('iOS: the WebView data store is excluded from backup at launch, before any WebView exists', () => {
  const app = read('ios/App/App/AppDelegate.swift');
  const launch = app.slice(app.indexOf('didFinishLaunchingWithOptions'));
  assert.match(launch.slice(0, launch.indexOf('return true')), /excludeAppDataFromBackup\(\)/);
  assert.match(app, /\.isExcludedFromBackup = true/);
  assert.match(app, /setResourceValues/);
  assert.match(app, /\.libraryDirectory/);
  for (const dir of ['"WebKit"', '"Cookies"', '"Application Support"']) assert.ok(app.includes(dir), dir);
  // The scene delegate (which shows the storyboard's CAPBridgeViewController and so the WebView) does not run first.
  assert.doesNotMatch(read('ios/App/App/SceneDelegate.swift'), /isExcludedFromBackup/);
});
