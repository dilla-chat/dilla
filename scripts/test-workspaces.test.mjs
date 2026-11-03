import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, realpathSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { test } from 'node:test';

const runner = resolve(import.meta.dirname, 'test-workspaces.mjs');
const adjacentNpm = join(dirname(process.execPath), 'node_modules/npm/bin/npm-cli.js');
const npmCLI = process.env.npm_execpath ?? (existsSync(adjacentNpm) ? adjacentNpm : realpathSync(join(dirname(process.execPath), 'npm')));

function fixture(t, failure) {
  const root = mkdtempSync(join(tmpdir(), 'dilla workspace tests '));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const log = join(root, 'events.jsonl');
  const recorder = join(root, 'record.cjs');
  writeFileSync(recorder, `require('node:fs').appendFileSync(${JSON.stringify(log)}, JSON.stringify(process.argv[2]) + '\\n'); process.exit(process.argv[2] === ${JSON.stringify(failure)} ? 23 : 0);\n`);
  const command = event => `${JSON.stringify(process.execPath)} ${JSON.stringify(recorder)} ${JSON.stringify(event)}`;
  // Deliberately not alphabetical: npm's workspace declaration order is the contract.
  const order = ['third', 'missing', 'first', 'empty', 'second'];
  writeFileSync(join(root, 'package.json'), JSON.stringify({ name: 'fixture-root', private: true,
    workspaces: order.map(name => `packages/${name}`), scripts: { test: `node ${JSON.stringify(runner)}` } }));
  for (const name of order) {
    const directory = join(root, 'packages', name);
    mkdirSync(directory, { recursive: true });
    const scripts = name === 'missing' ? { pretest: command('missing:must-not-run') }
      : { pretest: command(`${name}:pre`), test: name === 'empty' ? '' : command(`${name}:test`), posttest: command(`${name}:post`) };
    writeFileSync(join(directory, 'package.json'), JSON.stringify({ name: `@fixture/${name}`, version: '1.0.0', scripts }));
  }
  const result = spawnSync(process.execPath, [npmCLI, 'test'], { cwd: root, encoding: 'utf8', timeout: 30_000,
    env: { ...process.env, npm_config_workspaces: '', npm_config_workspace: '', npm_config_include_workspace_root: 'false' } });
  const events = existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse) : [];
  return { result, events };
}

test('a failed workspace exits immediately without starting later workspaces or posttest', t => {
  const { result, events } = fixture(t, 'first:test');
  assert.equal(result.status, 23, result.stderr);
  assert.deepEqual(events, ['third:pre', 'third:test', 'third:post', 'first:pre', 'first:test']);
});

test('successful workspaces retain npm order and hooks, skipping only absent test scripts', t => {
  const { result, events } = fixture(t, 'never');
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(events, ['third:pre', 'third:test', 'third:post', 'first:pre', 'first:test', 'first:post',
    'empty:pre', 'empty:post', 'second:pre', 'second:test', 'second:post']);
});
