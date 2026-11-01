import type { ElectronApplication } from '@playwright/test';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

interface NetEvent { type: number; time: string; source: { id: number; type: number }; params?: Record<string, unknown> }
interface NetLog { constants: { logEventTypes: Record<string, number>; logSourceType: Record<string, number> }; events: NetEvent[] }
export interface SignalWire {
  socket: number; sentClose: number; receivedClose: number;
  frames: { direction: 'sent' | 'received'; ms: number }[];
  errors: { kind: 'read' | 'write' | 'connect' | 'tls'; code: number; ms: number }[];
}

/** Raw URLs are used only to classify the RTC request, never copied into the result.
 * Walk only connection-binding edges, excluding shared DNS/pool infrastructure and
 * other request identities. A gateway's close must never masquerade as an SFU close. */
export function signalWireSummary(raw: unknown): SignalWire[] {
  const log = raw as NetLog;
  if (!log?.constants?.logEventTypes || !log.constants.logSourceType || !Array.isArray(log.events)) throw new Error('Invalid private network log');
  const types = new Map(Object.entries(log.constants.logEventTypes).map(([name, id]) => [id, name]));
  const requests = new Map<number, { rtc: boolean; at: number }>();
  const edges = new Map<number, Set<number>>();
  const bind = /^(?:HTTP_STREAM_JOB_CONTROLLER_BOUND|HTTP_STREAM_JOB|HTTP_STREAM_REQUEST_STARTED_JOB|HTTP_STREAM_REQUEST_BOUND_TO_JOB|HTTP_STREAM_JOB_BOUND_TO_REQUEST|SOCKET_ALIVE|SOCKET_IN_USE|SOCKET_POOL_BOUND_TO_CONNECT_JOB|SOCKET_POOL_BOUND_TO_SOCKET|CONNECT_JOB_SET_SOCKET|TCP_CONNECT_JOB_CONNECTOR_CONNECT_START)$/;
  for (const event of log.events) {
    if (!Number.isSafeInteger(event.source?.id)) continue;
    const id = event.source.id, params = event.params ?? {}, name = types.get(event.type) ?? '';
    if (event.source.type === log.constants.logSourceType.URL_REQUEST && typeof params.url === 'string') {
      try {
        const url = new URL(params.url);
        requests.set(id, { rtc: ['ws:', 'wss:'].includes(url.protocol) && /^\/rtc(?:\/v1)?$/.test(url.pathname), at: Number(event.time) });
      } catch { requests.set(id, { rtc: false, at: Number(event.time) }); }
    }
    const dependency = params.source_dependency as { id?: number } | undefined;
    if (bind.test(name) && Number.isSafeInteger(dependency?.id)) {
      const other = dependency!.id!;
      if (!edges.has(id)) edges.set(id, new Set());
      if (!edges.has(other)) edges.set(other, new Set());
      edges.get(id)!.add(other); edges.get(other)!.add(id);
    }
  }
  const out: SignalWire[] = [];
  const errors: Record<string, SignalWire['errors'][number]['kind']> = {
    SOCKET_READ_ERROR: 'read', SOCKET_WRITE_ERROR: 'write', TCP_CONNECT: 'connect', TCP_CONNECT_ATTEMPT: 'connect', SSL_CONNECT: 'tls',
  };
  for (const [root, request] of requests) {
    if (!request.rtc || !Number.isFinite(request.at) || out.length >= 16) continue;
    const connected = new Set([root]), queue = [root];
    for (let i = 0; i < queue.length && connected.size < 256; i++) {
      for (const other of edges.get(queue[i]) ?? []) {
        if (connected.has(other) || (requests.has(other) && other !== root)) continue;
        connected.add(other); queue.push(other);
      }
    }
    const summary: SignalWire = { socket: out.length + 1, sentClose: 0, receivedClose: 0, frames: [], errors: [] };
    for (const event of log.events) {
      if (!connected.has(event.source?.id)) continue;
      const name = types.get(event.type), params = event.params ?? {}, ms = Number(event.time) - request.at;
      if (!Number.isFinite(ms) || ms < 0) continue;
      if (params.opcode === 8 && (name === 'WEBSOCKET_SENT_FRAME_HEADER' || name === 'WEBSOCKET_RECV_FRAME_HEADER')) {
        const direction = name === 'WEBSOCKET_SENT_FRAME_HEADER' ? 'sent' : 'received';
        if (direction === 'sent') summary.sentClose++; else summary.receivedClose++;
        if (summary.frames.length < 32) summary.frames.push({ direction, ms });
      }
      const kind = name ? errors[name] : undefined, code = params.net_error;
      if (kind && Number.isInteger(code) && Number(code) < 0 && Number(code) >= -10000 && summary.errors.length < 32) summary.errors.push({ kind, code: Number(code), ms });
    }
    out.push(summary);
  }
  return out;
}

/** Default capture excludes authentication/cookie payloads. The raw file still
 * contains routing URLs: keep it outside every artifact directory, never attach
 * or print it, and remove it even when parsing or native shutdown fails. */
export async function startSignalWire(application: ElectronApplication): Promise<() => Promise<SignalWire[]>> {
  const directory = await mkdtemp(join(tmpdir(), 'dilla-private-signal-wire-')), path = join(directory, 'network.json');
  try {
    await application.evaluate(async ({ BrowserWindow }, path) => {
      const log = BrowserWindow.getAllWindows()[0].webContents.session.netLog;
      if (log.currentlyLogging) throw new Error('Private signal capture already active');
      await log.startLogging(path, { captureMode: 'default', maxFileSize: 8 * 1024 * 1024 });
    }, path);
  } catch (error) { await rm(directory, { recursive: true, force: true }); throw error; }
  let done: Promise<SignalWire[]> | undefined;
  return () => done ??= (async () => {
    try {
      await application.evaluate(async ({ BrowserWindow }) => { await BrowserWindow.getAllWindows()[0].webContents.session.netLog.stopLogging(); });
      return signalWireSummary(JSON.parse(await readFile(path, 'utf8')));
    } finally { await rm(directory, { recursive: true, force: true }); }
  })();
}
