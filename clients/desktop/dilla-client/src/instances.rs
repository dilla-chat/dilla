//! The instances on this machine: up to 32 copies of the client, each with its own server, data dir
//! (`<root>/instances/<n>`) and keychain account. The list is `<root>/instances.json`, which holds
//! the root's install id, instance ids and server URLs only, never a secret.
//!
//! The install id is 16 random bytes minted the first time a root is loaded. It scopes every keychain
//! account to its root (`<install-id>/instance-<n>`), so two roots sharing one OS keychain (another
//! `XDG_DATA_HOME`, Flatpak beside a native install, a roaming profile, a test harness) never read or
//! overwrite each other's keys.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::seams::{InstanceConfig, InstanceId};
use crate::store::{StoreError, create_private_dir_all, lock_instance, sync_dir};

/// Instance ids are `0..MAX_INSTANCES`.
pub const MAX_INSTANCES: usize = 32;
/// The preview client's directory name under the OS data dir (never the Electron app's).
pub const PRODUCT_DIR: &str = "Dilla Preview";
const LIST_FILE: &str = "instances.json";
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
        let listing = match fs::read(root.join(LIST_FILE)) {
            Ok(raw) => parse(&raw)?,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Listing {
                install_id: None,
                entries: Vec::new(),
            },
            Err(e) => return Err(e.into()),
        };
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

fn remove_dir_if_present(dir: &Path) -> Result<(), StoreError> {
    match fs::remove_dir_all(dir) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e.into()),
    }
}
