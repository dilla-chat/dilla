// Ubuntu CI's AppArmor policy restricts user namespaces for downloaded browser
// builds. Install that pinned browser's own SUID helper under Chromium's expected
// adjacent filename. Keep both the OS policy and Chromium sandbox enabled.
// https://chromium.googlesource.com/chromium/src/+/main/docs/linux/suid_sandbox_development.md
import { chromium } from '@playwright/test';
import { dirname, join } from 'node:path';
import { lstat } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
if (process.platform !== 'linux' || process.env.CI !== 'true') throw new Error('Browser sandbox preparation is restricted to Linux CI');
const directory = dirname(chromium.executablePath());
const source = join(directory, 'chrome_sandbox'), destination = join(directory, 'chrome-sandbox');
const file = await lstat(source);
if (!file.isFile() || file.isSymbolicLink()) throw new Error('Missing pinned Chromium sandbox helper');
const installed = spawnSync('sudo', ['install', '-o', 'root', '-g', 'root', '-m', '4755', '--', source, destination], { stdio: 'inherit' });
if (installed.error) throw installed.error;
if (installed.status !== 0) throw new Error('Chromium sandbox helper installation failed');
const ready = await lstat(destination);
if (!ready.isFile() || ready.uid !== 0 || (ready.mode & 0o7777) !== 0o4755) throw new Error('Chromium sandbox helper is not configured correctly');
