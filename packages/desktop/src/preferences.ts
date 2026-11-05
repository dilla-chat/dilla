import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { canonicalInstance } from './policy.ts';

/** Allow answers worth remembering; local-network and loopback-network keep asking. */
export const GRANTABLE_PERMISSIONS = ['audio', 'video', 'notifications', 'speaker-selection'] as const;
const grantable = (value: unknown): value is string => typeof value === 'string' && (GRANTABLE_PERMISSIONS as readonly string[]).includes(value);

/** The remembered tokens a permission needs: a media request is split into microphone and camera. Unknown media types need both. */
export function permissionTokens(permission: string, mediaTypes: readonly string[]): string[] {
  if (permission !== 'media') return [permission];
  const wanted = mediaTypes.filter(type => type === 'audio' || type === 'video');
  return wanted.length ? [...new Set(wanted)] : ['audio', 'video'];
}

/** Non-secret shell preferences only. Device credentials remain in each core worker's storage. */
export class Preferences {
  instances: string[] = [];
  autostart = false;
  private granted = new Map<string, string[]>();
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
    const doc = JSON.parse(text) as { version?: unknown; instances?: unknown; autostart?: unknown; grants?: unknown };
    if (doc?.version !== 1 || !Array.isArray(doc.instances) || doc.instances.length > 32 || typeof doc.autostart !== 'boolean') throw new Error('Invalid desktop preferences');
    this.instances = [...new Set(doc.instances.map(instance => canonicalInstance(instance, this.allowLoopback)))];
    this.autostart = doc.autostart;
    this.granted = this.parseGrants(doc.grants);
  }
  private parseGrants(raw: unknown): Map<string, string[]> {
    const result = new Map<string, string[]>();
    if (raw === undefined) return result;
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) { console.warn('Dilla dropped invalid permission grants from desktop preferences'); return result; }
    let dropped = false;
    for (const [key, value] of Object.entries(raw)) {
      try {
        const instance = canonicalInstance(key, this.allowLoopback);
        if (instance !== key || !this.instances.includes(instance) || !Array.isArray(value) || !value.every(grantable) || new Set(value).size !== value.length) throw new Error('invalid');
        if (value.length) result.set(instance, [...value]);
      } catch { dropped = true; }
    }
    if (dropped) console.warn('Dilla dropped invalid permission grants from desktop preferences');
    return result;
  }
  grants(input: string): string[] { return [...(this.granted.get(canonicalInstance(input, this.allowLoopback)) ?? [])]; }
  async grant(input: string, permission: string): Promise<void> {
    const instance = canonicalInstance(input, this.allowLoopback);
    if (!this.instances.includes(instance)) throw new Error('Unknown instance');
    if (!grantable(permission)) throw new Error('This permission is not remembered');
    const current = this.granted.get(instance) ?? [];
    if (current.includes(permission)) return;
    this.granted.set(instance, [...current, permission]);
    await this.save();
  }
  async removeInstance(input: string): Promise<void> {
    const instance = canonicalInstance(input, this.allowLoopback);
    this.instances = this.instances.filter(value => value !== instance);
    this.granted.delete(instance);
    await this.save();
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
    const text = JSON.stringify({ version: 1, instances: this.instances, autostart: this.autostart, ...(this.granted.size ? { grants: Object.fromEntries(this.granted) } : {}) }) + '\n';
    const next = this.writing.catch(() => {}).then(async () => {
      await mkdir(this.directory, { recursive: true, mode: 0o700 });
      await writeFile(this.path + '.tmp', text, { mode: 0o600 });
      await rename(this.path + '.tmp', this.path);
    });
    this.writing = next;
    return next;
  }
}
