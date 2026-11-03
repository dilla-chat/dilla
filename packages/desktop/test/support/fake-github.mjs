import { createHash } from 'node:crypto';

/**
 * An in-memory GitHub for the release scripts: the REST endpoints they call,
 * uploads.github.com and release downloads, behind one `fetch`. Every request
 * is logged (method, URL, redirect mode) so tests can assert order and transport.
 */
const REPOSITORY = 'dilla-chat/dilla';
const API = `https://api.github.com/repos/${REPOSITORY}`;
const sha256 = bytes => 'sha256:' + createHash('sha256').update(bytes).digest('hex');
const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const missing = () => new Response('{"message":"Not Found"}', { status: 404 });

export function fakeGithub({ tags = {}, runs = {}, jobs = {}, releases = [], immutable = true, onPublish } = {}) {
 const state = { tags: { ...tags }, runs, jobs, releases: [], immutable, log: [], nextRelease: 100, nextAsset: 1000, onPublish };
 const view = release => ({ ...release, assets: release.assets.map(({ bytes, ...asset }) => ({ ...asset })) });
 const uploadUrl = id => `https://uploads.github.com/repos/${REPOSITORY}/releases/${id}/assets{?name,label}`;
 function addRelease(fields) {
  const release = { id: state.nextRelease++, tag_name: fields.tag_name, name: fields.name ?? '', body: fields.body ?? '', draft: fields.draft ?? false, prerelease: fields.prerelease ?? false,
   target_commitish: fields.target_commitish ?? 'main', immutable: fields.immutable ?? false, assets: [],
   // Releases the workflow creates are authored by the Actions bot; a test can name a person instead.
   author: fields.author ?? { login: 'github-actions[bot]', type: 'Bot' } };
  release.upload_url = uploadUrl(release.id);
  state.releases.push(release);
  for (const [name, bytes] of Object.entries(fields.files ?? {})) addAsset(release, name, Buffer.from(bytes));
  return release;
 }
 function addAsset(release, name, bytes) {
  const asset = { id: state.nextAsset++, name, size: bytes.length, digest: sha256(bytes), state: 'uploaded', bytes,
   browser_download_url: `https://github.com/${REPOSITORY}/releases/download/${release.tag_name}/${name}` };
  release.assets.push(asset);
  return asset;
 }
 for (const release of releases) addRelease(release);
 const byId = id => state.releases.find(release => release.id === Number(id));
 const page = (list, url) => {
  const per = Number(url.searchParams.get('per_page') ?? 30), number = Number(url.searchParams.get('page') ?? 1);
  return list.slice((number - 1) * per, number * per);
 };

 async function fetchImpl(input, init = {}) {
  const url = new URL(String(input)), method = init.method ?? 'GET';
  state.log.push({ method, url: url.href, redirect: init.redirect, body: typeof init.body === 'string' ? JSON.parse(init.body) : undefined });
  const body = typeof init.body === 'string' ? JSON.parse(init.body) : undefined;
  if (url.origin === 'https://uploads.github.com') {
   const match = new RegExp(`^/repos/${REPOSITORY}/releases/(\\d+)/assets$`).exec(url.pathname), release = match && byId(match[1]);
   if (method !== 'POST' || !release) return missing();
   const name = url.searchParams.get('name');
   if ((release.immutable && !release.draft) || release.assets.some(asset => asset.name === name)) return json({ message: 'Validation Failed' }, 422);
   const { bytes, ...asset } = addAsset(release, name, Buffer.from(init.body));
   return json(asset, 201);
  }
  if (url.origin === 'https://github.com') {
   const match = new RegExp(`^/${REPOSITORY}/releases/download/([^/]+)/([^/]+)$`).exec(url.pathname);
   const release = match && state.releases.find(item => item.tag_name === decodeURIComponent(match[1]) && !item.draft);
   const asset = release?.assets.find(item => item.name === decodeURIComponent(match[2]));
   if (!asset) return missing();
   // Release downloads redirect to the asset host; the final URL is what a real fetch reports.
   const response = new Response(asset.bytes, { status: 200 });
   Object.defineProperty(response, 'url', { value: `https://release-assets.githubusercontent.com/github-production-release-asset/${asset.id}` });
   return response;
  }
  if (!url.href.startsWith(API + '/')) return missing();
  const path = url.pathname.slice(`/repos/${REPOSITORY}`.length);
  let match;
  if ((match = /^\/git\/ref\/tags\/(.+)$/.exec(path)) && method === 'GET') {
   const tag = decodeURIComponent(match[1]), sha = state.tags[tag];
   return sha ? json({ ref: `refs/tags/${tag}`, object: { type: 'commit', sha } }) : missing();
  }
  if ((match = /^\/actions\/runs\/(\d+)$/.exec(path)) && method === 'GET') return state.runs[match[1]] ? json(state.runs[match[1]]) : missing();
  if ((match = /^\/actions\/runs\/(\d+)\/jobs$/.exec(path)) && method === 'GET') return state.jobs[match[1]] ? json({ jobs: page(state.jobs[match[1]], url) }) : missing();
  if ((match = /^\/releases\/tags\/(.+)$/.exec(path)) && method === 'GET') {
   const release = state.releases.find(item => item.tag_name === decodeURIComponent(match[1]) && !item.draft);
   return release ? json(view(release)) : missing();
  }
  if (path === '/releases' && method === 'GET') return json(page(state.releases.map(view), url));
  if (path === '/releases' && method === 'POST') return json(view(addRelease({ ...body, immutable: false })), 201);
  if ((match = /^\/releases\/(\d+)$/.exec(path))) {
   const release = byId(match[1]);
   if (!release) return missing();
   if (method === 'GET') return json(view(release));
   if (method === 'PATCH') {
    if (release.immutable && !release.draft) return json({ message: 'Immutable' }, 422);
    const publishing = release.draft && body.draft === false;
    Object.assign(release, body);
    if (publishing) { release.immutable = state.immutable; await state.onPublish?.(state, release); }
    return json(view(release));
   }
  }
  if ((match = /^\/releases\/(\d+)\/assets$/.exec(path)) && method === 'GET') {
   const release = byId(match[1]);
   return release ? json(page(view(release).assets, url)) : missing();
  }
  if ((match = /^\/releases\/assets\/(\d+)$/.exec(path)) && method === 'DELETE') {
   const release = state.releases.find(item => item.assets.some(asset => asset.id === Number(match[1])));
   if (!release) return missing();
   if (release.immutable && !release.draft) return json({ message: 'Immutable' }, 422);
   release.assets = release.assets.filter(asset => asset.id !== Number(match[1]));
   return new Response(null, { status: 204 });
  }
  return missing();
 }
 return { fetch: fetchImpl, state, addRelease, addAsset, sha256 };
}
