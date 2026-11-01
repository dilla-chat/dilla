import { spawn } from 'node:child_process';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { combinePerformance } from './performance-report.mjs';
const root = resolve(import.meta.dirname, '../../..');
const output = resolve(process.env.DILLA_PERFORMANCE_OUTPUT ?? resolve(root, 'packages/desktop/test-report'));
await mkdir(output, { recursive: true });
for (const mode of ['browser', 'desktop']) {
 await new Promise((done, reject) => {
  const child = spawn(process.execPath, [resolve(root, 'node_modules/@playwright/test/cli.js'), 'test', '--config', 'playwright.performance.config.ts', '--workers=1'], {
   cwd: resolve(root, 'e2e'), stdio: 'inherit', env: { ...process.env, DILLA_PERFORMANCE_CLIENT: mode, DILLA_PERFORMANCE_OUTPUT: output },
  });
  child.once('error', reject);
  child.once('exit', (code, signal) => code === 0 ? done() : reject(new Error(`${mode} performance measurement failed (${signal ?? code})`)));
 });
}
const target = `${process.platform === 'darwin' ? 'mac' : process.platform === 'win32' ? 'win' : 'linux'}-${process.arch}`;
const records = await Promise.all(['browser', 'desktop'].map(mode => readFile(resolve(output, `performance-${mode}-${target}.json`), 'utf8').then(JSON.parse)));
const report = combinePerformance(...records);
await writeFile(resolve(output, `performance-${target}.json`), JSON.stringify(report, null, 2) + '\n');
console.log('Measured browser/desktop comparison: ' + resolve(output, `performance-${target}.json`));
