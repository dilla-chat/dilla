//! The signed update feed (release plan Task 9's format; beta.5 `update.ts` `verifyRelease`): pure checks, no I/O.
//!
//! The order is fixed (Review Focus 8): the sizes of the feed and of its signature, then the Ed25519 signature over the
//! exact feed bytes under one of the pinned keys (`clients/desktop/release-trust.json`, compiled in), and only then is
//! the document parsed: format 1, the pinned channel, a `<x.y.z>-<channel>.<n>` version, the dates (published at most
//! 5 minutes ahead, not expired, valid for at most 93 days, as `desktopdownload.Verify`), and the assets. [`select`]
//! then takes only a version above the running one (never a downgrade) and the asset for this platform; [`Checked`]
//! holds a download to the asset's size and SHA-512.

use std::sync::OnceLock;

use ed25519_dalek::{Signature, VerifyingKey};
use jiff::Timestamp;
use semver::Version;
use serde::Deserialize;
use sha2::{Digest as _, Sha512};

/// The pins: the release metadata keys and the channel (Task 9 writes the same file).
const TRUST_JSON: &str = include_str!("../../../release-trust.json");

/// The largest feed accepted (beta.5: 1 MiB).
pub const MAX_FEED: usize = 1024 * 1024;
/// The largest signature file accepted: 88 base64 characters and a line break fit with room to spare.
pub const MAX_SIG: usize = 128;
/// The largest asset accepted. The biggest GPUI asset (the macOS zip) is a few hundred MiB.
pub const MAX_ASSET: u64 = 1024 * 1024 * 1024;
/// The most assets one release lists.
pub const MAX_ASSETS: usize = 32;
/// How far ahead of this device's clock a feed may be published.
const SKEW_S: i64 = 5 * 60;
/// The longest a feed may be valid (Task 9 signs for 90 days; `desktopdownload` allows 93).
const MAX_VALID_S: i64 = 93 * 86_400;
/// The release downloads: `<base><version>/<asset>` (release plan D1: tags `desktop-gpui-v<version>`).
pub const DOWNLOAD_BASE: &str =
    "https://github.com/dilla-chat/dilla/releases/download/desktop-gpui-v";

/// Why a feed was refused. Every variant means the same to the person (the update did not pass its checks); the
/// variants are for the log and the tests.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FeedError {
    /// The feed or its signature is empty or too large.
    Size,
    /// The signature is malformed or does not verify under any pinned key.
    Signature,
    /// The signed bytes are not a format-1 document.
    Document,
    /// The document is for another channel.
    Channel,
    /// The version is not a release of this channel.
    Version,
    /// The feed expired, or its dates are malformed.
    Dates,
    /// An asset is malformed, too large or listed twice.
    Assets,
}

impl std::fmt::Display for FeedError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Size => "the feed or its signature is empty or too large",
            Self::Signature => "the feed signature does not verify under the pinned keys",
            Self::Document => "the feed is not a format-1 release document",
            Self::Channel => "the feed is for another channel",
            Self::Version => "the feed version is not a release of this channel",
            Self::Dates => "the feed expired or its dates are invalid",
            Self::Assets => "the feed assets are invalid",
        })
    }
}

impl std::error::Error for FeedError {}

/// The pinned keys and channel.
#[derive(Clone, Debug)]
pub struct Trust {
    keys: Vec<VerifyingKey>,
    channel: String,
}

impl Trust {
    /// The pins compiled in. A malformed `release-trust.json` is a build defect (`the_pinned_trust_is_key_a`).
    pub fn pinned() -> &'static Trust {
        static PINNED: OnceLock<Trust> = OnceLock::new();
        PINNED.get_or_init(|| Trust::parse(TRUST_JSON).expect("release-trust.json is valid"))
    }

    /// `{ "metadataKeys": [<SPKI PEM>, ...], "channel": "<name>" }`.
    pub fn parse(json: &str) -> Result<Self, String> {
        #[derive(Deserialize)]
        #[serde(rename_all = "camelCase")]
        struct File {
            metadata_keys: Vec<String>,
            channel: String,
        }
        let file: File = serde_json::from_str(json).map_err(|e| e.to_string())?;
        if file.metadata_keys.is_empty() {
            return Err("no metadata key".into());
        }
        if !is_channel(&file.channel) || file.channel == "beta" {
            return Err("the channel must be a lowercase name other than beta".into());
        }
        let keys = file
            .metadata_keys
            .iter()
            .map(|pem| spki_pem_key(pem))
            .collect::<Result<_, _>>()?;
        Ok(Self {
            keys,
            channel: file.channel,
        })
    }

    /// A trust over `keys` for `channel` (the tests).
    pub fn new(keys: Vec<VerifyingKey>, channel: &str) -> Self {
        Self {
            keys,
            channel: channel.to_owned(),
        }
    }

    pub fn channel(&self) -> &str {
        &self.channel
    }

    pub fn keys(&self) -> &[VerifyingKey] {
        &self.keys
    }
}

fn is_channel(s: &str) -> bool {
    !s.is_empty() && s.len() <= 16 && s.bytes().all(|b| b.is_ascii_lowercase())
}

/// The DER prefix of an Ed25519 SubjectPublicKeyInfo (RFC 8410): SEQUENCE { SEQUENCE { OID 1.3.101.112 }, BIT STRING }.
const ED25519_SPKI_PREFIX: [u8; 12] = [
    0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00,
];

/// One `PUBLIC KEY` PEM block holding an Ed25519 SPKI, as `desktopdownload.ParseKeys` reads it.
fn spki_pem_key(pem: &str) -> Result<VerifyingKey, String> {
    let body = pem
        .strip_prefix("-----BEGIN PUBLIC KEY-----\n")
        .and_then(|s| {
            s.trim_end_matches('\n')
                .strip_suffix("-----END PUBLIC KEY-----")
        })
        .ok_or("a metadata key is not one PEM public key")?;
    let b64: String = body.split('\n').collect();
    let der = base64_decode(&b64).ok_or("a metadata key is not base64")?;
    let raw = der
        .strip_prefix(&ED25519_SPKI_PREFIX[..])
        .filter(|k| k.len() == 32)
        .ok_or("a metadata key is not an Ed25519 SPKI")?;
    let mut bytes = [0u8; 32];
    bytes.copy_from_slice(raw);
    VerifyingKey::from_bytes(&bytes).map_err(|_| "a metadata key is not a valid Ed25519 key".into())
}

/// Strict standard base64 (RFC 4648 §4): padded, no whitespace, and canonical (the unused bits of the last character
/// are zero), so one value has one spelling, as Go's `StdEncoding.Strict()` and beta.5's re-encoding check.
pub fn base64_decode(s: &str) -> Option<Vec<u8>> {
    fn value(c: u8) -> Option<u32> {
        Some(match c {
            b'A'..=b'Z' => c - b'A',
            b'a'..=b'z' => c - b'a' + 26,
            b'0'..=b'9' => c - b'0' + 52,
            b'+' => 62,
            b'/' => 63,
            _ => return None,
        } as u32)
    }
    let bytes = s.as_bytes();
    if bytes.is_empty() || !bytes.len().is_multiple_of(4) {
        return None;
    }
    let pad = bytes.iter().rev().take_while(|&&b| b == b'=').count();
    if pad > 2 {
        return None;
    }
    let mut out = Vec::with_capacity(bytes.len() / 4 * 3);
    for (i, quad) in bytes.chunks(4).enumerate() {
        let last = i == bytes.len() / 4 - 1;
        let n = if last { 4 - pad } else { 4 };
        let mut acc = 0u32;
        for (j, &c) in quad.iter().enumerate() {
            let v = if j < n { value(c)? } else { 0 };
            acc |= v << (18 - 6 * j);
        }
        let take = n - 1;
        // The bits the last character carries beyond the output bytes must be zero.
        if take < 3 && acc & ((1u32 << (8 * (3 - take))) - 1) != 0 {
            return None;
        }
        out.extend_from_slice(&acc.to_be_bytes()[1..1 + take]);
    }
    Some(out)
}

/// One asset of a verified release.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Asset {
    pub name: String,
    pub size: u64,
    pub sha512: [u8; 64],
}

/// A verified release document.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Release {
    pub version: Version,
    pub expires_at: Timestamp,
    pub assets: Vec<Asset>,
}

/// An update this app may take: a newer version and this platform's asset in it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Update {
    pub version: Version,
    pub asset: Asset,
}

impl Update {
    /// The asset's download (`desktop-gpui-v<version>/<name>` on the project's releases).
    pub fn url(&self) -> String {
        format!("{DOWNLOAD_BASE}{}/{}", self.version, self.asset.name)
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Document {
    format: u64,
    channel: String,
    version: String,
    published_at: String,
    expires_at: String,
    assets: Vec<DocAsset>,
}

#[derive(Deserialize)]
struct DocAsset {
    name: String,
    size: u64,
    sha512: String,
}

/// Checks `feed` and its detached signature `sig` under `trust` at `now`; see the module comment for the order.
pub fn verify(
    feed: &[u8],
    sig: &[u8],
    trust: &Trust,
    now: Timestamp,
) -> Result<Release, FeedError> {
    // 1. Sizes.
    if feed.is_empty() || feed.len() > MAX_FEED || sig.is_empty() || sig.len() > MAX_SIG {
        return Err(FeedError::Size);
    }
    // 2. The signature, over the exact bytes, before anything reads them.
    let sig = sig.strip_suffix(b"\n").unwrap_or(sig);
    let sig = std::str::from_utf8(sig).map_err(|_| FeedError::Signature)?;
    if sig.len() != 88 || !sig.ends_with("==") {
        return Err(FeedError::Signature);
    }
    let raw: [u8; 64] = base64_decode(sig)
        .and_then(|v| v.try_into().ok())
        .ok_or(FeedError::Signature)?;
    let signature = Signature::from_bytes(&raw);
    if !trust
        .keys
        .iter()
        .any(|k| k.verify_strict(feed, &signature).is_ok())
    {
        return Err(FeedError::Signature);
    }
    // 3. The document.
    let doc: Document = serde_json::from_slice(feed).map_err(|_| FeedError::Document)?;
    if doc.format != 1 {
        return Err(FeedError::Document);
    }
    if doc.channel != trust.channel {
        return Err(FeedError::Channel);
    }
    let version = channel_version(&doc.version, &trust.channel).ok_or(FeedError::Version)?;
    let published = utc(&doc.published_at).ok_or(FeedError::Dates)?;
    let expires = utc(&doc.expires_at).ok_or(FeedError::Dates)?;
    let (p, e, n) = (secs(published), secs(expires), secs(now));
    if p > n + SKEW_S || e <= n || e <= p || e - p > MAX_VALID_S {
        return Err(FeedError::Dates);
    }
    if doc.assets.is_empty() || doc.assets.len() > MAX_ASSETS {
        return Err(FeedError::Assets);
    }
    let prefix = format!("dilla-{}-", doc.version);
    let mut assets: Vec<Asset> = Vec::with_capacity(doc.assets.len());
    for a in doc.assets {
        let name_ok = a.name.len() <= 128
            && a.name.starts_with(&prefix)
            && !a.name.contains("..")
            && a.name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b'_'));
        let sha512: Option<[u8; 64]> = (a.sha512.len() == 88)
            .then(|| base64_decode(&a.sha512))
            .flatten()
            .and_then(|v| v.try_into().ok());
        let Some(sha512) = sha512 else {
            return Err(FeedError::Assets);
        };
        if !name_ok || a.size == 0 || a.size > MAX_ASSET || assets.iter().any(|b| b.name == a.name)
        {
            return Err(FeedError::Assets);
        }
        assets.push(Asset {
            name: a.name,
            size: a.size,
            sha512,
        });
    }
    Ok(Release {
        version,
        expires_at: expires,
        assets,
    })
}

/// `<major>.<minor>.<patch>-<channel>.<n>`, every number without a leading zero (Task 11's
/// `^\d+\.\d+\.\d+-alpha\.\d+$`, made strict), parsed by semver.
fn channel_version(s: &str, channel: &str) -> Option<Version> {
    let (core, pre) = s.split_once('-')?;
    let n = pre.strip_prefix(channel)?.strip_prefix('.')?;
    let numbers: Vec<&str> = core.split('.').chain(std::iter::once(n)).collect();
    let canonical = |d: &&str| {
        !d.is_empty()
            && d.len() <= 9
            && d.bytes().all(|b| b.is_ascii_digit())
            && (*d == "0" || !d.starts_with('0'))
    };
    if numbers.len() != 4 || !numbers.iter().all(canonical) {
        return None;
    }
    Version::parse(s).ok()
}

/// `YYYY-MM-DDTHH:MM:SS[.fraction]Z` (the shape `desktopdownload` and beta.5 accept).
fn utc(s: &str) -> Option<Timestamp> {
    let b = s.as_bytes();
    let shape = b.len() >= 20
        && b.iter().enumerate().take(19).all(|(i, &c)| match i {
            4 | 7 => c == b'-',
            10 => c == b'T',
            13 | 16 => c == b':',
            _ => c.is_ascii_digit(),
        })
        && b[b.len() - 1] == b'Z'
        && (b.len() == 20
            || (b[19] == b'.'
                && (22..=30).contains(&b.len())
                && b[20..b.len() - 1].iter().all(u8::is_ascii_digit)));
    if !shape {
        return None;
    }
    s.parse::<Timestamp>().ok()
}

/// Whole unix seconds of an instant, rounded down.
fn secs(t: Timestamp) -> i64 {
    t.as_millisecond().div_euclid(1000)
}

/// The update in `release` for an app at `current` whose platform asset ends in `suffix` (`linux-x64.AppImage`):
/// `None` unless the release is strictly newer (no downgrade, no reinstall) and lists that asset.
pub fn select(release: &Release, current: &Version, suffix: &str) -> Option<Update> {
    if release.version <= *current {
        return None;
    }
    let name = format!("dilla-{}-{suffix}", release.version);
    let asset = release.assets.iter().find(|a| a.name == name)?.clone();
    Some(Update {
        version: release.version.clone(),
        asset,
    })
}

/// Why a download was refused.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum DownloadError {
    /// More or fewer bytes than the feed's size.
    Size,
    /// The bytes' SHA-512 is not the feed's.
    Hash,
}

impl std::fmt::Display for DownloadError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Size => "the download's size is not the feed's",
            Self::Hash => "the download's SHA-512 is not the feed's",
        })
    }
}

impl std::error::Error for DownloadError {}

/// A download held to its asset: every chunk is counted and hashed as it arrives, and more bytes than the feed's size
/// stop it at once.
pub struct Checked<'a> {
    asset: &'a Asset,
    seen: u64,
    hash: Sha512,
}

impl<'a> Checked<'a> {
    pub fn new(asset: &'a Asset) -> Self {
        Self {
            asset,
            seen: 0,
            hash: Sha512::new(),
        }
    }

    /// Takes the next chunk; refuses one that goes past the asset's size.
    pub fn push(&mut self, chunk: &[u8]) -> Result<(), DownloadError> {
        self.seen = self
            .seen
            .checked_add(chunk.len() as u64)
            .filter(|&n| n <= self.asset.size)
            .ok_or(DownloadError::Size)?;
        self.hash.update(chunk);
        Ok(())
    }

    /// The whole download: exactly the asset's size, with its SHA-512.
    pub fn finish(self) -> Result<(), DownloadError> {
        if self.seen != self.asset.size {
            return Err(DownloadError::Size);
        }
        let digest = self.hash.finalize();
        if digest.as_slice() != self.asset.sha512.as_slice() {
            return Err(DownloadError::Hash);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use ed25519_dalek::{Signer as _, SigningKey};

    use super::*;

    const NOW: &str = "2026-11-16T12:00:00Z";

    fn now() -> Timestamp {
        utc(NOW).unwrap()
    }

    fn key(seed: u8) -> SigningKey {
        SigningKey::from_bytes(&[seed; 32])
    }

    fn trust() -> Trust {
        Trust::new(vec![key(1).verifying_key()], "alpha")
    }

    fn b64(bytes: &[u8]) -> String {
        const A: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
        let mut out = String::new();
        for c in bytes.chunks(3) {
            let n = (u32::from(c[0]) << 16)
                | (u32::from(*c.get(1).unwrap_or(&0)) << 8)
                | u32::from(*c.get(2).unwrap_or(&0));
            for i in 0..4 {
                if i <= c.len() {
                    out.push(A[(n >> (18 - 6 * i) & 63) as usize] as char);
                } else {
                    out.push('=');
                }
            }
        }
        out
    }

    fn sha(bytes: &[u8]) -> String {
        b64(&Sha512::digest(bytes))
    }

    /// Task 9's canonical bytes: `JSON.stringify(doc) + "\n"`.
    fn doc(channel: &str, version: &str, published: &str, expires: &str) -> Vec<u8> {
        format!(
            r#"{{"format":1,"channel":"{channel}","version":"{version}","publishedAt":"{published}","expiresAt":"{expires}","assets":[{{"name":"dilla-{version}-linux-x64.AppImage","size":5,"sha512":"{}"}},{{"name":"dilla-{version}-mac-arm64.zip","size":3,"sha512":"{}"}}]}}"#,
            sha(b"hello"),
            sha(b"mac"),
        )
        .into_bytes()
        .into_iter()
        .chain(*b"\n")
        .collect()
    }

    fn good() -> Vec<u8> {
        doc(
            "alpha",
            "0.2.0-alpha.2",
            "2026-11-15T10:00:00.123Z",
            "2027-02-13T10:00:00.123Z",
        )
    }

    fn sign(feed: &[u8], with: &SigningKey) -> Vec<u8> {
        format!("{}\n", b64(&with.sign(feed).to_bytes())).into_bytes()
    }

    fn current() -> Version {
        Version::parse("0.2.0-alpha.1").unwrap()
    }

    #[test]
    fn the_pinned_trust_is_key_a() {
        let pinned = Trust::pinned();
        assert_eq!(pinned.channel(), "alpha");
        assert_eq!(pinned.keys().len(), 1);
        // internal/desktopdownload/keys.go: beta.5 metadata key slot A.
        let spki =
            base64_decode("MCowBQYDK2VwAyEAPT530pTGlCCbjSNbXeajnERzU1k9JL2KjSYupHPZjrw=").unwrap();
        assert_eq!(pinned.keys()[0].as_bytes()[..], spki[12..]);
    }

    #[test]
    fn a_trust_file_for_beta_or_without_keys_is_refused() {
        let pem = "-----BEGIN PUBLIC KEY-----\\nMCowBQYDK2VwAyEAPT530pTGlCCbjSNbXeajnERzU1k9JL2KjSYupHPZjrw=\\n-----END PUBLIC KEY-----\\n";
        assert!(
            Trust::parse(&format!(
                r#"{{"metadataKeys":["{pem}"],"channel":"alpha"}}"#
            ))
            .is_ok()
        );
        assert!(
            Trust::parse(&format!(r#"{{"metadataKeys":["{pem}"],"channel":"beta"}}"#)).is_err()
        );
        assert!(Trust::parse(r#"{"metadataKeys":[],"channel":"alpha"}"#).is_err());
        assert!(Trust::parse(r#"{"metadataKeys":["x"],"channel":"alpha"}"#).is_err());
    }

    #[test]
    fn base64_is_strict() {
        assert_eq!(base64_decode("aGVsbG8=").unwrap(), b"hello");
        assert_eq!(base64_decode("aGk=").unwrap(), b"hi");
        assert_eq!(base64_decode("aGVs").unwrap(), b"hel");
        for bad in [
            "", "aGVsbG8", "aGVsbG9=", "aGk", "a===", "aG k=", "aGk=\n", "aGVsbG8-",
        ] {
            assert_eq!(base64_decode(bad), None, "{bad:?}");
        }
        let all: Vec<u8> = (0..=255).collect();
        assert_eq!(base64_decode(&b64(&all)).unwrap(), all);
    }

    #[test]
    fn a_good_feed_verifies_and_selects_this_platforms_asset() {
        let feed = good();
        let release = verify(&feed, &sign(&feed, &key(1)), &trust(), now()).unwrap();
        assert_eq!(release.version, Version::parse("0.2.0-alpha.2").unwrap());
        let update = select(&release, &current(), "linux-x64.AppImage").unwrap();
        assert_eq!(update.asset.name, "dilla-0.2.0-alpha.2-linux-x64.AppImage");
        assert_eq!(update.asset.size, 5);
        assert_eq!(
            update.url(),
            "https://github.com/dilla-chat/dilla/releases/download/desktop-gpui-v0.2.0-alpha.2/dilla-0.2.0-alpha.2-linux-x64.AppImage"
        );
        // No asset for this platform: no update.
        assert_eq!(select(&release, &current(), "win-x64-setup.exe"), None);
    }

    #[test]
    fn a_bad_signature_is_refused() {
        let feed = good();
        // Another key.
        assert_eq!(
            verify(&feed, &sign(&feed, &key(2)), &trust(), now()),
            Err(FeedError::Signature)
        );
        // Malformed signatures.
        for sig in [&b"AAAA\n"[..], &[b'A'; 88][..], &b"=="[..]] {
            assert_eq!(
                verify(&feed, sig, &trust(), now()),
                Err(FeedError::Signature)
            );
        }
        // A signature of a key that is not pinned, with the pinned set empty.
        let none = Trust::new(Vec::new(), "alpha");
        assert_eq!(
            verify(&feed, &sign(&feed, &key(1)), &none, now()),
            Err(FeedError::Signature)
        );
    }

    #[test]
    fn tampering_is_refused_before_parsing() {
        let feed = good();
        let sig = sign(&feed, &key(1));
        // One changed byte, an added byte, the newline dropped: the signature no longer verifies.
        let mut changed = feed.clone();
        let at = changed.iter().position(|&b| b == b'2').unwrap();
        changed[at] = b'9';
        let mut longer = feed.clone();
        longer.push(b' ');
        let shorter = feed[..feed.len() - 1].to_vec();
        for f in [changed, longer, shorter] {
            assert_eq!(verify(&f, &sig, &trust(), now()), Err(FeedError::Signature));
        }
        // Signed bytes that are not JSON fail only after the signature.
        let junk = b"not json\n".to_vec();
        assert_eq!(
            verify(&junk, &sign(&junk, &key(1)), &trust(), now()),
            Err(FeedError::Document)
        );
    }

    #[test]
    fn oversized_feeds_and_signatures_are_refused() {
        let big = vec![b' '; MAX_FEED + 1];
        assert_eq!(
            verify(&big, &sign(&big, &key(1)), &trust(), now()),
            Err(FeedError::Size)
        );
        let feed = good();
        let mut sig = sign(&feed, &key(1));
        sig.resize(MAX_SIG + 1, b'\n');
        assert_eq!(verify(&feed, &sig, &trust(), now()), Err(FeedError::Size));
        assert_eq!(verify(&[], &sig, &trust(), now()), Err(FeedError::Size));
    }

    #[test]
    fn another_channel_is_refused() {
        for channel in ["beta", "stable", "Alpha"] {
            let feed = doc(
                channel,
                "0.2.0-alpha.2",
                "2026-11-15T10:00:00Z",
                "2027-02-13T10:00:00Z",
            );
            assert_eq!(
                verify(&feed, &sign(&feed, &key(1)), &trust(), now()),
                Err(FeedError::Channel),
                "{channel}"
            );
        }
        // A beta.5 version in an alpha feed.
        let feed = doc(
            "alpha",
            "0.1.0-beta.5",
            "2026-11-15T10:00:00Z",
            "2027-02-13T10:00:00Z",
        );
        assert_eq!(
            verify(&feed, &sign(&feed, &key(1)), &trust(), now()),
            Err(FeedError::Version)
        );
    }

    #[test]
    fn versions_must_be_canonical_channel_releases() {
        for bad in [
            "0.2.0",
            "0.2.0-alpha",
            "0.2.0-alpha.01",
            "00.2.0-alpha.1",
            "0.2.0-alpha.1+build",
            "0.2-alpha.1",
            "0.2.0-alpha.1.1",
        ] {
            assert_eq!(channel_version(bad, "alpha"), None, "{bad}");
        }
        assert!(channel_version("10.20.30-alpha.40", "alpha").is_some());
    }

    #[test]
    fn expired_or_ill_dated_feeds_are_refused() {
        for (published, expires) in [
            // Expired a second ago.
            ("2026-08-18T11:59:59Z", "2026-11-16T11:59:59Z"),
            // Expires exactly now.
            ("2026-08-18T12:00:00Z", NOW),
            // Published more than 5 minutes ahead.
            ("2026-11-16T12:05:01Z", "2027-01-16T12:00:00Z"),
            // Valid for more than 93 days.
            ("2026-11-15T12:00:00Z", "2027-02-16T12:00:01Z"),
            // Not UTC, or not the accepted shape.
            ("2026-11-15T10:00:00+00:00", "2027-02-13T10:00:00Z"),
            ("2026-11-15 10:00:00Z", "2027-02-13T10:00:00Z"),
            ("2026-11-15T10:00:00Z", "2027-02-30T10:00:00Z"),
        ] {
            let feed = doc("alpha", "0.2.0-alpha.2", published, expires);
            assert_eq!(
                verify(&feed, &sign(&feed, &key(1)), &trust(), now()),
                Err(FeedError::Dates),
                "{published} {expires}"
            );
        }
        // Published within the 5 minutes of skew is fine.
        let feed = doc(
            "alpha",
            "0.2.0-alpha.2",
            "2026-11-16T12:05:00Z",
            "2027-01-16T12:00:00Z",
        );
        assert!(verify(&feed, &sign(&feed, &key(1)), &trust(), now()).is_ok());
    }

    #[test]
    fn downgrades_and_the_same_version_are_not_updates() {
        let feed = good();
        let release = verify(&feed, &sign(&feed, &key(1)), &trust(), now()).unwrap();
        for running in [
            "0.2.0-alpha.2",
            "0.2.0-alpha.3",
            "0.2.0-alpha.10",
            "0.2.0",
            "0.3.0-alpha.1",
        ] {
            assert_eq!(
                select(
                    &release,
                    &Version::parse(running).unwrap(),
                    "linux-x64.AppImage"
                ),
                None,
                "{running}"
            );
        }
        // alpha.10 sorts above alpha.9 (numeric identifiers).
        let feed = doc(
            "alpha",
            "0.2.0-alpha.10",
            "2026-11-15T10:00:00Z",
            "2027-02-13T10:00:00Z",
        );
        let release = verify(&feed, &sign(&feed, &key(1)), &trust(), now()).unwrap();
        assert!(
            select(
                &release,
                &Version::parse("0.2.0-alpha.9").unwrap(),
                "mac-arm64.zip"
            )
            .is_some()
        );
    }

    #[test]
    fn malformed_assets_are_refused() {
        let base = String::from_utf8(good()).unwrap();
        let hello = sha(b"hello");
        for (from, to) in [
            ("\"size\":5", "\"size\":0"),
            ("\"size\":5", "\"size\":1073741825"),
            ("\"size\":5", "\"size\":-5"),
            ("\"size\":5", "\"size\":5.5"),
            (hello.as_str(), "AAAA"),
            ("dilla-0.2.0-alpha.2-linux", "dilla-0.2.0-alpha.1-linux"),
            (
                "dilla-0.2.0-alpha.2-linux-x64.AppImage",
                "dilla-0.2.0-alpha.2-../x.AppImage",
            ),
            (
                "dilla-0.2.0-alpha.2-mac-arm64.zip",
                "dilla-0.2.0-alpha.2-linux-x64.AppImage",
            ),
        ] {
            let feed = base.replacen(from, to, 1).into_bytes();
            assert_ne!(feed, base.as_bytes(), "{from}");
            let got = verify(&feed, &sign(&feed, &key(1)), &trust(), now());
            assert!(
                matches!(got, Err(FeedError::Assets | FeedError::Document)),
                "{from} -> {to}: {got:?}"
            );
        }
    }

    #[test]
    fn a_download_must_have_the_feeds_size_and_hash() {
        let asset = Asset {
            name: "dilla-0.2.0-alpha.2-linux-x64.AppImage".into(),
            size: 5,
            sha512: Sha512::digest(b"hello").into(),
        };
        let mut ok = Checked::new(&asset);
        ok.push(b"hel").unwrap();
        ok.push(b"lo").unwrap();
        assert_eq!(ok.finish(), Ok(()));
        // Too long: stopped at the chunk that goes past the size.
        let mut long = Checked::new(&asset);
        long.push(b"hell").unwrap();
        assert_eq!(long.push(b"o!"), Err(DownloadError::Size));
        // Too short.
        let mut short = Checked::new(&asset);
        short.push(b"hell").unwrap();
        assert_eq!(short.finish(), Err(DownloadError::Size));
        // The right size, other bytes.
        let mut other = Checked::new(&asset);
        other.push(b"jello").unwrap();
        assert_eq!(other.finish(), Err(DownloadError::Hash));
    }

    /// The feed's date parser, pinned to what the chrono-based one answered: exact seconds for the accepted shape, `None` for a
    /// date that does not exist, and the leap second read as the second before it.
    #[test]
    fn feed_dates_parse_to_exact_unix_seconds() {
        let secs = |s: &str| utc(s).map(super::secs);
        assert_eq!(secs("1970-01-01T00:00:00Z"), Some(0));
        assert_eq!(secs("2026-11-16T12:00:00Z"), Some(1_794_830_400));
        assert_eq!(secs("2026-11-16T12:00:00.999Z"), Some(1_794_830_400));
        assert_eq!(secs("2026-11-16T12:00:00.123456789Z"), Some(1_794_830_400));
        assert_eq!(secs("2028-02-29T00:00:00Z"), Some(1_835_395_200));
        // A leap second is the second it follows.
        assert_eq!(secs("2026-12-31T23:59:60Z"), secs("2026-12-31T23:59:59Z"));
        // Whole seconds round down before 1970 too.
        assert_eq!(secs("1969-12-31T23:59:59.5Z"), Some(-1));
        // Dates and times that do not exist.
        for bad in [
            "2027-02-29T00:00:00Z",
            "2026-04-31T00:00:00Z",
            "2026-13-01T00:00:00Z",
            "2026-00-10T00:00:00Z",
            "2026-11-16T24:00:00Z",
            "2026-11-16T12:60:00Z",
            "2026-11-16T12:00:61Z",
            "2026-11-16T12:00:00.Z",
            "2026-11-16T12:00:00z",
        ] {
            assert_eq!(utc(bad), None, "{bad}");
        }
    }
}
