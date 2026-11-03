import { sign } from 'node:crypto';

export function sparkleVersion(version) {
 if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-beta\.(0|[1-9]\d*))?$/.test(version)) throw new Error('Invalid desktop version');
 return version.replace('-beta.', 'b');
}
export function signedAppcast(xml, key) {
 const content = Buffer.from(xml, 'utf8');
 // Sparkle 2.9+ SPUExtractSignedFeed / common_cli/Signing.swift format.
 return Buffer.concat([content, Buffer.from(`<!-- sparkle-signatures:\nedSignature: ${sign(null, content, key).toString('base64')}\nlength: ${content.length}\n-->\n`)]);
}
export function appcast(release, artifact, signature) {
 const safe = value => String(value).replaceAll('&', '&amp;').replaceAll('"', '&quot;').replaceAll('<', '&lt;').replaceAll('>', '&gt;');
 return `<?xml version="1.0" encoding="utf-8"?>\n<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle"><channel><title>Dilla desktop ${safe(release.channel)}</title><item><title>Dilla ${safe(release.version)}</title><sparkle:version>${sparkleVersion(release.version)}</sparkle:version><sparkle:shortVersionString>${safe(release.version)}</sparkle:shortVersionString><sparkle:minimumSystemVersion>13.0.0</sparkle:minimumSystemVersion><pubDate>${new Date(release.publishedAt).toUTCString()}</pubDate><enclosure url="${safe(artifact.url)}" sparkle:edSignature="${safe(signature)}" length="${artifact.size}" type="application/octet-stream" /></item></channel></rss>\n`;
}

