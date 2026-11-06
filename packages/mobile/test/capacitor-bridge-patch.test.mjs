import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

// Capacitor Android is compiled from node_modules (android/capacitor.settings.gradle), so the installed source is
// what ships. patches/@capacitor+android+<version>.patch (applied by the root postinstall) makes MessageHandler set
// the reply proxy before it dispatches the plugin call. Stock Capacitor dispatches first, and after an in-app
// navigation a fast plugin's answer can go to the previous page's proxy: the new page's first call never resolves
// and the client boots to a blank page.
const repo = fileURLToPath(new URL('../../..', import.meta.url));
const pkgJson = createRequire(import.meta.url).resolve('@capacitor/android/package.json');
const pkgDir = dirname(pkgJson);
const { version } = JSON.parse(readFileSync(pkgJson, 'utf8'));
const source = join(pkgDir, 'capacitor/src/main/java/com/getcapacitor/MessageHandler.java');

test('the installed Capacitor Android has a patch for its exact version', () => {
  const patch = join(repo, 'patches', `@capacitor+android+${version}.patch`);
  assert.ok(existsSync(patch), `no ${patch}: re-create the MessageHandler patch for @capacitor/android ${version} (npx patch-package @capacitor/android --exclude '^package\\.json$|/build/|/\\.gradle/')`);
});

test('MessageHandler sets the reply proxy before it dispatches the plugin call', () => {
  const java = readFileSync(source, 'utf8');
  const listener = java.match(/WebMessageListener capListener = \(view, message, sourceOrigin, isMainFrame, replyProxy\) -> \{\s*if \(isMainFrame\) \{([\s\S]*?)\} else \{/);
  assert.ok(listener, `${source}: the WebMessageListener changed shape; re-check the reply-proxy race and update this test and the patch`);
  const body = listener[1];
  const assign = body.indexOf('javaScriptReplyProxy = replyProxy;');
  const dispatch = body.indexOf('postMessage(message.getData());');
  assert.ok(assign >= 0 && dispatch >= 0, `${source}: the main-frame branch no longer assigns the proxy and dispatches; re-check the race`);
  assert.equal(body.split('javaScriptReplyProxy = replyProxy;').length, 2, 'the proxy is assigned exactly once');
  assert.ok(assign < dispatch, `${source}: the reply proxy is assigned after postMessage (stock Capacitor); run npm install so the root postinstall applies patches/`);
});
