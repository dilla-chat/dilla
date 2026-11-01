import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { canonicalInstance } from './policy.ts';

/** Non-secret shell preferences only. Device credentials remain in each core worker's storage. */
export class Preferences {
  instances: string[] = [];
  autostart = false;
  private path: string;
  private directory: string;
  private allowLoopback: boolean;
  private writing: Promise<void> = Promise.resolve();
  constructor(directory: string, allowLoopback = false) { this.directory = directory; this.path = join(directory, 'desktop.json'); this.allowLoopback = allowLoopback; }
  async load(): Promise<void> {
    let text: string;
    try { text = await readFile(this.path, 'utf8'); }
    catch (error) { if ((error as NodeJS.ErrnoException).code === 'ENOENT') return; throw error; }
    if (text.length > 131_072) throw new Error('Desktop preferences too large');
    const doc = JSON.parse(text) as { version?: unknown; instances?: unknown; autostart?: unknown };
    if (doc?.version !== 1 || !Array.isArray(doc.instances) || doc.instances.length > 32 || typeof doc.autostart !== 'boolean') throw new Error('Invalid desktop preferences');
    this.instances = [...new Set(doc.instances.map(instance => canonicalInstance(instance, this.allowLoopback)))];
    this.autostart = doc.autostart;
  }
  async addInstance(input: string): Promise<void> {
    const instance = canonicalInstance(input, this.allowLoopback);
    if (this.instances.includes(instance)) return;
    if (this.instances.length >= 32) throw new Error('At most 32 instances can be open');
    this.instances.push(instance);
    await this.save();
  }
  async setAutostart(enabled: boolean): Promise<void> { this.autostart = enabled; await this.save(); }
  private save(): Promise<void> {
    const text = JSON.stringify({ version: 1, instances: this.instances, autostart: this.autostart }) + '\n';
    const next = this.writing.catch(() => {}).then(async () => {
      await mkdir(this.directory, { recursive: true, mode: 0o700 });
      await writeFile(this.path + '.tmp', text, { mode: 0o600 });
      await rename(this.path + '.tmp', this.path);
    });
    this.writing = next;
    return next;
  }
}
