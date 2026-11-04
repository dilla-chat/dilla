#!/usr/bin/env node
/**
 * `npm run ci:local`: .github/workflows/ci.yml run on this machine before a push. The jobs, their steps, the needs graph
 * and the area gates come from ci.yml itself (scripts/ci-local-plan.mjs maps each step); GitHub-only steps become local
 * equivalents (artifacts are copied through target/ci-local/artifacts, tool installs are prerequisite checks, Postgres
 * is a throwaway container), and every job logs to target/ci-local/<job>.log. docs/dev/ci-local.md is the manual.
 */
import { spawn, spawnSync } from 'node:child_process';
import {
  closeSync, copyFileSync, cpSync, existsSync, mkdirSync, openSync, readFileSync, readdirSync, renameSync, rmSync, statSync,
  writeFileSync, writeSync,
} from 'node:fs';
import { availableParallelism, tmpdir } from 'node:os';
import { createServer } from 'node:net';
import { basename, dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { computeAreas } from './ci-areas.mjs';
import {
  buildCleanTree, busyPorts, lockDir, lockDirPath, recoverSnapshots, releaseLock, restoreSnapshot, scrubEnv, takeSnapshot, tryLock,
} from './ci-local-fs.mjs';
import { randomBytes } from 'node:crypto';
import {
  DEFAULT_BASE, E2E_PORT_SPAN, EXTRA_JOBS, JOB_MAP, MAX_BLOCK, UI_PORT_SPAN, blockPorts, classifyStep, e2eBlockEnv, evalIf, loadWorkflow,
  localMirrorProblems, mapService, moveUiPorts, selectJobs, skipReason, substitute,
} from './ci-local-plan.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const OUT = join(ROOT, 'target', 'ci-local');
const RUNNER_TEMP = join(OUT, 'runner-temp');
const ARTIFACTS = join(OUT, 'artifacts');
const LOCK_DIR = lockDirPath();
const SNAPSHOTS = join(OUT, 'snapshots');
const EVERYTHING = { go: true, core: true, web: true, e2e: true, text: true, code: true };

const USAGE = `usage: npm run ci:local -- [options]
  --only <job,...>   run these jobs (ci.yml names, or a matrix leg such as a native target, or codeql) and what they need
  --no-deps          with --only: do not run the needed jobs; reuse their artifacts from an earlier run
  --changed          the jobs the ci-areas gates run for this branch's diff against origin/main (uncommitted files included)
  --stress           run the Go and browser jobs on a quarter of the CPUs beside a busy-loop load, like a loaded runner
  --keep-going       run every job even after one failed (default: stop at the first failure)
  --base <n>         the first port block (default ${DEFAULT_BASE}; 0..${MAX_BLOCK}): block k moves the browser suites by k*1000
  --jobs <n>         the CPU budget the parallel jobs share (default: this machine's ${availableParallelism()} CPUs)
  --e2e-jobs <n>     browser jobs that run at once (default 3)
  --dry-run          print the plan (jobs, needs, how each step maps) and exit
  --doctor           check the prerequisites of the selected jobs and exit`;

export function parseArgs(argv) {
  const args = { only: null, deps: true, changed: false, stress: false, keepGoing: false, base: DEFAULT_BASE, jobs: availableParallelism(), e2eJobs: 3, dryRun: false, doctor: false, help: false };
  const int = (flag, v, min, max) => {
    const n = Number(v);
    if (!Number.isInteger(n) || n < min || n > max) throw new Error(`${flag} takes an integer from ${min} to ${max}`);
    return n;
  };
  for (let i = 0; i < argv.length; i++) {
    const [flag, inline] = argv[i].split(/=(.*)/s);
    const next = () => {
      if (inline !== undefined) return inline;
      if (i + 1 >= argv.length) throw new Error(`${flag} needs a value`);
      return argv[++i];
    };
    switch (flag) {
      case '--only': args.only = next().split(',').map((s) => s.trim()).filter(Boolean); break;
      case '--no-deps': args.deps = false; break;
      case '--changed': args.changed = true; break;
      case '--stress': args.stress = true; break;
      case '--keep-going': args.keepGoing = true; break;
      case '--base': args.base = int('--base', next(), 0, MAX_BLOCK); break;
      case '--jobs': args.jobs = int('--jobs', next(), 1, 1024); break;
      case '--e2e-jobs': args.e2eJobs = int('--e2e-jobs', next(), 1, MAX_BLOCK + 1); break;
      case '--dry-run': args.dryRun = true; break;
      case '--doctor': args.doctor = true; break;
      case '-h': case '--help': args.help = true; break;
      default: throw new Error(`unknown option ${argv[i]}\n${USAGE}`);
    }
  }
  if (args.only !== null && args.changed) throw new Error('--only and --changed do not combine');
  if (!args.deps && args.only === null) throw new Error('--no-deps needs --only');
  if (args.only !== null && args.only.length === 0) throw new Error('--only needs at least one job');
  return args;
}

// ---------------------------------------------------------------- the diff (--changed)

const git = (gitArgs) => spawnSync('git', gitArgs, { cwd: ROOT, encoding: 'utf8', maxBuffer: 64 << 20 });

/** The merge base the branch is compared with: origin/main, else main; null when neither exists. */
function mergeBase() {
  for (const ref of ['origin/main', 'main']) {
    const r = git(['merge-base', 'HEAD', ref]);
    if (r.status === 0) return { ref, sha: r.stdout.trim() };
  }
  return null;
}

/** The files that differ from `base`: committed, staged, unstaged and untracked (a push may carry any of them). */
function changedFiles(base) {
  const z = (r) => {
    if (r.status !== 0) throw new Error(r.stderr);
    return r.stdout.split('\0').filter(Boolean);
  };
  return [...new Set([...z(git(['diff', '--name-only', '--no-renames', '-z', base])), ...z(git(['ls-files', '--others', '--exclude-standard', '-z']))])];
}

/** The areas of a diff, as the `changes` job computes them for a pull request (fail safe to everything). */
export function changedOutputs(baseSha, files) {
  return computeAreas({ eventName: 'pull_request', baseSha, diff: () => files });
}

// ---------------------------------------------------------------- prerequisites

const TOOL_HINTS = {
  bash: 'bash',
  git: 'git',
  node: 'Node.js of .nvmrc (nvm install / fnm use)',
  npm: 'npm (ships with Node.js)',
  npx: 'npx (ships with Node.js)',
  go: "Go of go.mod's go line: https://go.dev/dl (GOTOOLCHAIN=auto fetches it)",
  cargo: 'rustup: https://rustup.rs (rust-toolchain.toml pins the toolchain, clippy, rustfmt and the wasm32 targets)',
  rustup: 'rustup: https://rustup.rs',
  jq: 'jq (pacman -S jq / apt install jq)',
  curl: 'curl (install-ci-tool.sh downloads the pinned CI tools with it)',
  sha256sum: 'coreutils sha256sum',
  tar: 'tar',
  prlimit: 'util-linux prlimit',
  taskset: 'util-linux taskset (for --stress)',
  pactl: 'pactl (libpulse) and a running PipeWire-Pulse or PulseAudio server: the browser-media suites need one',
  container: 'podman or docker, for go-postgres (a throwaway postgres container)',
};

const has = (bin) => spawnSync('bash', ['-c', `command -v ${bin}`], { encoding: 'utf8' }).status === 0;

/** The tools the selected jobs' steps call. */
function requiredTools(jobs, args) {
  const need = new Set(['bash', 'git', 'node']);
  const words = { npm: /\bnpm\b/, npx: /\bnpx\b/, go: /(^|[\s(;&|])go\s/, cargo: /\bcargo\b/, rustup: /\brustup\b/, jq: /\bjq\b/, prlimit: /\bprlimit\b/ };
  for (const job of jobs) {
    if (Object.keys(job.services ?? {}).length > 0) need.add('container');
    for (const step of job.steps ?? []) {
      let m;
      try { m = classifyStep(step, job); } catch { continue; }
      if (m.kind === 'prereq' && m.tool === 'audio') need.add('pactl');
      if (m.kind === 'prereq' && m.tool === 'go') need.add('go');
      if (m.kind !== 'run') continue;
      for (const [tool, re] of Object.entries(words)) if (re.test(m.script)) need.add(tool);
      if (/install-ci-tool\.sh/.test(m.script)) for (const t of ['curl', 'sha256sum', 'tar']) need.add(t);
    }
  }
  if (args.stress) need.add('taskset');
  return [...need];
}

function missingTools(tools) {
  return tools.filter((t) => (t === 'container' ? !(has('podman') || has('docker')) : !has(t))).map((t) => `${t}: install ${TOOL_HINTS[t] ?? t}`);
}

function toolVersion(bin, flag = '--version') {
  const r = spawnSync(bin, [flag], { cwd: ROOT, encoding: 'utf8' });
  return r.status === 0 ? `${r.stdout}${r.stderr}` : null;
}

/** setup-node / setup-go / the clang step / the audio server: checked here, never installed. Returns { ok, text, env? }. */
function checkPrereq(m, job) {
  if (m.tool === 'node') {
    const want = readFileSync(join(ROOT, m.versionFile ?? '.nvmrc'), 'utf8').trim().replace(/^v/, '');
    const have = process.versions.node;
    const ok = have.split('.')[0] === want.split('.')[0] && (want.split('.').length === 1 || have.startsWith(want));
    return { ok, text: ok ? `node ${have} matches ${m.versionFile} (${want})` : `node ${have} does not match ${m.versionFile} (${want}): install ${TOOL_HINTS.node}` };
  }
  if (m.tool === 'go') {
    const want = /^go (\S+)$/m.exec(readFileSync(join(ROOT, m.versionFile ?? 'go.mod'), 'utf8'))?.[1];
    const r = spawnSync('go', ['env', 'GOVERSION'], { cwd: ROOT, encoding: 'utf8' });
    if (r.status !== 0) return { ok: false, text: `go is not runnable: install ${TOOL_HINTS.go}` };
    const have = r.stdout.trim().replace(/^go/, '');
    const cmp = compareVersions(have, want);
    if (cmp < 0) return { ok: false, text: `go ${have} is older than go.mod's ${want}: install ${TOOL_HINTS.go}` };
    return { ok: true, text: cmp === 0 ? `go ${have} matches go.mod` : `go ${have} is newer than go.mod's ${want} (CI uses ${want}); results may differ` };
  }
  if (m.tool === 'llvm') {
    const env = {};
    const notes = [];
    for (const [variable, local] of [['CC_wasm32_unknown_unknown', 'clang'], ['AR_wasm32_unknown_unknown', 'llvm-ar']]) {
      const ciPath = job.env[variable];
      if (ciPath === undefined) continue;
      const line = (bin) => {
        const v = toolVersion(bin);
        return v !== null && new RegExp(`version ${m.version.replace('.', '\\.')}\\.`).test(v) ? v.split('\n').find((l) => /version/.test(l)).trim() : null;
      };
      if (existsSync(ciPath) && line(ciPath)) {
        notes.push(`${variable}=${ciPath} (${line(ciPath)})`);
        continue;
      }
      const found = spawnSync('bash', ['-c', `command -v ${local}`], { encoding: 'utf8' }).stdout.trim();
      if (found && line(found)) {
        env[variable] = found;
        notes.push(`${variable}=${found} stands in for ${ciPath} (${line(found)})`);
        continue;
      }
      return { ok: false, text: `${ciPath} is missing and no ${local} on PATH is LLVM ${m.version}.x: install clang and llvm ${m.version} (Arch: pacman -S clang llvm; Debian/Ubuntu: apt.llvm.org clang-${m.version.split('.')[0]} llvm-${m.version.split('.')[0]})` };
    }
    return { ok: true, text: notes.join('\n'), env };
  }
  if (m.tool === 'gtk') {
    const r = spawnSync('pkg-config', ['--exists', 'gtk+-3.0'], { encoding: 'utf8' });
    return r.status === 0 ? { ok: true, text: 'GTK3 headers found by pkg-config' } : { ok: false, text: 'pkg-config cannot find gtk+-3.0: install the GTK3 headers and pkg-config (Arch: pacman -S gtk3 pkgconf; Debian/Ubuntu: apt install libgtk-3-dev pkg-config)' };
  }
  if (m.tool === 'bsdtar') {
    return has('bsdtar') ? { ok: true, text: 'bsdtar is on PATH' } : { ok: false, text: 'no bsdtar: install libarchive (Arch: pacman -S libarchive; Debian/Ubuntu: apt install libarchive-tools)' };
  }
  if (m.tool === 'audio') {
    if (!has('pactl')) return { ok: false, text: `no pactl: install ${TOOL_HINTS.pactl}` };
    const r = spawnSync('pactl', ['info'], { encoding: 'utf8' });
    return r.status === 0
      ? { ok: true, text: `local audio server: ${(/Server Name: (.*)/.exec(r.stdout)?.[1] ?? 'running').trim()}` }
      : { ok: false, text: `pactl info failed: start an audio server (systemctl --user start pipewire-pulse, or pulseaudio --start)\n${r.stderr}` };
  }
  return { ok: false, text: `unknown prerequisite ${m.tool}` };
}

function compareVersions(a, b) {
  const pa = String(a).split(/[.-]/).map((x) => Number.parseInt(x, 10) || 0);
  const pb = String(b).split(/[.-]/).map((x) => Number.parseInt(x, 10) || 0);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    if ((pa[i] ?? 0) !== (pb[i] ?? 0)) return (pa[i] ?? 0) < (pb[i] ?? 0) ? -1 : 1;
  }
  return 0;
}

// ---------------------------------------------------------------- locks across lanes

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// ---------------------------------------------------------------- the run

const fmt = (ms) => {
  const s = Math.round(ms / 1000);
  return s >= 3600 ? `${Math.floor(s / 3600)}h${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m` : `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s`;
};

class Runner {
  constructor(args, jobs, selection, outputs, workflowEnv) {
    this.args = args;
    this.jobs = new Map(jobs.map((j) => [j.id, j]));
    this.allJobs = jobs;
    this.selection = selection;
    this.outputs = outputs;
    this.workflowEnv = workflowEnv;
    this.state = new Map(selection.map((id) => [id, { status: 'pending', reason: '', ms: 0 }]));
    this.running = new Map();
    this.children = new Set();
    this.memo = new Map();
    this.locks = new Set();
    this.machineLocks = new Set();
    this.blocks = new Map();
    this.containers = new Set();
    this.snapshots = new Set();
    this.groups = new Set();
    this.extraPath = [];
    this.loadGens = [];
    this.stopping = false;
    this.t0 = Date.now();
    this.stressCpus = null;
  }

  log(line) {
    console.log(`[${fmt(Date.now() - this.t0).padStart(6)}] ${line}`);
  }

  info(id) {
    return id in EXTRA_JOBS ? { kind: 'extra', ...EXTRA_JOBS[id] } : { kind: 'steps', ...(JOB_MAP[this.jobs.get(id).name] ?? {}) };
  }

  baseEnv(job) {
    const env = {};
    Object.assign(env, scrubEnv(process.env));
    const ctx = { root: ROOT, runnerTemp: RUNNER_TEMP, jobName: job.name, matrix: job.matrix };
    Object.assign(env, {
      CI: 'true', GITHUB_WORKSPACE: ROOT, RUNNER_TEMP, RUNNER_OS: 'Linux', RUNNER_ARCH: process.arch === 'arm64' ? 'ARM64' : 'X64', GITHUB_JOB: job.name,
    });
    for (const [k, v] of Object.entries(this.workflowEnv ?? {})) env[k] = substitute(v, ctx);
    for (const [k, v] of Object.entries(job.env ?? {})) env[k] = substitute(v, ctx);
    env.PATH = [...this.extraPath, env.PATH].join(':');
    return env;
  }

  // -------------------------------------------------------------- scheduling

  depsOf(id) {
    if (id in EXTRA_JOBS) return [];
    const job = this.jobs.get(id);
    return job.needs.flatMap((name) => this.allJobs.filter((j) => j.name === name).map((j) => j.id)).filter((d) => this.state.has(d));
  }

  canStart(id) {
    const info = this.info(id);
    const usedWeight = [...this.running.keys()].reduce((sum, r) => sum + (this.info(r).weight ?? 1), 0);
    if ([...this.running.keys()].some((r) => this.info(r).exclusive)) return false;
    if (info.exclusive && this.running.size > 0) return false;
    if (this.running.size > 0 && usedWeight + (info.weight ?? 1) > this.args.jobs) return false;
    if ((info.locks ?? []).some((l) => this.locks.has(l))) return false;
    if (info.ports === 'fixed' && this.locks.has('fixed-ports')) return false;
    if (info.ports === 'block' && [...this.running.keys()].filter((r) => this.info(r).ports === 'block').length >= this.args.e2eJobs) return false;
    return true;
  }

  pump() {
    for (const id of this.stopping ? [] : this.selection) {
      const st = this.state.get(id);
      if (st.status !== 'pending') continue;
      const deps = this.depsOf(id);
      const info = this.info(id);
      if (info.kind === 'aggregate') {
        if (deps.every((d) => !['pending', 'running'].includes(this.state.get(d).status))) this.aggregate(id, deps);
        continue;
      }
      const bad = deps.find((d) => ['fail', 'cancelled', 'skipped'].includes(this.state.get(d).status));
      if (bad) {
        this.finish(id, 'skipped', `needs ${bad} (${this.state.get(bad).status})`);
        continue;
      }
      if (!deps.every((d) => this.state.get(d).status === 'pass')) continue;
      if (!this.canStart(id)) continue;
      this.start(id);
    }
    if (this.running.size === 0 && [...this.state.values()].every((s) => s.status !== 'pending')) this.done?.();
  }

  start(id) {
    const info = this.info(id);
    for (const l of info.locks ?? []) this.locks.add(l);
    if (info.ports === 'fixed') this.locks.add('fixed-ports');
    const st = this.state.get(id);
    st.status = 'running';
    st.started = Date.now();
    const promise = this.runJob(id).then(
      (r) => r,
      (e) => ({ status: 'fail', reason: e.stack ?? String(e) }),
    ).then(({ status, reason }) => {
      for (const l of info.locks ?? []) this.locks.delete(l);
      if (info.ports === 'fixed') this.locks.delete('fixed-ports');
      this.running.delete(id);
      this.finish(id, this.stopping && status !== 'pass' && st.cancelled ? 'cancelled' : status, reason);
      if (status === 'fail' && !this.args.keepGoing && !this.stopping) this.failFast(id);
      this.pump();
    });
    this.running.set(id, promise);
    this.log(`start  ${id}`);
  }

  finish(id, status, reason = '') {
    const st = this.state.get(id);
    st.status = status;
    st.reason = reason;
    st.ms = st.started ? Date.now() - st.started : 0;
    const logPath = relative(ROOT, join(OUT, `${id}.log`));
    const label = { pass: 'PASS', fail: 'FAIL', skipped: 'SKIP', cancelled: 'CANCEL' }[status];
    this.log(`${label.padEnd(6)} ${id}${st.started ? ` ${fmt(st.ms)}` : ''}${reason ? `  (${reason.split('\n')[0]})` : ''}${status === 'fail' ? `  log: ${logPath}` : ''}`);
    if (status === 'fail' && existsSync(join(OUT, `${id}.log`))) {
      const tail = readFileSync(join(OUT, `${id}.log`), 'utf8').trimEnd().split('\n').slice(-25);
      console.log(tail.map((l) => `    | ${l}`).join('\n'));
    }
  }

  aggregate(id, deps) {
    const failed = deps.filter((d) => ['fail', 'cancelled'].includes(this.state.get(d).status));
    this.finish(id, failed.length === 0 ? 'pass' : 'fail', failed.length === 0 ? 'every needed job passed or was skipped' : `failed: ${failed.join(', ')}`);
  }

  failFast(id) {
    this.stopping = true;
    this.log(`fail-fast: ${id} failed; cancelling the running jobs (--keep-going runs on)`);
    for (const rid of this.running.keys()) this.state.get(rid).cancelled = true;
    this.killAll();
    for (const [pid, st] of this.state) if (st.status === 'pending') this.finish(pid, 'cancelled', `not started: ${id} failed first`);
  }

  /** Resolves once every process group a step started is gone, SIGKILLing what is left after `graceMs`. */
  async waitChildren(graceMs = 10_000) {
    const alive = () => [...this.groups].filter((pgid) => {
      try { process.kill(-pgid, 0); return true; } catch { this.groups.delete(pgid); return false; }
    });
    const deadline = Date.now() + graceMs;
    while (alive().length > 0 && Date.now() < deadline) await sleep(200);
    for (const pgid of alive()) { try { process.kill(-pgid, 'SIGKILL'); } catch { /* gone */ } }
    const hard = Date.now() + 5_000;
    while (alive().length > 0 && Date.now() < hard) await sleep(100);
  }

  killAll() {
    for (const child of this.children) {
      try { process.kill(-child.pid, 'SIGTERM'); } catch { /* gone */ }
      setTimeout(() => { try { process.kill(-child.pid, 'SIGKILL'); } catch { /* gone */ } }, 10_000).unref();
    }
  }

  async run() {
    const finished = new Promise((r) => { this.done = r; });
    this.heartbeat = setInterval(() => {
      if (this.running.size > 0) this.log(`running: ${[...this.running.keys()].map((id) => `${id} ${fmt(Date.now() - this.state.get(id).started)}`).join(', ')}`);
    }, 120_000);
    this.pump();
    await finished;
    clearInterval(this.heartbeat);
  }

  // -------------------------------------------------------------- one job

  async runJob(id) {
    mkdirSync(OUT, { recursive: true });
    const logPath = join(OUT, `${id}.log`);
    const fd = openSync(logPath, 'w');
    const write = (s) => writeSync(fd, `${s}\n`);
    const cleanups = [];
    try {
      if (id in EXTRA_JOBS) return await this.runCodeql(id, write, fd);
      const job = this.jobs.get(id);
      const info = this.info(id);
      write(`ci-local: job ${id} (ci.yml job "${job.name}") in ${ROOT}`);
      if (info.kind === 'areas') {
        write(`changes: ${Object.entries(this.outputs).map(([k, v]) => `${k}=${v}`).join(' ')}`);
        write(this.args.changed ? `(computed by scripts/ci-areas.mjs for the diff against ${this.base ?? 'nothing: everything on'})` : '(a full run: every area on, as for a push to main)');
        return { status: 'pass', reason: '' };
      }
      const env = this.baseEnv(job);
      const rewrites = [];
      // Ports: a block of its own for the browser suites and the ui job, one lock (across this user's lanes) for the fixed-port suites.
      if (info.ports === 'block' || info.ports === 'ui') {
        const k = await this.acquireBlock(id, write, info.ports);
        cleanups.push(() => this.releaseBlock(k));
        if (info.ports === 'block') Object.assign(env, e2eBlockEnv(k, id, ROOT, join(tmpdir(), 'dilla-ci-local')));
        const { uiOffset } = blockPorts(k);
        if (info.ports === 'ui') rewrites.push((s) => moveUiPorts(s, uiOffset));
        write(`ports: block ${k} (${info.ports === 'block' ? `DILLA_E2E_PORT_OFFSET=${env.DILLA_E2E_PORT_OFFSET}, results in ${relative(ROOT, env.DILLA_E2E_OUTPUT_DIR)}` : `Storybook ports +${uiOffset}`})`);
      }
      if (info.ports === 'fixed') {
        const lock = await this.waitMachineLock('fixed-e2e-ports', id, write);
        cleanups.push(() => this.releaseMachineLock(lock));
      }
      if (info.snapshot) {
        for (const p of info.snapshot) {
          takeSnapshot(ROOT, SNAPSHOTS, p);
          this.snapshots.add(p);
        }
        cleanups.push(() => { for (const p of info.snapshot) this.restore(p); });
      }
      for (const [name, body] of Object.entries(job.services)) {
        const svc = mapService(name, body);
        const started = await this.startService(id, svc, write, cleanups);
        for (const p of svc.ports) rewrites.push((s) => s.replaceAll(`127.0.0.1:${p.host}`, `127.0.0.1:${started.ports[p.host]}`).replaceAll(`localhost:${p.host}`, `localhost:${started.ports[p.host]}`));
      }
      const stress = this.stressCpus !== null && ['go', 'e2e'].includes(info.group);
      if (stress) write(`stress: pinned to CPUs ${this.stressCpus} beside the load generators`);
      const deadline = Date.now() + job.timeout * 60_000;
      const stepsOut = {};
      let failed = false;
      let failure = '';
      const stepDir = join(OUT, 'steps', id);
      rmSync(stepDir, { recursive: true, force: true });
      mkdirSync(stepDir, { recursive: true });
      for (const [n, step] of job.steps.entries()) {
        if (this.stopping && this.state.get(id).cancelled) return { status: 'cancelled', reason: 'cancelled' };
        const label = step.name ?? step.run?.split('\n')[0] ?? step.uses;
        const ctx = { outputs: this.outputs, failed, steps: stepsOut };
        if (!evalIf(step.if, ctx)) {
          write(`\n==> step ${n + 1}: ${label}\n    skipped (if: ${step.if ?? 'success()'})`);
          continue;
        }
        const m = classifyStep(step, job);
        write(`\n==> step ${n + 1}: ${label}`);
        const t = Date.now();
        let ok = true;
        let why = '';
        if (m.kind === 'noop') write(`    mapped: ${m.reason}`);
        else if (m.kind === 'prereq') {
          const r = checkPrereq(m, job);
          write(`    prerequisite (${m.tool})${m.reason ? `: ${m.reason}` : ''}\n${r.text.split('\n').map((l) => `    ${l}`).join('\n')}`);
          Object.assign(env, r.env ?? {});
          ok = r.ok;
          why = r.ok ? '' : r.text.split('\n')[0];
        } else if (m.kind === 'upload') {
          ({ ok, why } = this.upload(m, write));
        } else if (m.kind === 'download') {
          ({ ok, why } = this.download(m, write));
        } else if (m.kind === 'run') {
          // A sharded job's run names its leg (`${{ matrix.shard }}`): the expressions resolve as the step env's do.
          let script = substitute(m.script, { root: ROOT, runnerTemp: RUNNER_TEMP, jobName: job.name, matrix: job.matrix });
          for (const r of rewrites) script = r(script);
          const stepEnv = { ...env };
          for (const [k, v] of Object.entries(step.env ?? {})) {
            let value = substitute(v, { root: ROOT, runnerTemp: RUNNER_TEMP, jobName: job.name, matrix: job.matrix });
            for (const r of rewrites) value = r(value);
            stepEnv[k] = value;
          }
          if (m.note) write(`    mapped: ${m.note}`);
          const stepMs = Math.min(deadline - Date.now(), step['timeout-minutes'] === undefined ? Infinity : Number(step['timeout-minutes']) * 60_000);
          let cwd = ROOT;
          if ((info.cleanTree ?? []).includes(m.script)) {
            cwd = this.cleanTree(id);
            stepEnv.GITHUB_WORKSPACE = cwd;
            write(`    mapped: runs in a clean copy of the tree (${relative(ROOT, cwd)}), as on a fresh checkout without build output`);
          }
          const r = await this.runStep({ id, n, script, env: stepEnv, fd, write, memo: m.memo, timeoutMs: stepMs, stress, stepDir, stepsOut, stepId: step.id, jobEnv: env, cwd });
          ok = r.ok;
          why = r.why;
        }
        write(`<== step ${n + 1}: ${ok ? 'ok' : 'FAILED'} (${fmt(Date.now() - t)})${why ? ` ${why}` : ''}`);
        if (!ok) {
          failed = true;
          failure ||= `step ${n + 1} (${label}): ${why || 'failed'}`;
        }
      }
      return failed ? { status: 'fail', reason: failure } : { status: 'pass', reason: '' };
    } finally {
      // A job that rewrites the tree runs alone: its processes are all gone before the snapshot goes back.
      if (!(id in EXTRA_JOBS) && this.info(id).snapshot) await this.waitChildren();
      for (const c of cleanups.reverse()) {
        try { c(); } catch (e) { write(`cleanup failed: ${e.message}`); }
      }
      closeSync(fd);
    }
  }

  async runStep({ id, n, script, env, fd, write, memo, timeoutMs, stress, stepDir, stepsOut, stepId, jobEnv, cwd = ROOT }) {
    if (memo !== null && this.memo.has(memo)) {
      const prior = this.memo.get(memo);
      write(`    shared step: \`${memo}\` runs once per ci-local run; waiting for ${prior.by}`);
      const r = await prior.promise;
      write(`    ${r.ok ? 'reused' : 'failed in'} ${prior.by} (see target/ci-local/${prior.by}.log)`);
      return { ok: r.ok, why: r.ok ? '' : `shared step failed in ${prior.by}` };
    }
    const exec = async () => {
      const file = join(stepDir, `${n + 1}.sh`);
      writeFileSync(file, `${script}\n`);
      const files = { GITHUB_ENV: join(stepDir, `${n + 1}.env`), GITHUB_PATH: join(stepDir, `${n + 1}.path`), GITHUB_OUTPUT: join(stepDir, `${n + 1}.out`) };
      for (const f of Object.values(files)) writeFileSync(f, '');
      write(script.split('\n').map((l) => `    $ ${l}`).join('\n'));
      const cmd = stress ? ['taskset', ['-c', this.stressCpus, 'bash', '--noprofile', '--norc', '-eo', 'pipefail', file]] : ['bash', ['--noprofile', '--norc', '-eo', 'pipefail', file]];
      const r = await this.spawnLogged(cmd[0], cmd[1], { ...env, ...files }, fd, timeoutMs, cwd);
      // GITHUB_PATH and GITHUB_ENV carry into the later steps, GITHUB_PATH into every job (a shared tool install).
      for (const p of readFileSync(files.GITHUB_PATH, 'utf8').split('\n').map((s) => s.trim()).filter(Boolean)) {
        if (!this.extraPath.includes(p)) this.extraPath.unshift(p);
        jobEnv.PATH = `${p}:${jobEnv.PATH}`;
      }
      Object.assign(jobEnv, parseEnvFile(readFileSync(files.GITHUB_ENV, 'utf8')));
      if (stepId) stepsOut[stepId] = parseEnvFile(readFileSync(files.GITHUB_OUTPUT, 'utf8'));
      return r;
    };
    if (memo === null) return exec();
    const entry = { by: id, promise: exec() };
    this.memo.set(memo, entry);
    return entry.promise;
  }

  /** A fresh copy of the working tree's tracked and untracked-unignored files: what a runner's checkout holds. */
  cleanTree(id) {
    const files = git(['ls-files', '-z', '--cached', '--others', '--exclude-standard']).stdout.split('\0').filter(Boolean);
    return buildCleanTree(ROOT, join(OUT, 'clean-tree', id), files);
  }

  spawnLogged(cmd, cmdArgs, env, fd, timeoutMs, cwd = ROOT) {
    return new Promise((done) => {
      const child = spawn(cmd, cmdArgs, { cwd, env, stdio: ['ignore', fd, fd], detached: true });
      this.children.add(child);
      this.groups.add(child.pid);
      let timedOut = false;
      const timer = Number.isFinite(timeoutMs) ? setTimeout(() => {
        timedOut = true;
        try { process.kill(-child.pid, 'SIGTERM'); } catch { /* gone */ }
        setTimeout(() => { try { process.kill(-child.pid, 'SIGKILL'); } catch { /* gone */ } }, 10_000).unref();
      }, Math.max(1, timeoutMs)) : null;
      child.on('error', (e) => {
        clearTimeout(timer);
        this.children.delete(child);
        done({ ok: false, why: e.message });
      });
      child.on('exit', (code, signal) => {
        clearTimeout(timer);
        this.children.delete(child);
        if (timedOut) done({ ok: false, why: `timed out after ${fmt(timeoutMs)} (the CI timeout-minutes)` });
        else done({ ok: code === 0, why: code === 0 ? '' : `exit ${signal ?? code}` });
      });
    });
  }

  // -------------------------------------------------------------- artifacts

  upload(m, write) {
    const dest = join(ARTIFACTS, m.name);
    rmSync(dest, { recursive: true, force: true });
    mkdirSync(dest, { recursive: true });
    let copied = 0;
    for (const p of m.paths) {
      const base = p.split('/').filter(Boolean);
      const globAt = base.findIndex((s) => /[*?[]/.test(s));
      const src = join(ROOT, ...(globAt < 0 ? base : base.slice(0, globAt)));
      if (!existsSync(src)) continue;
      const target = m.paths.length === 1 ? (statSync(src).isDirectory() ? dest : join(dest, basename(src))) : join(dest, relative(ROOT, src));
      cpSync(src, target, { recursive: true });
      copied++;
      write(`    artifact ${m.name}: ${relative(ROOT, src)} -> ${relative(ROOT, target)}`);
    }
    if (copied === 0 && m.required) return { ok: false, why: `no files at ${m.paths.join(', ')} (if-no-files-found: error)` };
    return { ok: true, why: '' };
  }

  download(m, write) {
    const src = join(ARTIFACTS, m.name);
    if (!existsSync(src)) return { ok: false, why: `artifact ${m.name} is not in ${relative(ROOT, ARTIFACTS)}: run the job that uploads it (drop --no-deps)` };
    const dest = join(ROOT, m.path);
    const age = fmt(Date.now() - statSync(src).mtimeMs);
    let copied = 0;
    let same = 0;
    const walk = (from, to) => {
      mkdirSync(to, { recursive: true });
      for (const e of readdirSync(from, { withFileTypes: true })) {
        const a = join(from, e.name);
        const b = join(to, e.name);
        if (e.isDirectory()) walk(a, b);
        else if (existsSync(b) && statSync(b).size === statSync(a).size && readFileSync(a).equals(readFileSync(b))) same++;
        else {
          // Copied beside and renamed over, so a job reading the file never sees half of it.
          const tmp = `${b}.ci-local-${process.pid}`;
          copyFileSync(a, tmp);
          renameSync(tmp, b);
          copied++;
        }
      }
    };
    walk(src, dest);
    write(`    artifact ${m.name} (uploaded ${age} ago) -> ${m.path}: ${copied} copied, ${same} already identical`);
    return { ok: true, why: '' };
  }

  restore(p) {
    if (!this.snapshots.has(p)) return;
    restoreSnapshot(ROOT, SNAPSHOTS, p);
    this.snapshots.delete(p);
  }

  // -------------------------------------------------------------- ports and locks

  /**
   * The first block from --base that no other run of this user holds (a per-user lock) and whose ports nothing on this machine
   * binds: a developer's own service on a suite port would otherwise answer the suite's requests.
   */
  async acquireBlock(id, write, kind) {
    let waited = false;
    for (;;) {
      for (let k = this.args.base; k <= MAX_BLOCK; k++) {
        if (this.blocks.has(k)) continue;
        const lock = tryLock(lockDir(LOCK_DIR), `port-block-${k}`, { job: id, root: ROOT });
        if (lock === null) continue;
        const { offset, uiOffset } = blockPorts(k);
        const busy = kind === 'ui'
          ? await busyPorts(UI_PORT_SPAN[0] + uiOffset, UI_PORT_SPAN[1] + uiOffset)
          : await busyPorts(E2E_PORT_SPAN[0] + offset, E2E_PORT_SPAN[1] + offset, { udp: true });
        if (busy.length > 0) {
          releaseLock(lock);
          write(`ports: block ${k} skipped, in use on this machine: ${busy.slice(0, 5).join(', ')}${busy.length > 5 ? ` and ${busy.length - 5} more` : ''}`);
          continue;
        }
        this.blocks.set(k, lock);
        return k;
      }
      if (!waited) write(`ports: every block from ${this.args.base} to ${MAX_BLOCK} is taken or busy; waiting`);
      waited = true;
      await sleep(5_000);
    }
  }

  releaseBlock(k) {
    releaseLock(this.blocks.get(k) ?? null);
    this.blocks.delete(k);
  }

  async waitMachineLock(name, id, write) {
    let waited = false;
    for (;;) {
      const lock = tryLock(lockDir(LOCK_DIR), name, { job: id, root: ROOT });
      if (lock !== null) {
        this.machineLocks.add(lock);
        return lock;
      }
      if (!waited) write(`${name}: held by another run on this machine; waiting`);
      waited = true;
      await sleep(5_000);
    }
  }

  releaseMachineLock(lock) {
    releaseLock(lock);
    this.machineLocks.delete(lock);
  }

  // -------------------------------------------------------------- services

  async startService(id, svc, write, cleanups) {
    const runtime = has('podman') ? 'podman' : has('docker') ? 'docker' : null;
    if (runtime === null) throw new Error(`job ${id} needs a ${svc.name} container: install ${TOOL_HINTS.container}`);
    const container = `dilla-ci-local-${process.pid}-${randomBytes(3).toString('hex')}-${id}-${svc.name}`;
    const ports = {};
    const publish = [];
    for (const p of svc.ports) {
      ports[p.host] = await freePort();
      publish.push('-p', `127.0.0.1:${ports[p.host]}:${p.container}`);
    }
    const envArgs = Object.entries(svc.env).flatMap(([k, v]) => ['-e', `${k}=${v}`]);
    const runArgs = ['run', '-d', '--rm', '--name', container, ...publish, ...envArgs, ...svc.options, svc.image];
    write(`service ${svc.name}: ${runtime} ${runArgs.join(' ')}`);
    const r = spawnSync(runtime, runArgs, { encoding: 'utf8' });
    if (r.status !== 0) throw new Error(`${runtime} run ${svc.image} failed: ${r.stderr}`);
    this.containers.add({ runtime, container });
    // Removed when the job ends, whatever happens next (a health wait that times out included).
    cleanups.push(() => this.stopService(container));
    const deadline = Date.now() + 180_000;
    for (;;) {
      if (svc.health === null) break;
      const h = spawnSync(runtime, ['exec', container, 'sh', '-c', svc.health], { encoding: 'utf8' });
      if (h.status === 0) break;
      if (Date.now() > deadline) throw new Error(`${svc.name} did not pass "${svc.health}" within 3 minutes`);
      await sleep(1_000);
    }
    write(`service ${svc.name}: healthy, ${svc.ports.map((p) => `127.0.0.1:${p.host} is 127.0.0.1:${ports[p.host]} here`).join(', ')}`);
    return { container, runtime, ports };
  }

  stopService(container) {
    for (const c of this.containers) {
      if (c.container !== container) continue;
      spawnSync(c.runtime, ['rm', '-f', c.container], { stdio: 'ignore' });
      this.containers.delete(c);
    }
  }

  // -------------------------------------------------------------- stress

  startStress() {
    const n = availableParallelism();
    const k = Math.max(2, Math.floor(n / 4));
    this.stressCpus = k === 1 ? '0' : `0-${k - 1}`;
    for (let i = 0; i < k; i++) {
      const child = spawn('taskset', ['-c', this.stressCpus, process.execPath, '-e', 'for(;;){}'], { stdio: 'ignore', detached: true });
      this.loadGens.push(child);
    }
    this.log(`stress: Go and browser jobs pinned to CPUs ${this.stressCpus} of ${n}, beside ${k} busy-loop processes on the same CPUs`);
  }

  // -------------------------------------------------------------- codeql

  async runCodeql(id, write, fd) {
    const spec = EXTRA_JOBS[id];
    if (!has('codeql')) {
      write(spec.install);
      return { status: 'skipped', reason: spec.install };
    }
    const dir = join(OUT, 'codeql');
    mkdirSync(dir, { recursive: true });
    const pack = { actions: 'actions', go: 'go', 'javascript-typescript': 'javascript', rust: 'rust' };
    const findings = [];
    for (const lang of spec.languages) {
      const db = join(dir, `db-${lang}`);
      const sarif = join(dir, `${lang}.sarif`);
      const mode = lang === 'go' ? 'autobuild' : 'none';
      const script = [
        `codeql database create ${db} --overwrite --language=${lang} --build-mode=${mode} --source-root=${ROOT} --threads=0`,
        `codeql database analyze ${db} --format=sarif-latest --output=${sarif} --download --threads=0 codeql/${pack[lang]}-queries:codeql-suites/${pack[lang]}-code-scanning.qls`,
      ].join('\n');
      write(`\n==> codeql ${lang}\n${script.split('\n').map((l) => `    $ ${l}`).join('\n')}`);
      const file = join(dir, `${lang}.sh`);
      writeFileSync(file, `${script}\n`);
      const r = await this.spawnLogged('bash', ['-eo', 'pipefail', file], { ...process.env }, fd, 60 * 60_000);
      if (!r.ok) return { status: 'fail', reason: `codeql ${lang}: ${r.why}` };
      const blocking = sarifBlocking(JSON.parse(readFileSync(sarif, 'utf8')));
      write(`    ${lang}: ${blocking.total} results, ${blocking.blocking.length} at error level or security-severity >= 7`);
      for (const b of blocking.blocking) write(`    ${b}`);
      findings.push(...blocking.blocking);
    }
    return findings.length === 0 ? { status: 'pass', reason: '' } : { status: 'fail', reason: `${findings.length} CodeQL alerts that would fail the code scanning check` };
  }

  // -------------------------------------------------------------- teardown

  cleanupSync() {
    for (const c of this.containers) spawnSync(c.runtime, ['rm', '-f', c.container], { stdio: 'ignore' });
    this.containers.clear();
    for (const p of [...this.snapshots]) this.restore(p);
    for (const l of this.machineLocks) releaseLock(l);
    this.machineLocks.clear();
    for (const l of this.blocks.values()) releaseLock(l);
    this.blocks.clear();
    for (const g of this.loadGens) { try { process.kill(-g.pid, 'SIGKILL'); } catch { /* gone */ } }
    this.loadGens = [];
  }

  summary() {
    const rows = this.selection.map((id) => {
      const st = this.state.get(id);
      return [id, { pass: 'pass', fail: 'FAIL', skipped: 'skipped', cancelled: 'cancelled', pending: 'not run', running: 'interrupted' }[st.status], st.started ? fmt(st.ms) : '-', st.status === 'fail' ? `${st.reason.split('\n')[0]}  [${relative(ROOT, join(OUT, `${id}.log`))}]` : st.reason.split('\n')[0]];
    });
    const w = [0, 1, 2].map((c) => Math.max(...rows.map((r) => r[c].length), ['job', 'result', 'time'][c].length));
    const line = (r) => `${r[0].padEnd(w[0])}  ${r[1].padEnd(w[1])}  ${r[2].padStart(w[2])}  ${r[3]}`.trimEnd();
    const out = [line(['job', 'result', 'time', 'note']), line(['-'.repeat(w[0]), '-'.repeat(w[1]), '-'.repeat(w[2]), '----']), ...rows.map(line)];
    out.push('', `total ${fmt(Date.now() - this.t0)}; logs in ${relative(ROOT, OUT)}/<job>.log`);
    writeFileSync(join(OUT, 'summary.txt'), `${out.join('\n')}\n`);
    writeFileSync(join(OUT, 'summary.json'), `${JSON.stringify(this.selection.map((id) => ({ job: id, ...this.state.get(id) })), null, 2)}\n`);
    return out.join('\n');
  }
}

/** GITHUB_ENV / GITHUB_OUTPUT: `K=V` lines and `K<<DELIM` … `DELIM` blocks. */
export function parseEnvFile(text) {
  const out = {};
  const lines = text.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const heredoc = /^([^=<\s]+)<<(\S+)$/.exec(lines[i]);
    if (heredoc) {
      const body = [];
      for (i++; i < lines.length && lines[i] !== heredoc[2]; i++) body.push(lines[i]);
      out[heredoc[1]] = body.join('\n');
    } else {
      const eq = lines[i].indexOf('=');
      if (eq > 0) out[lines[i].slice(0, eq)] = lines[i].slice(eq + 1);
    }
  }
  return out;
}

/** The SARIF results that would fail GitHub's code scanning check: error level, or a security severity of 7 or more. */
export function sarifBlocking(sarif) {
  let total = 0;
  const blocking = [];
  for (const run of sarif.runs ?? []) {
    const rules = new Map((run.tool?.driver?.rules ?? []).map((r) => [r.id, r]));
    for (const res of run.results ?? []) {
      total++;
      const rule = rules.get(res.ruleId);
      const severity = Number(rule?.properties?.['security-severity'] ?? 0);
      const level = res.level ?? rule?.defaultConfiguration?.level ?? 'warning';
      if (level === 'error' || severity >= 7) {
        const loc = res.locations?.[0]?.physicalLocation;
        blocking.push(`${res.ruleId} (${level}${severity ? `, severity ${severity}` : ''}) ${loc?.artifactLocation?.uri ?? '?'}:${loc?.region?.startLine ?? '?'}: ${res.message?.text ?? ''}`);
      }
    }
  }
  return { total, blocking };
}

function freePort() {
  return new Promise((done, fail) => {
    const srv = createServer();
    srv.unref();
    srv.on('error', fail);
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address();
      srv.close(() => done(port));
    });
  });
}

function describeStep(step, job) {
  try {
    const m = classifyStep(step, job);
    const what = m.kind === 'run' ? `run${m.memo ? ' (once per run)' : ''}: ${m.script.split('\n')[0]}` : m.kind === 'prereq' ? `prerequisite ${m.tool}` : m.kind === 'noop' ? `skip: ${m.reason}` : `${m.kind} ${m.name}`;
    return `${step.if ? `[if ${step.if}] ` : ''}${what}`;
  } catch (e) {
    return `UNMAPPED: ${e.message}`;
  }
}

async function main() {
  let args;
  try {
    args = parseArgs(process.argv.slice(2));
  } catch (e) {
    console.error(e.message);
    process.exit(2);
  }
  if (args.help) {
    console.log(USAGE);
    return;
  }
  // A run that died during go-release left the real client in internal/web/dist: put the tracked tree back first.
  for (const p of recoverSnapshots(ROOT, SNAPSHOTS)) console.log(`ci-local: restored ${p} from the snapshot an interrupted run left`);
  const { text, workflow, jobs } = loadWorkflow(ROOT);
  const problems = localMirrorProblems(text);
  if (problems.length > 0) {
    console.error(`ci.yml has steps ci-local cannot map (scripts/ci-local-plan.mjs):\n${problems.join('\n')}`);
    process.exit(2);
  }
  let outputs = EVERYTHING;
  let base = null;
  if (args.changed) {
    const mb = mergeBase();
    if (mb === null) console.log('ci-local: no origin/main or main to diff against; every area is on');
    else {
      const files = changedFiles(mb.sha);
      outputs = changedOutputs(mb.sha, files);
      base = `${mb.ref} (${mb.sha.slice(0, 10)}, ${files.length} files)`;
      console.log(`ci-local: ${files.length} files changed against ${mb.ref}; areas ${Object.entries(outputs).map(([k, v]) => `${k}=${v}`).join(' ')}`);
    }
  }
  let selection;
  try {
    selection = selectJobs(jobs, { only: args.only, outputs, deps: args.deps });
  } catch (e) {
    console.error(e.message);
    process.exit(2);
  }
  const byId = new Map(jobs.map((j) => [j.id, j]));
  if (args.dryRun) {
    for (const id of selection) {
      if (id in EXTRA_JOBS) {
        console.log(`\n${id} (ci-local extra: ${EXTRA_JOBS[id].languages.join(', ')})${has('codeql') ? '' : ' -> SKIPPED: codeql not installed'}`);
        continue;
      }
      const job = byId.get(id);
      const info = JOB_MAP[job.name];
      const skip = skipReason(job, process.arch);
      console.log(`\n${id}${job.needs.length ? ` (needs ${job.needs.join(', ')})` : ''} weight ${info.weight}${info.ports ? ` ports:${info.ports}` : ''}${info.exclusive ? ' exclusive' : ''}${skip ? ` -> SKIPPED: ${skip}` : ''}${info.kind && info.kind !== 'steps' ? ` [${info.kind}]` : ''}`);
      if (!skip && (!info.kind || info.kind === 'steps')) job.steps.forEach((s, n) => console.log(`  ${String(n + 1).padStart(2)}. ${describeStep(s, job)}`));
    }
    return;
  }
  const runnable = selection.filter((id) => id in EXTRA_JOBS || (JOB_MAP[byId.get(id).name].kind !== 'aggregate' && skipReason(byId.get(id), process.arch) === null));
  const missing = missingTools(requiredTools(runnable.filter((id) => !(id in EXTRA_JOBS)).map((id) => byId.get(id)), args));
  if (args.doctor) {
    console.log(missing.length === 0 ? 'ci-local: every prerequisite of the selected jobs is on PATH' : `ci-local: missing prerequisites:\n  ${missing.join('\n  ')}`);
    console.log(has('codeql') ? 'codeql: installed' : `codeql (optional): ${EXTRA_JOBS.codeql.install}`);
    process.exit(missing.length === 0 ? 0 : 2);
  }
  if (missing.length > 0) {
    console.error(`ci-local: missing prerequisites for the selected jobs:\n  ${missing.join('\n  ')}`);
    process.exit(2);
  }
  const dirty = git(['status', '--porcelain']).stdout.trim();
  if (dirty !== '') console.log('ci-local: the working tree has uncommitted changes; the jobs run on it as it is (go-sqlc and the vectors checks require a clean tree, as CI sees the pushed commit)');

  mkdirSync(OUT, { recursive: true });
  mkdirSync(RUNNER_TEMP, { recursive: true });
  for (const id of selection) rmSync(join(OUT, `${id}.log`), { force: true });
  const runner = new Runner(args, jobs, selection, outputs, workflow.env);
  runner.base = base;
  for (const id of selection) {
    if (id in EXTRA_JOBS) continue;
    const reason = skipReason(byId.get(id), process.arch);
    if (reason) Object.assign(runner.state.get(id), { status: 'skipped', reason });
  }
  let interrupted = false;
  const onSignal = (sig) => {
    if (interrupted) process.exit(130);
    interrupted = true;
    runner.log(`${sig}: stopping every job and tearing down`);
    runner.stopping = true;
    for (const id of runner.running.keys()) runner.state.get(id).cancelled = true;
    runner.killAll();
    // The tree is restored only once every step's processes are gone, so a dying go build cannot write after it.
    runner.waitChildren().then(() => {
      runner.cleanupSync();
      console.log(`\n${runner.summary()}`);
      process.exit(130);
    });
  };
  process.on('SIGINT', onSignal);
  process.on('SIGTERM', onSignal);
  process.on('exit', () => runner.cleanupSync());

  runner.log(`ci-local: ${selection.length} jobs (${args.only ? `--only ${args.only.join(',')}${args.deps ? ' and their needs' : ' without their needs'}` : args.changed ? '--changed' : 'full run'}), CPU budget ${args.jobs}, ${args.e2eJobs} browser jobs at once from port block ${args.base}, ${args.keepGoing ? 'keep going' : 'fail fast'}`);
  if (args.stress) runner.startStress();

  // npm ci deletes node_modules: it runs once, before any job, so it never pulls the tree from under a running job.
  const npmCi = selection.some((id) => !(id in EXTRA_JOBS) && byId.get(id).steps.some((s) => { try { return classifyStep(s).memo === 'npm ci'; } catch { return false; } }));
  if (npmCi) {
    runner.log('setup: npm ci (shared by every job that runs it)');
    const fd = openSync(join(OUT, 'npm-ci.log'), 'w');
    const t = Date.now();
    const promise = runner.spawnLogged('bash', ['-eo', 'pipefail', '-c', 'npm ci'], { ...runner.baseEnv({ name: 'setup', env: {}, matrix: {} }) }, fd, 30 * 60_000);
    runner.memo.set('npm ci', { by: 'npm-ci', promise });
    const r = await promise;
    closeSync(fd);
    runner.log(`setup: npm ci ${r.ok ? 'ok' : `FAILED (${r.why}); log: target/ci-local/npm-ci.log`} ${fmt(Date.now() - t)}`);
    if (!r.ok) {
      runner.cleanupSync();
      process.exit(1);
    }
  }

  await runner.run();
  runner.cleanupSync();
  console.log(`\n${runner.summary()}`);
  const failed = [...runner.state.entries()].filter(([, s]) => ['fail', 'cancelled'].includes(s.status));
  if (failed.length > 0) {
    const first = failed.find(([, s]) => s.status === 'fail');
    if (first) console.log(`\nfirst failure: ${first[0]}: ${relative(ROOT, join(OUT, `${first[0]}.log`))}`);
    process.exit(1);
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
