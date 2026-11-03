import { createServer, STATUS_CODES } from 'node:http';
import { randomBytes, randomUUID } from 'node:crypto';
import type { APIResponse, BrowserContext, ElectronApplication, Page, Request as BrowserRequest, Response as BrowserResponse, Route } from '@playwright/test';

type Matcher = Parameters<BrowserContext['route']>[0];
type Handler = Parameters<BrowserContext['route']>[1];
type Packet = { id: string; url: string; method: string; headers: Record<string, string>; body?: string };
type Decision = { action: 'continue' | 'fulfill' | 'abort'; status?: number; headers?: Record<string, string>; body?: string };

function pattern(matcher: Matcher): { source: string; flags: string } {
 if (matcher instanceof RegExp) return { source: matcher.source, flags: matcher.flags };
 if (typeof matcher !== 'string') throw new Error('Desktop network fixtures require a string or RegExp route');
 let source = '^';
 for (let i = 0; i < matcher.length; i++) {
  if (matcher[i] === '*') { if (matcher[i + 1] === '*') { source += '.*'; i++; } else source += '[^/]*'; }
  else source += matcher[i].replace(/[|\\{}()[\]^$+?.]/g, '\\$&');
 }
 return { source: source + '$', flags: '' };
}

/** Session.fetch deliberately bypasses custom protocol handlers, so its real
 * HTTP requests are outside Playwright's renderer Fetch interception. Observe
 * that public Electron transport at the fixture boundary. Only matched routes
 * cross the private loopback bridge; ordinary requests still use Session.fetch.
 * No production capability or worker/key implementation is replaced. */
export async function desktopNetwork(context: BrowserContext, application: ElectronApplication, page: Page): Promise<() => Promise<void>> {
 const rules: { id: string; matcher: Matcher; regex: RegExp; handler: Handler; remaining: number }[] = [];
 const pending = new Map<string, { packet: Packet; request: BrowserRequest }>();
 const secret = '/' + randomBytes(32).toString('hex');
 // Chromium also reports the outer protocol response. API listeners must see
 // the transport request exactly once, with the same identity as route.request().
 const emitContext = (context as any).emit.bind(context), emitPage = (page as any).emit.bind(page);
 const instance = new URL(page.url());
 const isAPI = (value: { url(): string }) => {
  const url = new URL(value.url());
  return url.origin === instance.origin && /\/(?:v1\/|gateway(?:\/|$)|rtc(?:\/|$))/.test(url.pathname);
 };
 const networkEvents = new Set(['request', 'response', 'requestfinished', 'requestfailed']);
 (context as any).emit = (name: string, value: any, ...rest: unknown[]) => networkEvents.has(name) && isAPI(value) ? false : emitContext(name, value, ...rest);
 (page as any).emit = (name: string, value: any, ...rest: unknown[]) => networkEvents.has(name) && isAPI(value) ? false : emitPage(name, value, ...rest);
 const emit = (name: string, value: any) => { if (isAPI(value)) { emitContext(name, value); emitPage(name, value); } };
 function requestOf(packet: Packet): BrowserRequest {
  return { url: () => packet.url, method: () => packet.method, headers: () => packet.headers,
   allHeaders: async () => packet.headers, postData: () => packet.body ? Buffer.from(packet.body, 'base64').toString() : null,
   postDataBuffer: () => packet.body === undefined ? null : Buffer.from(packet.body, 'base64'),
   resourceType: () => 'fetch', isNavigationRequest: () => false, frame: () => page.mainFrame(),
  } as unknown as BrowserRequest;
 }
 function responseOf(packet: Packet & { status: number; responseHeaders?: Record<string, string> }, request: BrowserRequest): BrowserResponse {
  return { url: () => packet.url, request: () => request, status: () => packet.status, statusText: () => STATUS_CODES[packet.status] ?? '',
   ok: () => packet.status >= 200 && packet.status < 300, headers: () => packet.responseHeaders ?? {},
   frame: () => page.mainFrame(), fromServiceWorker: () => false, finished: async () => null,
   body: async () => { throw new Error('Use route.fetch() to inspect a native transport response body'); },
  } as unknown as BrowserResponse;
 }
 const server = createServer((incoming, outgoing) => {
  if (incoming.method !== 'POST' || incoming.url !== secret) { outgoing.writeHead(404).end(); return; }
  let size = 0; const chunks: Buffer[] = [];
  incoming.on('data', (chunk: Buffer) => { size += chunk.length; if (size > 96 * 1024 * 1024) incoming.destroy(); else chunks.push(chunk); });
  incoming.on('end', () => { void (async () => {
   const packet = JSON.parse(Buffer.concat(chunks).toString()) as Packet & { event: string; status: number; responseHeaders?: Record<string, string> };
   if (packet.event === 'response' || packet.event === 'finished' || packet.event === 'failed') {
    const entry = pending.get(packet.id);
    const request = entry?.request ?? requestOf(packet);
    if (packet.event === 'response') emit('response', responseOf(packet, request));
    else { pending.delete(packet.id); emit(packet.event === 'finished' ? 'requestfinished' : 'requestfailed', request); }
    outgoing.writeHead(204).end(); return;
   }
   const request = requestOf(packet);
   if (pending.size >= 512) pending.delete(pending.keys().next().value!);
   pending.set(packet.id, { packet, request }); emit('request', request);
   const finish = (decision: Decision) => { if (!outgoing.destroyed && !outgoing.writableEnded) outgoing.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(decision)); };
   if (packet.event === 'request') { outgoing.writeHead(204).end(); return; }
   const matched = rules.filter(rule => { rule.regex.lastIndex = 0; return rule.remaining > 0 && rule.regex.test(packet.url); });
   async function dispatch(index: number): Promise<void> {
    const rule = matched[index]; if (!rule) { finish({ action: 'continue' }); return; }
    rule.remaining--;
    const route = {
     request: () => request,
     continue: async (options?: { headers?: Record<string, string>; postData?: string | Buffer }) => {
      finish({ action: 'continue', headers: options?.headers, ...(options?.postData !== undefined ? { body: Buffer.from(options.postData).toString('base64') } : {}) });
     },
     fallback: async () => { await dispatch(index + 1); },
     abort: async () => { finish({ action: 'abort' }); },
     fetch: async (options: Parameters<Route['fetch']>[0] = {}) => context.request.fetch(options.url ?? packet.url, {
      method: options.method ?? packet.method, headers: options.headers ?? packet.headers,
      ...(packet.method === 'GET' || packet.method === 'HEAD' ? {} : { data: options.postData ?? (packet.body === undefined ? undefined : Buffer.from(packet.body, 'base64')) }),
      timeout: options.timeout, maxRedirects: options.maxRedirects, maxRetries: options.maxRetries,
     }),
     fulfill: async (options: { response?: APIResponse; status?: number; headers?: Record<string, string>; body?: string | Buffer; json?: unknown; contentType?: string }) => {
      const original = options.response;
      const headers = { ...(original?.headers() ?? {}), ...options.headers };
      delete headers['content-encoding']; delete headers['content-length']; delete headers['transfer-encoding'];
      if (options.contentType) headers['content-type'] = options.contentType;
      if (options.json !== undefined) headers['content-type'] = 'application/json';
      const body = options.json !== undefined ? Buffer.from(JSON.stringify(options.json)) : options.body !== undefined ? Buffer.from(options.body) : original ? await original.body() : Buffer.alloc(0);
      finish({ action: 'fulfill', status: options.status ?? original?.status() ?? 200, headers, body: body.toString('base64') });
     },
    } as unknown as Route;
    await rule.handler(route, request);
   }
   await dispatch(0);
  })().catch(error => { if (!outgoing.destroyed && !outgoing.writableEnded) outgoing.writeHead(500).end(String(error)); }); });
 });
 await new Promise<void>((done, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', done); });
 const address = server.address(); if (!address || typeof address === 'string') throw new Error('Native network fixture did not start');
 await application.evaluate(({ BrowserWindow }, endpoint) => {
  const session = BrowserWindow.getAllWindows()[0].webContents.session, native = session.fetch.bind(session);
  const root = globalThis as any; root.__dillaAcceptanceRoutes = [];
  session.fetch = async (input: string | Request, init?: RequestInit & { bypassCustomProtocolHandlers?: boolean }) => {
   const request = input instanceof Request ? input.clone() : new Request(input, init);
   const packet: Packet = { id: crypto.randomUUID(), url: request.url, method: request.method, headers: Object.fromEntries(request.headers.entries()) };
   const report = async (value: unknown) => { const response = await fetch(endpoint, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(value) }); if (!response.ok) throw new Error('Native network fixture failed'); return response; };
   const routed = root.__dillaAcceptanceRoutes.some((rule: { source: string; flags: string }) => new RegExp(rule.source, rule.flags).test(request.url));
   let decision: Decision = { action: 'continue' };
   if (routed) { if (packet.method !== 'GET' && packet.method !== 'HEAD') packet.body = Buffer.from(await request.arrayBuffer()).toString('base64'); decision = await (await report({ ...packet, event: 'route' })).json(); }
   else await report({ ...packet, headers: {}, event: 'request' });
   if (decision.action === 'abort') throw new TypeError('Request aborted by native acceptance route');
   let response: Response;
   if (decision.action === 'fulfill') response = new Response([204, 304].includes(decision.status!) || packet.method === 'HEAD' ? null : Buffer.from(decision.body ?? '', 'base64'), { status: decision.status, headers: decision.headers });
   else {
    const changed = decision.headers || decision.body !== undefined;
    const next = changed ? new Request(packet.url, { method: packet.method, headers: decision.headers ?? packet.headers,
     ...(packet.method === 'GET' || packet.method === 'HEAD' ? {} : { body: decision.body !== undefined ? Buffer.from(decision.body, 'base64') : Buffer.from(packet.body ?? '', 'base64') }),
    }) : input;
    try { response = await native(next, init); }
    catch (error) { await report({ ...packet, headers: {}, body: undefined, event: 'failed' }); throw error; }
   }
   await report({ ...packet, headers: {}, body: undefined, event: 'response', status: response.status, responseHeaders: Object.fromEntries(response.headers.entries()) });
   void response.clone().arrayBuffer().then(() => report({ ...packet, headers: {}, body: undefined, event: 'finished' })).catch(() => {});
   return response;
  };
 }, `http://127.0.0.1:${address.port}${secret}`);
 const publish = async () => application.evaluate((_electron, patterns) => { (globalThis as any).__dillaAcceptanceRoutes = patterns; }, rules.filter(rule => rule.remaining > 0).map(rule => pattern(rule.matcher)));
 context.route = async (matcher, handler, options) => {
  const value = pattern(matcher), rule = { id: randomUUID(), matcher, regex: new RegExp(value.source, value.flags), handler, remaining: options?.times ?? Infinity };
  rules.unshift(rule); await publish();
  const dispose = async () => { const index = rules.indexOf(rule); if (index >= 0) rules.splice(index, 1); await publish(); };
  return { dispose, [Symbol.asyncDispose]: dispose };
 };
 context.unroute = async (matcher, handler) => { const value = pattern(matcher); for (let i = rules.length - 1; i >= 0; i--) { const other = pattern(rules[i].matcher); if (other.source === value.source && other.flags === value.flags && (!handler || rules[i].handler === handler)) rules.splice(i, 1); } await publish(); };
 context.unrouteAll = async () => { rules.length = 0; await publish(); };
 const offline = context.setOffline.bind(context);
 context.setOffline = async value => {
  await application.evaluate(({ BrowserWindow }, value) => { BrowserWindow.getAllWindows()[0].webContents.session.enableNetworkEmulation({ offline: value }); }, value);
  await offline(value);
 };
 return async () => { (context as any).emit = emitContext; (page as any).emit = emitPage; server.closeAllConnections(); pending.clear(); await new Promise<void>(done => server.close(() => done())); };
}
