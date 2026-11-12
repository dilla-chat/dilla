// The native GPUI client as a test peer: clients/desktop/dilla-client/examples/peer.rs, driven over its line protocol
// (stdin commands, one stdout answer line each). Test-only: nothing here changes any client.
import { spawn, spawnSync, type ChildProcessByStdio } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import type { Readable, Writable } from 'node:stream';
import { REPO_ROOT } from './app';

/** DILLA_GPUI_PEER names a prebuilt binary (CI builds it in its own step); otherwise the example is built on first use,
 *  which only a developer machine does: a CI run without the binary is a setup error, not something to build here. */
export function gpuiPeerBinary(): string {
  const named = process.env.DILLA_GPUI_PEER;
  if (named !== undefined && named !== '') {
    if (!existsSync(named)) throw new Error(`DILLA_GPUI_PEER=${named} does not exist: cargo build -p dilla-client --example peer`);
    return named;
  }
  const bin = join(REPO_ROOT, 'target', 'debug', 'examples', process.platform === 'win32' ? 'peer.exe' : 'peer');
  if (process.env.CI === 'true') {
    if (!existsSync(bin)) throw new Error(`${bin} is missing: build it in the job (cargo build -p dilla-client --example peer --locked) and set DILLA_GPUI_PEER`);
    return bin;
  }
  const built = spawnSync('cargo', ['build', '-p', 'dilla-client', '--example', 'peer', '--locked'], { cwd: REPO_ROOT, stdio: 'inherit' });
  if (built.status !== 0) throw new Error('cargo build -p dilla-client --example peer failed');
  return bin;
}

export class GpuiPeer {
  private readonly lines: string[] = [];
  private waiting: ((line: string) => void) | null = null;
  private exited = false;
  private constructor(private readonly child: ChildProcessByStdio<Writable, Readable, null>, private readonly dir: string) {
    createInterface({ input: child.stdout }).on('line', (line) => {
      if (this.waiting !== null) { const w = this.waiting; this.waiting = null; w(line); } else this.lines.push(line);
    });
    child.on('exit', () => { this.exited = true; this.waiting?.('err the peer process exited'); this.waiting = null; });
  }

  static spawn(): GpuiPeer {
    const dir = mkdtempSync(join(tmpdir(), 'dilla-gpui-peer-'));
    const child = spawn(gpuiPeerBinary(), [], { stdio: ['pipe', 'pipe', 'inherit'] });
    return new GpuiPeer(child, dir);
  }

  /** One command and its one answer line: `ok ...` resolves with what follows, `err ...` rejects. */
  async command(line: string, timeoutMs = 60_000): Promise<string> {
    if (this.exited) throw new Error('the peer process exited');
    const answer = new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => { this.waiting = null; reject(new Error(`peer: no answer to ${line.split(' ')[0]} in ${String(timeoutMs)} ms`)); }, timeoutMs);
      const settle = (got: string) => {
        clearTimeout(timer);
        if (got === 'ok' || got.startsWith('ok ')) resolve(got.slice(3));
        else reject(new Error(`peer: ${got}`));
      };
      const queued = this.lines.shift();
      if (queued !== undefined) settle(queued); else this.waiting = settle;
    });
    this.child.stdin.write(`${line}\n`);
    return answer;
  }

  /** Signs the peer up on the instance, joins `community` with the invite code and starts its client. Answers user and device ids. */
  async start(o: { server: string; control: string; instanceInvite: string; community: string; code: string; username: string }): Promise<{ user: string; device: string }> {
    const reply = await this.command(`start ${this.dir} ${o.server} ${o.control} ${o.instanceInvite} ${o.community} ${o.code} ${o.username}`, 120_000);
    const m = /^user=([0-9a-f]{32}) device=([0-9a-f]{32})$/.exec(reply);
    if (m === null) throw new Error(`peer: unexpected start answer ${reply}`);
    return { user: m[1], device: m[2] };
  }

  channel(community: string, name: string): Promise<string> { return this.command(`channel ${community} ${name}`, 60_000); }
  /** The community owner (a user id), as the native client's own state has it. */
  owner(community: string): Promise<string> { return this.command(`owner ${community}`, 30_000); }
  send(channel: string, text: string): Promise<string> { return this.command(`send ${channel} ${text}`, 15_000); }
  /** Waits for a confirmed message of someone else; answers its author's user id. */
  async wait(channel: string, text: string, timeoutMs: number): Promise<string> {
    const reply = await this.command(`wait ${channel} ${text} ${String(timeoutMs)}`, timeoutMs + 5_000);
    return reply.replace(/^author=/, '');
  }

  async close(): Promise<void> {
    try { if (!this.exited) await this.command('quit', 10_000); } catch { /* it is going away either way */ }
    this.child.kill('SIGKILL');
    rmSync(this.dir, { recursive: true, force: true });
  }
}
