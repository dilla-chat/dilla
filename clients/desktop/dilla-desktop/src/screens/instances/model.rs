//! What the instance manager does, apart from how it looks: the list, connecting by address or by pasted invite, and
//! removing. The manager runs before any `Client` exists (the area has no state slice), so this works on the instance
//! list and the keychain directly.

use std::path::{Path, PathBuf};
use std::sync::Arc;

use dilla_client::instances::address::{self, InvalidAddress};
use dilla_client::instances::{InstanceError, Instances, MAX_INSTANCES};
use dilla_client::seams::{InstanceConfig, InstanceId, Keychain};
use dilla_client::store::StoreError;

use super::copy;

/// One line of the list: an instance by its address, shown as the host and a base path.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Entry {
    pub id: InstanceId,
    /// `host` or `host:port/base`, as the shell's menu names an instance.
    pub label: String,
    pub server: url::Url,
}

/// What a connect or an invite leads to: open this instance, with the invite code a pasted link carried.
#[derive(Clone, Debug)]
pub struct Connect {
    pub instance: InstanceConfig,
    pub invite: Option<String>,
}

/// Why a connect, an invite or a removal did not happen. [`Refusal::message`] is what the window shows.
#[derive(Debug)]
pub enum Refusal {
    Address(InvalidAddress),
    TooMany,
    /// The instance is open in a window (here or in another process).
    InUse,
    /// The keychain would not delete the instance's key, so nothing was erased.
    Keychain,
    Store(StoreError),
    /// The list changed under the window (an instance removed and its id reused): nothing was done.
    Changed,
    /// A removal gave up waiting for the instance's client to stop; its key is already erased.
    Removing,
}

impl Refusal {
    pub fn message(&self) -> String {
        match self {
            Self::Address(_) => copy::ERROR_CONNECT.into(),
            Self::TooMany => copy::ERROR_TOO_MANY.replace("{max}", &MAX_INSTANCES.to_string()),
            Self::InUse => copy::ERROR_IN_USE.into(),
            Self::Keychain => copy::ERROR_KEYCHAIN.into(),
            Self::Store(_) => copy::ERROR_STORE.into(),
            Self::Changed => copy::ERROR_CHANGED.into(),
            Self::Removing => copy::ERROR_REMOVING.into(),
        }
    }
}

impl From<InstanceError> for Refusal {
    fn from(e: InstanceError) -> Self {
        match e {
            InstanceError::InUse => Self::InUse,
            InstanceError::Keychain => Self::Keychain,
            InstanceError::Changed => Self::Changed,
            InstanceError::Store(e) => e.into(),
        }
    }
}

impl From<StoreError> for Refusal {
    fn from(e: StoreError) -> Self {
        match e {
            StoreError::TooManyInstances => Self::TooMany,
            StoreError::InvalidServer => Self::Address(InvalidAddress("Invalid instance URL")),
            e => Self::Store(e),
        }
    }
}

/// The manager's model.
pub struct Manager {
    root: PathBuf,
    keychain: Arc<dyn Keychain>,
    instances: Instances,
    allow_loopback: bool,
}

impl Manager {
    /// Loads the list under `root`. `allow_loopback` admits `http://localhost` (a dev build only).
    pub fn load(
        root: &Path,
        keychain: Arc<dyn Keychain>,
        allow_loopback: bool,
    ) -> Result<Self, StoreError> {
        Ok(Self {
            root: root.to_path_buf(),
            keychain,
            instances: Instances::load(root)?,
            allow_loopback,
        })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn keychain(&self) -> &Arc<dyn Keychain> {
        &self.keychain
    }

    /// The list again from disk: another window or process may have changed it.
    pub fn refresh(&mut self) -> Result<(), StoreError> {
        self.instances.refresh()
    }

    pub fn instances(&self) -> &Instances {
        &self.instances
    }

    /// Every instance in id order.
    pub fn entries(&self) -> Vec<Entry> {
        self.instances
            .list()
            .iter()
            .map(|cfg| Entry {
                id: cfg.id,
                label: label_of(&cfg.server),
                server: cfg.server.clone(),
            })
            .collect()
    }

    /// Connects to what was typed: an instance address, or a page link of one (an `/i/<code>` link carries its invite).
    pub fn connect_url(&mut self, input: &str) -> Result<Connect, Refusal> {
        let parsed =
            address::instance_input(input.trim(), self.allow_loopback).map_err(Refusal::Address)?;
        self.connect(parsed)
    }

    /// Connects to a pasted `dilla://invite/<host>/<code>` link.
    pub fn connect_invite(&mut self, input: &str) -> Result<Connect, Refusal> {
        let parsed = address::parse_invite(input.trim()).map_err(Refusal::Address)?;
        self.connect(parsed)
    }

    /// Whether `input`, a pasted invite, names an instance that is not listed yet (the window asks before it adds it,
    /// as the shell does for an invite link: only continue if you recognise the address).
    pub fn invite_names_new_instance(&mut self, input: &str) -> Result<bool, Refusal> {
        let parsed = address::parse_invite(input.trim()).map_err(Refusal::Address)?;
        self.instances.refresh()?;
        Ok(self.instances.find(&parsed.instance).is_none())
    }

    fn connect(&mut self, parsed: address::Input) -> Result<Connect, Refusal> {
        // The list as other windows left it: a stale list must neither hide an instance nor hold a removed one.
        self.instances.refresh()?;
        let instance = match self.instances.find(&parsed.instance) {
            Some(cfg) => cfg.clone(),
            None => self.instances.add(parsed.instance)?,
        };
        Ok(Connect {
            instance,
            invite: parsed.invite,
        })
    }
}

/// `host` (with its port) and a base path other than `/`, the shell menu's name of an instance.
pub fn label_of(server: &url::Url) -> String {
    let host = server.host_str().unwrap_or_default();
    let host = match server.port() {
        Some(port) => format!("{host}:{port}"),
        None => host.to_owned(),
    };
    match server.path() {
        "/" => host,
        path => format!("{host}{path}"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn labels_are_host_port_and_base_path() {
        let l = |s: &str| label_of(&url::Url::parse(s).unwrap());
        assert_eq!(l("https://chat.example/"), "chat.example");
        assert_eq!(
            l("https://chat.example:8443/team/"),
            "chat.example:8443/team/"
        );
    }

    #[test]
    fn refusals_never_echo_the_input() {
        let r = Refusal::Address(InvalidAddress("Invalid instance URL"));
        assert_eq!(r.message(), copy::ERROR_CONNECT);
        assert!(Refusal::TooMany.message().contains("32"));
    }
}
