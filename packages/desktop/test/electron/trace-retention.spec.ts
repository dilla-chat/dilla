import { test, expect } from '@playwright/test';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { inflateRawSync } from 'node:zlib';

const exec = promisify(execFile);
const desktop = resolve(dirname(fileURLToPath(import.meta.url)), '../..');

// Read standard ZIP entries without depending on Playwright's private ZIP API.
function zipText(zip: Buffer): string {
 const entries: string[] = [];
 for (let offset = 0; offset <= zip.length - 46; offset++) {
  if (zip.readUInt32LE(offset) !== 0x02014b50) continue;
  const method = zip.readUInt16LE(offset + 10), size = zip.readUInt32LE(offset + 20);
  const nameLength = zip.readUInt16LE(offset + 28), extraLength = zip.readUInt16LE(offset + 30), commentLength = zip.readUInt16LE(offset + 32);
  const name = zip.subarray(offset + 46, offset + 46 + nameLength).toString();
  if (name.endsWith('.trace')) {
   const local = zip.readUInt32LE(offset + 42);
   const start = local + 30 + zip.readUInt16LE(local + 26) + zip.readUInt16LE(local + 28);
   const bytes = zip.subarray(start, start + size);
   expect([0, 8]).toContain(method);
   entries.push((method === 8 ? inflateRawSync(bytes) : bytes).toString());
  }
  offset += 45 + nameLength + extraLength + commentLength;
 }
 return entries.join('\n');
}

test('retains both real peer renderer traces when body finally closes before failure status', async ({}) => {
 test.setTimeout(120_000);
 const directory = await mkdtemp(join(tmpdir(), 'dilla-trace-retention-'));
 try {
  const report = join(directory, 'report.json');
  const config = join(directory, 'playwright.config.mjs');
  await writeFile(config, `export default ${JSON.stringify({
   testDir: join(desktop, 'test/support'), testMatch: 'trace-retention.fixture.ts',
   workers: 1, retries: 0, timeout: 45_000, outputDir: join(directory, 'results'),
   reporter: [['json', { outputFile: report }]],
   projects: [{ name: 'retain', use: { trace: 'retain-on-failure' } }, { name: 'off', use: { trace: 'off' } }],
  })};`);
  const cli = createRequire(import.meta.url).resolve('@playwright/test/cli');
  const result = await exec(process.execPath, [cli, 'test', '--config', config], {
   cwd: desktop, env: { ...process.env, CI: 'false' }, timeout: 110_000,
  }).then(() => ({ code: 0 }), error => ({ code: error.code }));
  expect(result.code).toBe(1); // The controlled fixture deliberately fails.
  const summary = JSON.parse(await readFile(report, 'utf8'));
  const tests = summary.suites.flatMap((suite: any) => suite.specs.flatMap((spec: any) => spec.tests));
  expect(tests).toHaveLength(2);
  for (const inner of tests) {
   const outcome = inner.results[0];
   expect(outcome.status).toBe('failed');
   expect(outcome.error.message).toContain('intentional failure before Electron finally cleanup');
   const attachments = outcome.attachments.filter((attachment: any) => attachment.name === 'Electron renderer trace');
   expect(attachments).toHaveLength(inner.projectName === 'off' ? 0 : 2);
   if (inner.projectName === 'off') continue;
   expect(new Set(attachments.map((attachment: any) => attachment.path)).size).toBe(2);
   const texts = await Promise.all(attachments.map(async (attachment: any) => zipText(await readFile(attachment.path))));
   for (const peer of ['alice', 'bob']) {
    const text = texts.find(text => text.includes(`electron-trace-${peer}`));
    expect(text, `Console event from ${peer}'s renderer`).toBeDefined();
    expect(text).toContain('frame-snapshot');
    expect(text).toContain(`Trace peer ${peer}`);
    expect(text).toMatch(/"(?:method|apiName)":"(?:click|locator.click)"/);
   }
  }
 } finally { await rm(directory, { recursive: true, force: true }); }
});
