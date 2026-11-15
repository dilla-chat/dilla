//! The instances on this machine: up to 32 copies of the client, each with its own server, data dir
//! (`<root>/instances/<n>`) and keychain account. The list is `<root>/instances.json`, which holds
//! the root's install id, instance ids and server URLs only, never a secret.
//!
//! The install id is 16 random bytes minted the first time a root is loaded. It scopes every keychain
//! account to its root (`<install-id>/instance-<n>`), so two roots sharing one OS keychain (another
//! `XDG_DATA_HOME`, Flatpak beside a native install, a roaming profile, a test harness) never read or
//! overwrite each other's keys.

use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::seams::{InstanceConfig, InstanceId, Keychain};
use crate::store::{StoreError, create_private_dir_all, lock_instance, sync_dir};

/// Instance ids are `0..MAX_INSTANCES`.
pub const MAX_INSTANCES: usize = 32;
/// The preview client's directory name under the OS data dir (never the Electron app's).
pub const PRODUCT_DIR: &str = "Dilla Preview";
const LIST_FILE: &str = "instances.json";
/// The cross-process lock around every read-modify-write of the list (never deleted, like the store's lock).
const LIST_LOCK_FILE: &str = "instances.json.lock";
const INSTANCES_DIR: &str = "instances";
const LIST_VERSION: u32 = 1;
const INSTALL_ID_BYTES: usize = 16;

/// `<OS data dir>/Dilla Preview`, the root [`Instances::load`] takes in the shipping app.
pub fn default_root() -> Result<PathBuf, StoreError> {
    dirs::data_dir()
        .map(|d| d.join(PRODUCT_DIR))
        .ok_or(StoreError::NoDataDir)
}

#[derive(Serialize, Deserialize)]
struct ListFile {
    version: u32,
    /// Absent only in a list written before install ids existed; [`Instances::load`] adds one.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    install_id: Option<String>,
    instances: Vec<ListEntry>,
}

#[derive(Serialize, Deserialize)]
struct ListEntry {
    id: u8,
    server: String,
}

/// A parsed, validated list: its install id (if it has one) and its instances.
struct Listing {
    install_id: Option<String>,
    entries: Vec<(InstanceId, url::Url)>,
}

#[derive(Debug)]
pub struct Instances {
    root: PathBuf,
    install_id: String,
    entries: Vec<InstanceConfig>,
}

impl Instances {
    /// Reads the instance list under `root`, creating `root` if needed. A missing list is empty. A
    /// root without an install id gets one, saved before this returns. A list that does not parse,
    /// or whose install id is malformed, is [`StoreError::CorruptInstanceList`]: never replaced.
    pub fn load(root: &Path) -> Result<Self, StoreError> {
        create_private_dir_all(root)?;
        // Under the list lock, so two first launches never mint two install ids.
        let _lock = lock_list(root)?;
        let listing = read_listing(root)?;
        let minted = listing.install_id.is_none();
        let install_id = match listing.install_id {
            Some(id) => id,
            None => new_install_id()?,
        };
        let mut this = Self {
            root: root.to_path_buf(),
            install_id,
            entries: Vec::new(),
        };
        for (id, server) in listing.entries {
            this.entries.push(this.config(id, server));
        }
        if minted {
            this.save()?;
        }
        Ok(this)
    }

    /// Re-reads the list from disk, under the caller's list lock: another process (or another manager window) may have
    /// added or removed instances since this value was loaded. A list that now names another install id is refused.
    fn reload(&mut self) -> Result<(), StoreError> {
        let listing = read_listing(&self.root)?;
        if listing
            .install_id
            .as_deref()
            .is_some_and(|id| id != self.install_id)
        {
            return Err(StoreError::CorruptInstanceList);
        }
        self.entries = listing
            .entries
            .into_iter()
            .map(|(id, server)| self.config(id, server))
            .collect();
        Ok(())
    }

    /// Re-reads the list, so a view drawn from this value shows what other processes changed.
    pub fn refresh(&mut self) -> Result<(), StoreError> {
        let _lock = lock_list(&self.root)?;
        self.reload()
    }

    /// The instance listed for exactly this server URL (two complete URLs on one host are two instances).
    pub fn find(&self, server: &url::Url) -> Option<&InstanceConfig> {
        self.entries.iter().find(|c| &c.server == server)
    }

    /// This root's install id (32 hex digits), the prefix of its keychain accounts.
    pub fn install_id(&self) -> &str {
        &self.install_id
    }

    /// Adds an instance for `server` on the lowest id that is neither listed nor has a data dir on
    /// disk. A data dir no list mentions (the list was lost or reset) is left alone, never wiped: its
    /// key may still be in the keychain under the old install id. Refused once no id is free.
    pub fn add(&mut self, server: url::Url) -> Result<InstanceConfig, StoreError> {
        if !is_http(&server) {
            return Err(StoreError::InvalidServer);
        }
        let _lock = lock_list(&self.root)?;
        // The list on disk, not this value's copy: a stale list never decides which id is free.
        self.reload()?;
        let mut free = None;
        for id in (0..MAX_INSTANCES as u8).map(InstanceId) {
            if self.get(id).is_some() {
                continue;
            }
            if !exists(&self.data_dir(id))? {
                free = Some(id);
                break;
            }
        }
        let id = free.ok_or(StoreError::TooManyInstances)?;
        let cfg = self.config(id, server);
        self.entries.push(cfg.clone());
        self.entries.sort_by_key(|c| c.id.0);
        if let Err(e) = self.save() {
            self.entries.retain(|c| c.id != id);
            return Err(e);
        }
        Ok(cfg)
    }

    /// Removes an instance and deletes its data dir, holding the instance's open lock throughout so
    /// no open of it runs meanwhile. The data dir goes first, so a crash between the two leaves a
    /// listed instance with an empty store, never stale data under a reused id. The lock file is never
    /// deleted (an unlinked lock file would let two later opens lock different files). The keychain
    /// entry may be deleted by the caller (`Keychain::delete(&keychain_account(..))`); a key left
    /// behind protects nothing, and the id's next first open replaces it with a fresh one.
    pub fn remove(&mut self, id: InstanceId) -> Result<Option<InstanceConfig>, StoreError> {
        let _list_lock = lock_list(&self.root)?;
        // A removal elsewhere is final: this (possibly stale) value never lists the instance again.
        self.reload()?;
        self.remove_listed(id)
    }

    /// [`Instances::remove`] for the manager, in one call: [`Instances::erase_key`] then [`Instances::remove_verified`],
    /// the instance held throughout. `expected` is the server the person saw in the list: an id that was removed and
    /// reused for another server meanwhile is [`InstanceError::Changed`], never erased.
    pub fn remove_erasing(
        &mut self,
        id: InstanceId,
        expected: &url::Url,
        kc: &dyn Keychain,
    ) -> Result<Option<InstanceConfig>, InstanceError> {
        let _open = self.hold(id)?;
        if self.erase_key(id, expected, kc, false)?.is_none() {
            return Ok(None);
        }
        self.remove_verified(id, expected, false)
    }

    /// The first half of a removal: checks that `id` is still listed for `expected`, then deletes its database key from
    /// the keychain (a crypto-erase, [`delete_instance_key`]). Nothing else is touched, so a keychain that refuses
    /// leaves the instance whole. The list lock is not held while the keychain answers. With `hold` the instance is
    /// held meanwhile (refused with [`InstanceError::InUse`] when a window has it); pass `false` when the caller
    /// already holds it. `Ok(None)`: it is no longer listed.
    pub fn erase_key(
        &mut self,
        id: InstanceId,
        expected: &url::Url,
        kc: &dyn Keychain,
        hold: bool,
    ) -> Result<Option<InstanceConfig>, InstanceError> {
        let _open = if hold { Some(self.hold(id)?) } else { None };
        let cfg = {
            let _list_lock = lock_list(&self.root)?;
            self.reload()?;
            match self.get(id) {
                None => return Ok(None),
                Some(c) if &c.server != expected => return Err(InstanceError::Changed),
                Some(c) => c.clone(),
            }
        };
        delete_instance_key(kc, &cfg)?;
        Ok(Some(cfg))
    }

    /// The second half of a removal: deletes the data dir and the list entry of `id` if it is still listed for
    /// `expected` ([`InstanceError::Changed`] otherwise). With `hold`, the instance is held meanwhile.
    pub fn remove_verified(
        &mut self,
        id: InstanceId,
        expected: &url::Url,
        hold: bool,
    ) -> Result<Option<InstanceConfig>, InstanceError> {
        let _open = if hold { Some(self.hold(id)?) } else { None };
        let _list_lock = lock_list(&self.root)?;
        self.reload()?;
        match self.get(id) {
            None => return Ok(None),
            Some(c) if &c.server != expected => return Err(InstanceError::Changed),
            Some(_) => {}
        }
        Ok(self.remove_listed(id)?)
    }

    /// The removal itself, under the caller's list lock and on a freshly reloaded list.
    fn remove_listed(&mut self, id: InstanceId) -> Result<Option<InstanceConfig>, StoreError> {
        let Some(pos) = self.entries.iter().position(|c| c.id == id) else {
            return Ok(None);
        };
        let data_dir = self.entries[pos].data_dir.clone();
        let _lock = lock_instance(&data_dir)?;
        remove_dir_if_present(&data_dir)?;
        let removed = self.entries.remove(pos);
        if let Err(e) = self.save() {
            self.entries.insert(pos, removed);
            return Err(e);
        }
        Ok(Some(removed))
    }

    /// Holds instance `id` for a window: an exclusive lock on `instances/<n>.window`, kept until the guard drops. A second
    /// window of the same instance, in this process or another, gets [`InstanceError::InUse`]; so does a removal of it.
    /// It is a lock of its own, not the store's open lock (`instances/<n>.lock`): the store takes that one for the
    /// moment of an open, so a window holding it for its whole life would block its own client's start.
    pub fn hold(&self, id: InstanceId) -> Result<InstanceGuard, InstanceError> {
        let dir = self.data_dir(id);
        let mut name = dir.file_name().unwrap_or_default().to_os_string();
        name.push(".window");
        let path = dir.with_file_name(name);
        create_private_dir_all(&self.root.join(INSTANCES_DIR))?;
        let file = lock_file(&path)?;
        match file.try_lock() {
            Ok(()) => Ok(InstanceGuard { _file: file, id }),
            Err(fs::TryLockError::WouldBlock) => Err(InstanceError::InUse),
            Err(fs::TryLockError::Error(e)) => Err(StoreError::Io(e).into()),
        }
    }

    /// Every instance, in id order.
    pub fn list(&self) -> &[InstanceConfig] {
        &self.entries
    }

    pub fn get(&self, id: InstanceId) -> Option<&InstanceConfig> {
        self.entries.iter().find(|c| c.id == id)
    }

    fn data_dir(&self, id: InstanceId) -> PathBuf {
        self.root.join(INSTANCES_DIR).join(id.0.to_string())
    }

    fn config(&self, id: InstanceId, server: url::Url) -> InstanceConfig {
        InstanceConfig {
            id,
            server,
            data_dir: self.data_dir(id),
        }
    }

    /// Writes the list to a temporary file and renames it over the old one.
    fn save(&self) -> Result<(), StoreError> {
        let file = ListFile {
            version: LIST_VERSION,
            install_id: Some(self.install_id.clone()),
            instances: self
                .entries
                .iter()
                .map(|c| ListEntry {
                    id: c.id.0,
                    server: c.server.to_string(),
                })
                .collect(),
        };
        let json = serde_json::to_vec_pretty(&file).map_err(|_| StoreError::CorruptInstanceList)?;
        let tmp = self.root.join(format!("{LIST_FILE}.tmp"));
        let mut out = fs::File::create(&tmp)?;
        out.write_all(&json)?;
        out.sync_all()?;
        drop(out);
        fs::rename(&tmp, self.root.join(LIST_FILE))?;
        sync_dir(&self.root)
    }
}

/// For the store: the install id of the root that `data_dir` (`<root>/instances/<n>`) belongs to, if
/// that root's list names instance `id` at exactly this data dir. Read-only: it never mints an install
/// id, so a missing or damaged list can never lead to a new key for an existing database.
pub(crate) fn install_id_for(data_dir: &Path, id: InstanceId) -> Result<String, StoreError> {
    let instances = data_dir.parent().ok_or(StoreError::InvalidInstance)?;
    if instances.file_name().is_none_or(|n| n != INSTANCES_DIR)
        || data_dir.file_name().is_none_or(|n| *n != *id.0.to_string())
    {
        return Err(StoreError::InvalidInstance);
    }
    let root = instances.parent().ok_or(StoreError::InvalidInstance)?;
    let raw = match fs::read(root.join(LIST_FILE)) {
        Ok(raw) => raw,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(StoreError::CorruptInstanceList);
        }
        Err(e) => return Err(e.into()),
    };
    let listing = parse(&raw)?;
    if !listing.entries.iter().any(|(listed, _)| *listed == id) {
        return Err(StoreError::InvalidInstance);
    }
    listing.install_id.ok_or(StoreError::CorruptInstanceList)
}

/// Why the manager's operations on an instance fail.
#[derive(Debug, thiserror::Error)]
pub enum InstanceError {
    /// The instance has a window open (in this process or another).
    #[error("this instance is open in a window")]
    InUse,
    /// The OS keychain could not delete the instance's database key.
    #[error("the platform keychain could not delete the instance's key")]
    Keychain,
    /// The id is listed for another server than the one the caller meant (it was removed and reused).
    #[error("the instance list changed")]
    Changed,
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// A window's hold on its instance ([`Instances::hold`]); the lock is released when it drops.
#[derive(Debug)]
pub struct InstanceGuard {
    _file: File,
    id: InstanceId,
}

impl InstanceGuard {
    pub fn id(&self) -> InstanceId {
        self.id
    }
}

/// Deletes instance `cfg`'s database key from the OS keychain: with the key gone, the instance's data dir is
/// unreadable, so removing an instance is a crypto-erase. Deleting a key that is not there succeeds.
///
/// The one place the manager reaches the keychain account of a key it removes.
pub fn delete_instance_key(kc: &dyn Keychain, cfg: &InstanceConfig) -> Result<(), InstanceError> {
    // The account that holds the key now: its generation suffix read as an open reads it (Task 2).
    let account = crate::store::store_keychain_account(cfg)?;
    kc.delete(&account).map_err(|_| InstanceError::Keychain)
}

/// Reads and parses the list under `root`; a missing list is empty and has no install id yet.
fn read_listing(root: &Path) -> Result<Listing, StoreError> {
    match fs::read(root.join(LIST_FILE)) {
        Ok(raw) => parse(&raw),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Listing {
            install_id: None,
            entries: Vec::new(),
        }),
        Err(e) => Err(e.into()),
    }
}

/// Takes the exclusive lock around the list's read-modify-write, blocking until it is free. Held by the returned file.
fn lock_list(root: &Path) -> Result<File, StoreError> {
    create_private_dir_all(root)?;
    let file = lock_file(&root.join(LIST_LOCK_FILE))?;
    file.lock()?;
    Ok(file)
}

/// Opens (creating, owner-only) a lock file. Lock files are never deleted.
fn lock_file(path: &Path) -> Result<File, StoreError> {
    let mut options = OpenOptions::new();
    options.read(true).write(true).create(true).truncate(false);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    Ok(options.open(path)?)
}

/// What a person may type or paste into the manager, and the rules for it: the same ones as the desktop shell's
/// `packages/desktop/src/policy.ts` in the former Electron shell (tag desktop-v0.1.0-beta.5) (`canonicalInstance`, `instanceInput`, `parseInvite`).
pub mod address {
    use url::Url;

    /// The input is not an address the client connects to; the text says why, never echoing the input.
    #[derive(Debug, PartialEq, Eq, thiserror::Error)]
    #[error("{0}")]
    pub struct InvalidAddress(pub &'static str);

    /// An instance and the invite code a pasted link carried.
    #[derive(Debug, PartialEq, Eq)]
    pub struct Input {
        pub instance: Url,
        pub invite: Option<String>,
    }

    /// The refusals' texts (the sign-up maps them to its own errors).
    pub const INVALID: &str = "Invalid instance URL";
    pub const CREDENTIALS: &str = "Instance URLs cannot contain credentials, queries or fragments";
    pub const INSECURE: &str = "An HTTPS instance is required";

    /// `unsafeUrl`: whitespace, a backslash or a control character anywhere.
    fn unsafe_url(input: &str) -> bool {
        input
            .chars()
            .any(|c| c.is_whitespace() || c == '\\' || c == '\u{feff}' || c.is_control())
    }

    fn encoded_separator(input: &str) -> bool {
        let lower = input.to_ascii_lowercase();
        ["%2f", "%5c", "%00", "%2e"]
            .iter()
            .any(|needle| lower.contains(needle))
    }

    fn is_hex32(s: &str) -> bool {
        s.len() == 32 && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
    }

    fn is_code(s: &str) -> bool {
        (1..=128).contains(&s.len())
            && s.bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
    }

    const USER_SECTIONS: [&str; 9] = [
        "profile",
        "devices",
        "signin",
        "notifications",
        "appearance",
        "voice",
        "privacy",
        "keyboard",
        "diagnostics",
    ];
    const SERVER_SECTIONS: [&str; 14] = [
        "overview",
        "channels",
        "gifs",
        "members",
        "roles",
        "invites",
        "selfRoles",
        "bot",
        "welcome",
        "joinRoles",
        "music",
        "moderationLog",
        "bans",
        "danger",
    ];

    /// `isAppNavigation` for a path without a query: the pages of the web app a pasted link may point at.
    fn is_app_path(path: &str) -> bool {
        if matches!(
            path,
            "/" | "/welcome" | "/dm" | "/activity" | "/you" | "/settings"
        ) {
            return true;
        }
        let parts: Vec<&str> = path.trim_start_matches('/').split('/').collect();
        match parts.as_slice() {
            ["dm", id] => is_hex32(id),
            ["settings", section] => USER_SECTIONS.contains(section),
            ["c", server] => is_hex32(server),
            ["c", server, channel] if is_hex32(server) => {
                is_hex32(channel) || *channel == "settings"
            }
            ["c", server, "settings", section] => {
                is_hex32(server) && SERVER_SECTIONS.contains(section)
            }
            _ => false,
        }
    }

    /// `/i/<code>`: the code of an invite page link.
    fn invite_code(path: &str) -> Option<&str> {
        let code = path.strip_prefix("/i/")?;
        let code = code.strip_suffix('/').unwrap_or(code);
        is_code(code).then_some(code)
    }

    /// `canonicalInstance`: an `https` (or, with `allow_loopback`, a loopback `http`) address, without credentials, query
    /// or fragment, ending in `/`. A pasted page of the web app (`/welcome`, `/c/<id>/<id>`, `/i/<code>`) names its
    /// server, so it becomes the server's root.
    pub fn canonical_instance(input: &str, allow_loopback: bool) -> Result<Url, InvalidAddress> {
        if input.is_empty() || input.len() > 2048 || unsafe_url(input) || encoded_separator(input) {
            return Err(InvalidAddress(INVALID));
        }
        let mut url = Url::parse(input).map_err(|_| InvalidAddress(INVALID))?;
        // The strictest of the desktop's two ports (Task 25's invite links, Task 27's addresses): a `?` or `#` refuses
        // the address even when empty.
        if !url.username().is_empty()
            || url.password().is_some()
            || url.query().is_some()
            || url.fragment().is_some()
            || input.contains(['?', '#'])
        {
            return Err(InvalidAddress(CREDENTIALS));
        }
        // Refused above whenever present (even empty).
        url.set_query(None);
        url.set_fragment(None);
        let host = url.host_str().unwrap_or_default();
        let local = allow_loopback && matches!(host, "127.0.0.1" | "[::1]" | "localhost");
        if url.scheme() != "https" && !(local && url.scheme() == "http") {
            return Err(InvalidAddress(INSECURE));
        }
        if host.is_empty() || host.ends_with('.') {
            return Err(InvalidAddress("Invalid instance host"));
        }
        let path = if url.path().len() > 1 {
            url.path().trim_end_matches('/').to_owned()
        } else {
            "/".to_owned()
        };
        if path != "/" && (invite_code(&path).is_some() || is_app_path(&path)) {
            url.set_path("/");
        } else if !url.path().ends_with('/') {
            let with_slash = format!("{}/", url.path());
            url.set_path(&with_slash);
        }
        if url.path().starts_with("/assets/") {
            return Err(InvalidAddress(
                "The installed client reserves /assets/; choose another instance base path",
            ));
        }
        Ok(url)
    }

    /// A pasted invite link (the sign-up's step 1, Task 25): exactly `<instance>/i/<code>` on an address
    /// [`canonical_instance`] takes, answered as the instance (the link's origin, `scheme://host[:port]/`) and its code.
    /// Anything else (a bare code, another page) is [`INVALID`].
    pub fn invite_link(input: &str, allow_loopback: bool) -> Result<Input, InvalidAddress> {
        let input = input.trim();
        // Not a web address at all (`javascript:`, a bare code) is no link, before any https rule.
        let url = Url::parse(input).map_err(|_| InvalidAddress(INVALID))?;
        if !matches!(url.scheme(), "https" | "http") {
            return Err(InvalidAddress(INVALID));
        }
        canonical_instance(input, allow_loopback)?;
        // An instance served under a base path mints `<base URL>/i/<code>` (dillad invites.go `NewInvites`): the link
        // names `<origin><base>/` when that is the instance's own canonical address (integration 3, step 0 item 4).
        // A base `canonical_instance` refuses or rewrites (`/assets/`, an app page, another invite) is no instance's.
        let path = url.path();
        let at = path.rfind("/i/").ok_or(InvalidAddress(INVALID))?;
        let (base, rest) = path.split_at(at);
        let code = invite_code(rest).ok_or(InvalidAddress(INVALID))?;
        let instance = canonical_instance(
            &format!("{}{base}/", url.origin().ascii_serialization()),
            allow_loopback,
        )?;
        if instance.path() != format!("{base}/") {
            return Err(InvalidAddress(INVALID));
        }
        Ok(Input {
            instance,
            invite: Some(code.to_owned()),
        })
    }

    /// `instanceInput`: the instance, and the invite code when the input was an `/i/<code>` page link.
    pub fn instance_input(input: &str, allow_loopback: bool) -> Result<Input, InvalidAddress> {
        let instance = canonical_instance(input, allow_loopback)?;
        let invite = Url::parse(input)
            .ok()
            .and_then(|u| invite_code(u.path()).map(str::to_owned));
        Ok(Input { instance, invite })
    }

    /// `parseInvite`: `dilla://invite/<host>/<code>` only, with the host canonicalised as an `https` instance.
    pub fn parse_invite(input: &str) -> Result<Input, InvalidAddress> {
        const BAD: InvalidAddress = InvalidAddress("Invalid invite");
        if input.len() > 4096 || unsafe_url(input) {
            return Err(BAD);
        }
        // Matched on the original string, before URL parsing could normalise dots, slashes or encodings.
        let rest = input.strip_prefix("dilla://invite/").ok_or(BAD)?;
        let (host, code) = rest.split_once('/').ok_or(BAD)?;
        let host_ok = !host.is_empty()
            && host.bytes().all(|b| {
                b.is_ascii_alphanumeric() || matches!(b, b'.' | b'[' | b']' | b':' | b'-')
            });
        if !host_ok || !is_code(code) {
            return Err(BAD);
        }
        let instance = canonical_instance(&format!("https://{host}/"), false)?;
        Ok(Input {
            instance,
            invite: Some(code.to_owned()),
        })
    }
}

fn parse(raw: &[u8]) -> Result<Listing, StoreError> {
    let file: ListFile =
        serde_json::from_slice(raw).map_err(|_| StoreError::CorruptInstanceList)?;
    if file.version != LIST_VERSION
        || file
            .install_id
            .as_deref()
            .is_some_and(|id| !valid_install_id(id))
    {
        return Err(StoreError::CorruptInstanceList);
    }
    let mut entries: Vec<(InstanceId, url::Url)> = Vec::new();
    for entry in file.instances {
        let id = InstanceId(entry.id);
        let server = url::Url::parse(&entry.server).map_err(|_| StoreError::CorruptInstanceList)?;
        if usize::from(entry.id) >= MAX_INSTANCES
            || entries.iter().any(|(listed, _)| *listed == id)
            || !is_http(&server)
        {
            return Err(StoreError::CorruptInstanceList);
        }
        entries.push((id, server));
    }
    entries.sort_by_key(|(id, _)| id.0);
    Ok(Listing {
        install_id: file.install_id,
        entries,
    })
}

fn valid_install_id(id: &str) -> bool {
    id.len() == INSTALL_ID_BYTES * 2
        && id.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
        && id.bytes().any(|b| b != b'0')
}

fn new_install_id() -> Result<String, StoreError> {
    let mut bytes = [0u8; INSTALL_ID_BYTES];
    while bytes.iter().all(|b| *b == 0) {
        getrandom::fill(&mut bytes)
            .map_err(|_| StoreError::Io(std::io::Error::other("the OS random source failed")))?;
    }
    Ok(bytes.iter().map(|b| format!("{b:02x}")).collect())
}

fn is_http(server: &url::Url) -> bool {
    matches!(server.scheme(), "http" | "https") && server.has_host()
}

fn exists(path: &Path) -> Result<bool, StoreError> {
    match fs::symlink_metadata(path) {
        Ok(_) => Ok(true),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e.into()),
    }
}

/// Deletes `dir`. A store whose last user (a runtime task of a client that just stopped) has not closed it yet makes
/// Windows refuse with a sharing violation, so a failure is tried again for about two seconds before it is reported.
fn remove_dir_if_present(dir: &Path) -> Result<(), StoreError> {
    let mut last = None;
    for attempt in 0..20 {
        match fs::remove_dir_all(dir) {
            Ok(()) => return Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(e) => last = Some(e),
        }
        if attempt < 19 {
            std::thread::sleep(std::time::Duration::from_millis(100));
        }
    }
    Err(last.map_or(StoreError::Database, StoreError::from))
}
