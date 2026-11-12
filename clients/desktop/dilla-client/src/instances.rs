//! The instances on this machine: up to 32 copies of the client, each with its own server, data dir
//! (`<root>/instances/<n>`) and keychain account. The list is `<root>/instances.json`, which holds
//! ids and server URLs only, never a secret.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::seams::{InstanceConfig, InstanceId};
use crate::store::{StoreError, create_private_dir_all, lock_path, sync_dir};

/// Instance ids are `0..MAX_INSTANCES`.
pub const MAX_INSTANCES: usize = 32;
/// The preview client's directory name under the OS data dir (never the Electron app's).
pub const PRODUCT_DIR: &str = "Dilla Preview";
const LIST_FILE: &str = "instances.json";
const LIST_VERSION: u32 = 1;

/// `<OS data dir>/Dilla Preview`, the root [`Instances::load`] takes in the shipping app.
pub fn default_root() -> Result<PathBuf, StoreError> {
    dirs::data_dir()
        .map(|d| d.join(PRODUCT_DIR))
        .ok_or(StoreError::NoDataDir)
}

#[derive(Serialize, Deserialize)]
struct ListFile {
    version: u32,
    instances: Vec<ListEntry>,
}

#[derive(Serialize, Deserialize)]
struct ListEntry {
    id: u8,
    server: String,
}

#[derive(Debug)]
pub struct Instances {
    root: PathBuf,
    entries: Vec<InstanceConfig>,
}

impl Instances {
    /// Reads the instance list under `root`, creating `root` if needed. A missing list is empty.
    pub fn load(root: &Path) -> Result<Self, StoreError> {
        create_private_dir_all(root)?;
        let mut this = Self {
            root: root.to_path_buf(),
            entries: Vec::new(),
        };
        let raw = match fs::read(root.join(LIST_FILE)) {
            Ok(raw) => raw,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(this),
            Err(e) => return Err(e.into()),
        };
        let file: ListFile =
            serde_json::from_slice(&raw).map_err(|_| StoreError::CorruptInstanceList)?;
        if file.version != LIST_VERSION {
            return Err(StoreError::CorruptInstanceList);
        }
        for entry in file.instances {
            let id = InstanceId(entry.id);
            let server =
                url::Url::parse(&entry.server).map_err(|_| StoreError::CorruptInstanceList)?;
            if usize::from(entry.id) >= MAX_INSTANCES || this.get(id).is_some() || !is_http(&server)
            {
                return Err(StoreError::CorruptInstanceList);
            }
            this.entries.push(this.config(id, server));
        }
        this.entries.sort_by_key(|c| c.id.0);
        Ok(this)
    }

    /// Adds an instance for `server` on the lowest free id. Refused once 32 exist.
    pub fn add(&mut self, server: url::Url) -> Result<InstanceConfig, StoreError> {
        if !is_http(&server) {
            return Err(StoreError::InvalidServer);
        }
        let id = (0..MAX_INSTANCES as u8)
            .map(InstanceId)
            .find(|id| self.get(*id).is_none())
            .ok_or(StoreError::TooManyInstances)?;
        let cfg = self.config(id, server);
        // A data dir left by an earlier instance on this id (a removal cut short) is not this one's.
        remove_dir_if_present(&cfg.data_dir)?;
        self.entries.push(cfg.clone());
        self.entries.sort_by_key(|c| c.id.0);
        if let Err(e) = self.save() {
            self.entries.retain(|c| c.id != id);
            return Err(e);
        }
        Ok(cfg)
    }

    /// Removes an instance and deletes its data dir. The data dir goes first, so a crash between the
    /// two leaves a listed instance with an empty store, never stale data under a reused id. The
    /// keychain entry is the caller's to delete (`Keychain::delete(&keychain_account(id))`); a key
    /// left behind protects nothing and is reused by the id's next first open.
    pub fn remove(&mut self, id: InstanceId) -> Result<Option<InstanceConfig>, StoreError> {
        let Some(pos) = self.entries.iter().position(|c| c.id == id) else {
            return Ok(None);
        };
        let data_dir = self.entries[pos].data_dir.clone();
        remove_dir_if_present(&data_dir)?;
        match fs::remove_file(lock_path(&data_dir)) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e.into()),
        }
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

    fn config(&self, id: InstanceId, server: url::Url) -> InstanceConfig {
        InstanceConfig {
            id,
            server,
            data_dir: self.root.join("instances").join(id.0.to_string()),
        }
    }

    /// Writes the list to a temporary file and renames it over the old one.
    fn save(&self) -> Result<(), StoreError> {
        let file = ListFile {
            version: LIST_VERSION,
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

fn is_http(server: &url::Url) -> bool {
    matches!(server.scheme(), "http" | "https") && server.has_host()
}

fn remove_dir_if_present(dir: &Path) -> Result<(), StoreError> {
    match fs::remove_dir_all(dir) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e.into()),
    }
}
