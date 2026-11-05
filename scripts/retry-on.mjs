import { spawn } from 'node:child_process';
import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

/**
 * Runs a command again only when it failed AND its output matches a known outside failure (a proxy hiccup, an Apple
 * service error), so a real build or test failure still fails at once and is never hidden behind a retry.
 *
 *   node scripts/retry-on.mjs --pattern <regex> [--pattern ...] [--attempts 3] [--wait 20] -- <cmd> <args...>
 *
 * The command runs without a shell, so its arguments are not re-parsed. Its output streams live and is kept for matching.
 * The exit code is the command's last. `run`, `sleep` and `log` are for tests.
 */
export async function retryOn({ patterns, attempts, waitMs, run, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), log = () => {} }) {
  let result;
  for (let attempt = 1; attempt <= attempts; attempt++) {
    result = await run();
    if (result.code === 0) return { code: 0, attempts: attempt };
    const matched = patterns.find((p) => p.test(result.output));
    if (matched === undefined) return { code: result.code, attempts: attempt };
    if (attempt === attempts) return { code: result.code, attempts: attempt };
    log(`retry-on: attempt ${attempt} of ${attempts} failed (exit ${result.code}) and matched ${matched}; retrying in ${waitMs / 1000} s`);
    await sleep(waitMs);
  }
  return { code: result.code, attempts };
}

export function parseArgs(argv) {
  const patterns = [];
  let attempts = 3;
  let waitSeconds = 20;
  const sep = argv.indexOf('--');
  const flags = sep === -1 ? argv : argv.slice(0, sep);
  const command = sep === -1 ? [] : argv.slice(sep + 1);
  for (let i = 0; i < flags.length; i += 2) {
    const value = flags[i + 1];
    if (value === undefined) throw new Error(`retry-on: ${flags[i]} needs a value`);
    if (flags[i] === '--pattern') patterns.push(new RegExp(value));
    else if (flags[i] === '--attempts') attempts = Number(value);
    else if (flags[i] === '--wait') waitSeconds = Number(value);
    else throw new Error(`retry-on: unknown argument ${flags[i]}`);
  }
  if (patterns.length === 0) throw new Error('retry-on: at least one --pattern is required');
  if (command.length === 0) throw new Error('retry-on: a command is required after --');
  if (!Number.isInteger(attempts) || attempts < 1) throw new Error('retry-on: --attempts must be a positive integer');
  if (!Number.isFinite(waitSeconds) || waitSeconds < 0) throw new Error('retry-on: --wait must be a non-negative number of seconds');
  return { patterns, attempts, waitMs: waitSeconds * 1000, command };
}

function runCommand([cmd, ...args]) {
  return new Promise((done) => {
    const chunks = [];
    const child = spawn(cmd, args, { stdio: ['inherit', 'pipe', 'pipe'] });
    child.stdout.on('data', (d) => { chunks.push(d); process.stdout.write(d); });
    child.stderr.on('data', (d) => { chunks.push(d); process.stderr.write(d); });
    child.on('error', (e) => { const msg = `retry-on: cannot run ${cmd}: ${e.message}\n`; process.stderr.write(msg); done({ code: 127, output: msg }); });
    child.on('close', (code, signal) => done({ code: code ?? (signal ? 128 : 1), output: Buffer.concat(chunks).toString('utf8') }));
  });
}

if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) {
  let parsed;
  try {
    parsed = parseArgs(process.argv.slice(2));
  } catch (e) {
    console.error(e.message);
    process.exit(2);
  }
  const { code } = await retryOn({ ...parsed, run: () => runCommand(parsed.command), log: (l) => console.error(l) });
  process.exitCode = code;
}
