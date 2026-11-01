import { themes } from '../../design-tokens/src/tokens.ts';
import { createHash } from 'node:crypto';
import type { DesktopCommand } from './contract.ts';

const unsafeUrl = /[\s\\\u0000-\u001f\u007f]/;

export function canonicalInstance(input: string, allowLoopback = false): string {
  if (typeof input !== 'string' || input.length > 2048 || unsafeUrl.test(input) || /%(?:2f|5c|00|2e)/i.test(input)) throw new Error('Invalid instance URL');
  const url = new URL(input);
  if (url.username || url.password || url.search || url.hash) throw new Error('Instance URLs cannot contain credentials, queries or fragments');
  const local = allowLoopback && ['127.0.0.1', '[::1]', 'localhost'].includes(url.hostname);
  if (url.protocol !== 'https:' && !(local && url.protocol === 'http:')) throw new Error('An HTTPS instance is required');
  if (!url.hostname || url.hostname.endsWith('.')) throw new Error('Invalid instance host');
  if (!url.pathname.endsWith('/')) url.pathname += '/';
  if (url.pathname.startsWith('/assets/')) throw new Error('The installed client reserves /assets/; choose another instance base path');
  return url.href;
}

export function partitionFor(instance: string): string {
  const canonical = canonicalInstance(instance, true);
  return `persist:dilla-${createHash('sha256').update(canonical).digest('hex')}`;
}

/** The installed UI uses origin-root routes; API and gateway requests are forwarded to the selected base path. */
export function instanceRequestURL(input: string, instance: string): string {
  const url = new URL(input), base = new URL(instance);
  const networkOrigin = url.protocol === 'wss:' ? `https://${url.host}` : url.protocol === 'ws:' ? `http://${url.host}` : url.origin;
  if (base.pathname !== '/' && url.pathname.startsWith(base.pathname)
    && /^\/(v1\/|i\/|gateway(?:\/|$)|rtc(?:\/|$))/.test(url.pathname.slice(base.pathname.length - 1))) return input;
  if (networkOrigin !== base.origin || !/^\/(v1\/|i\/|gateway(?:\/|$)|rtc(?:\/|$))/.test(url.pathname)) return input;
  url.pathname = base.pathname + url.pathname.slice(1);
  return url.href;
}

export function parseInvite(input: string): { instance: string; code: string } {
  if (typeof input !== 'string' || input.length > 4096 || unsafeUrl.test(input)) throw new Error('Invalid invite');
  // Match the original string before URL parsing can normalize dots, slashes or encoded characters.
  const match = /^dilla:\/\/invite\/([A-Za-z0-9.[\]:-]+)\/([A-Za-z0-9_-]{1,128})$/.exec(input);
  if (!match) throw new Error('Invalid invite');
  const instance = canonicalInstance(`https://${match[1]}/`);
  return { instance, code: match[2] };
}

export function isAppNavigation(input: string, instance: string): boolean {
  try {
    const url = new URL(input);
    if (url.origin !== new URL(instance).origin || url.username || url.password || unsafeUrl.test(input)) return false;
    const path = url.pathname;
    if (path === '/welcome') {
      const allowed = new Set(['invite', 'signin']);
      return [...url.searchParams.keys()].every(key => allowed.has(key))
        && [...url.searchParams.values()].every(value => /^[A-Za-z0-9_-]{0,128}$/.test(value));
    }
    if (url.search) return false;
    return path === '/' || /^\/(dm(?:\/[a-f0-9]{32})?|activity|you)$/.test(path)
      || /^\/c\/[a-f0-9]{32}(?:\/[a-f0-9]{32})?$/.test(path)
      || /^\/settings(?:\/(profile|devices|signin|notifications|appearance|voice|privacy|keyboard|diagnostics))?$/.test(path);
  } catch { return false; }
}

export function trustedFrame(frame: { url: string; top: boolean } | null, instance: string): boolean {
  return frame !== null && frame.top && isAppNavigation(frame.url, instance);
}

export function externalLink(input: string): string | null {
  try {
    if (unsafeUrl.test(input) || input.length > 8192) return null;
    const url = new URL(input);
    return ['https:', 'http:'].includes(url.protocol) && !url.username && !url.password ? url.href : null;
  } catch { return null; }
}

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid desktop command');
  return value as Record<string, unknown>;
}

export function validateCommand(value: unknown): DesktopCommand {
  const command = object(value);
  const fields: Record<string, string[]> = {
    badge: ['count'], guard: ['activeCall', 'unsavedDraft'], 'quit-ack': ['id', 'activeCall', 'unsavedDraft'], autostart: ['enabled'],
    'notify-ring': ['id', 'title', 'body'], notify: ['id', 'title', 'body'], 'dismiss-notification': ['id'], 'stop-app-audio': ['id'], 'check-update': [], 'restart-update': [], 'window-mode': ['mode', 'height'], 'window-menu': [], 'window-control': ['action'], 'desktop-theme': ['theme'],
  };
  const type = typeof command.type === 'string' ? command.type : '';
  if (!Object.hasOwn(fields, type)) throw new Error('Unknown desktop command');
  const expected = ['type', ...fields[type]];
  if (Object.keys(command).length !== expected.length || !Object.keys(command).every(key => expected.includes(key))) throw new Error('Invalid desktop command fields');
  if (type === 'window-control' && !['minimize', 'toggle-maximize', 'close'].includes(command.action as string)) throw new Error('Invalid window control');
  if (type === 'window-mode' && (!['starting', 'authentication', 'workspace'].includes(command.mode as string)
    || (command.height !== null && (command.mode !== 'authentication' || !Number.isSafeInteger(command.height) || (command.height as number) < 160 || (command.height as number) > 1600)))) throw new Error('Invalid desktop window mode');
  if (type === 'desktop-theme' && (typeof command.theme !== 'string' || !Object.hasOwn(themes, command.theme))) throw new Error('Invalid desktop theme');
  if (type === 'badge' && (!Number.isSafeInteger(command.count) || (command.count as number) < 0 || (command.count as number) > 9999)) throw new Error('Invalid badge');
  if (['guard', 'quit-ack'].includes(type) && (typeof command.activeCall !== 'boolean' || typeof command.unsavedDraft !== 'boolean')) throw new Error('Invalid quit guard');
  if (type === 'quit-ack' && (typeof command.id !== 'string' || !/^[a-f0-9-]{36}$/.test(command.id))) throw new Error('Invalid quit acknowledgment');
  if (type === 'autostart' && typeof command.enabled !== 'boolean') throw new Error('Invalid startup preference');
  if (type === 'notify-ring' || type === 'notify' || type === 'dismiss-notification' || type === 'stop-app-audio') {
    if (typeof command.id !== 'string' || !/^[A-Za-z0-9:_-]{1,128}$/.test(command.id)) throw new Error('Invalid notification id');
  }
  if (type === 'notify' || type === 'notify-ring') {
    if (typeof command.title !== 'string' || command.title.length < 1 || command.title.length > 256 || typeof command.body !== 'string' || command.body.length > 4096) throw new Error('Invalid notification');
  }
  return command as unknown as DesktopCommand;
}

/** Electron may supply an origin as either a serialized origin or an origin URL. */
export function permissionOriginMatches(input: string, instance: string): boolean {
  try {
    const url = new URL(input);
    return !url.username && !url.password && !unsafeUrl.test(input) && url.origin === new URL(instance).origin;
  } catch { return false; }
}
