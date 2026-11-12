//! Task 6: instances, the SQLCipher store and the keychain that holds its key.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use dilla_client::instances::{Instances, MAX_INSTANCES};
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{InstanceConfig, InstanceId, Keychain, KeychainError};
use dilla_client::store::{DB_FILE, StoreError, keychain_account, open_store};
use zeroize::Zeroizing;

static NEXT: AtomicU64 = AtomicU64::new(0);

/// A fresh root under the test target dir (not /tmp, which is small on the build hosts).
fn root() -> PathBuf {
    let p = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!(
        "store-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let _ = std::fs::remove_dir_all(&p);
    p
}

fn server() -> url::Url {
    url::Url::parse("https://chat.example.org").unwrap()
}

fn files_in(dir: &Path) -> Vec<String> {
    let mut names: Vec<String> = match std::fs::read_dir(dir) {
        Ok(rd) => rd
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect(),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Vec::new(),
        Err(e) => panic!("read_dir: {e}"),
    };
    names.sort();
    names
}

fn settings_contain(core: &dilla_core::client::ClientCore, needle: &str) -> bool {
    let cbor = core.settings().unwrap();
    cbor.windows(needle.len()).any(|w| w == needle.as_bytes())
}

/// Every call fails: no platform keychain (a locked or absent Secret Service, say).
struct NoKeychain;
impl Keychain for NoKeychain {
    fn get(&self, _: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        Err(KeychainError::Unavailable)
    }
    fn set(&self, _: &str, _: &[u8]) -> Result<(), KeychainError> {
        Err(KeychainError::Unavailable)
    }
    fn delete(&self, _: &str) -> Result<(), KeychainError> {
        Err(KeychainError::Unavailable)
    }
}

/// Reads succeed (and find nothing) but nothing can be stored.
struct ReadOnlyKeychain;
impl Keychain for ReadOnlyKeychain {
    fn get(&self, _: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        Ok(None)
    }
    fn set(&self, _: &str, _: &[u8]) -> Result<(), KeychainError> {
        Err(KeychainError::Unavailable)
    }
    fn delete(&self, _: &str) -> Result<(), KeychainError> {
        Ok(())
    }
}

#[test]
fn first_open_creates_key_and_reopens() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    assert_eq!(cfg.id, InstanceId(0));
    assert_eq!(cfg.data_dir, root.join("instances").join("0"));
    let kc = MemoryKeychain::new();

    let mut core = open_store(&cfg, &kc).unwrap();
    core.setting_put("greeting", "hello from instance 0")
        .unwrap();
    drop(core);

    let core = open_store(&cfg, &kc).unwrap();
    assert!(settings_contain(&core, "hello from instance 0"));
    drop(core);

    assert_eq!(kc.accounts(), vec!["instance-0".to_string()]);
    assert_eq!(kc.get("instance-0").unwrap().unwrap().len(), 32);

    // The database on disk is SQLCipher, not a plaintext SQLite file.
    let bytes = std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap();
    assert!(!bytes.starts_with(b"SQLite format 3\0"));
    assert!(
        !bytes
            .windows(b"hello from instance 0".len())
            .any(|w| w == b"hello from instance 0")
    );
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);

    // The instance list survives a reload and holds no secret.
    let reloaded = Instances::load(&root).unwrap();
    assert_eq!(reloaded.list().len(), 1);
    assert_eq!(reloaded.list()[0].server, server());
    let listing = std::fs::read(root.join("instances.json")).unwrap();
    let key = kc.get("instance-0").unwrap().unwrap();
    assert!(!listing.windows(key.len()).any(|w| w == key.as_slice()));
}

#[test]
fn no_keychain_is_an_error_not_a_fallback() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();

    // First open without a keychain: refused, nothing written.
    assert!(matches!(
        open_store(&cfg, &NoKeychain),
        Err(StoreError::KeychainUnavailable)
    ));
    assert!(files_in(&cfg.data_dir).is_empty());

    // A keychain that cannot store the new key: refused, nothing written.
    assert!(matches!(
        open_store(&cfg, &ReadOnlyKeychain),
        Err(StoreError::KeychainUnavailable)
    ));
    assert!(files_in(&cfg.data_dir).is_empty());

    // An existing store whose keychain has gone away: refused, only the encrypted database remains.
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());
    assert!(matches!(
        open_store(&cfg, &NoKeychain),
        Err(StoreError::KeychainUnavailable)
    ));
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);
}

#[test]
fn instances_are_isolated() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let a = instances.add(server()).unwrap();
    let b = instances.add(server()).unwrap();
    assert_ne!(a.id, b.id);
    assert_ne!(a.data_dir, b.data_dir);
    assert_ne!(keychain_account(a.id), keychain_account(b.id));

    let kc = MemoryKeychain::new();
    let mut core_a = open_store(&a, &kc).unwrap();
    let core_b = open_store(&b, &kc).unwrap();
    core_a.setting_put("secret", "only in a").unwrap();
    assert!(settings_contain(&core_a, "only in a"));
    assert!(!settings_contain(&core_b, "only in a"));
    drop((core_a, core_b));

    let core_b = open_store(&b, &kc).unwrap();
    assert!(!settings_contain(&core_b, "only in a"));
    assert_eq!(
        kc.accounts(),
        vec!["instance-0".to_string(), "instance-1".to_string()]
    );
    assert_ne!(
        *kc.get("instance-0").unwrap().unwrap(),
        *kc.get("instance-1").unwrap().unwrap()
    );
}

#[test]
fn thirty_third_instance_refused() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    for n in 0..MAX_INSTANCES {
        let cfg = instances.add(server()).unwrap();
        assert_eq!(cfg.id, InstanceId(n as u8));
    }
    assert!(matches!(
        instances.add(server()),
        Err(StoreError::TooManyInstances)
    ));
    let reloaded = Instances::load(&root).unwrap();
    assert_eq!(reloaded.list().len(), MAX_INSTANCES);

    // A freed id is reused, and the reused id starts from an empty data dir.
    let kc = MemoryKeychain::new();
    let seven = instances.list()[7].clone();
    let mut core = open_store(&seven, &kc).unwrap();
    core.setting_put("old", "from the removed instance")
        .unwrap();
    drop(core);
    assert!(instances.remove(InstanceId(7)).unwrap().is_some());
    assert!(!seven.data_dir.exists());
    let again = instances.add(server()).unwrap();
    assert_eq!(again.id, InstanceId(7));
    let core = open_store(&again, &kc).unwrap();
    assert!(!settings_contain(&core, "from the removed instance"));
}

#[test]
fn wrong_key_does_not_open() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    let mut core = open_store(&cfg, &kc).unwrap();
    core.setting_put("k", "kept through a failed open").unwrap();
    drop(core);
    let right = kc.get("instance-0").unwrap().unwrap();
    let db = std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap();

    let mut wrong = right.to_vec();
    wrong[0] ^= 0x01;
    kc.set("instance-0", &wrong).unwrap();
    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Locked)));

    // A value of the wrong length is refused the same way.
    kc.set("instance-0", &right[..16]).unwrap();
    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Locked)));

    // The failed opens changed nothing: the database is byte-identical and opens with the right key.
    assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), db);
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);
    kc.set("instance-0", &right).unwrap();
    let core = open_store(&cfg, &kc).unwrap();
    assert!(settings_contain(&core, "kept through a failed open"));
}

#[test]
fn key_stored_but_database_never_created_reopens_with_that_key() {
    // A crash after the key reached the keychain and before the database was published.
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    let key = [0x5au8; 32];
    kc.set("instance-0", &key).unwrap();
    // An abandoned half-built database from the same crash.
    std::fs::create_dir_all(&cfg.data_dir).unwrap();
    std::fs::write(cfg.data_dir.join("store.db.init"), b"half written").unwrap();
    std::fs::write(cfg.data_dir.join("store.db.init-journal"), b"half written").unwrap();

    let mut core = open_store(&cfg, &kc).unwrap();
    core.setting_put("k", "v1").unwrap();
    drop(core);
    assert_eq!(kc.get("instance-0").unwrap().unwrap().as_slice(), &key);
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);
    assert!(settings_contain(&open_store(&cfg, &kc).unwrap(), "v1"));
}

#[test]
fn database_without_key_is_refused_and_left_alone() {
    // The key is gone (keychain reset) but the database is there: never mint a new key over it.
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());
    let db = std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap();
    kc.delete("instance-0").unwrap();

    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::KeyMissing)));
    assert!(kc.accounts().is_empty());
    assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), db);
}

#[test]
fn instance_id_outside_range_is_refused() {
    let root = root();
    let cfg = InstanceConfig {
        id: InstanceId(32),
        server: server(),
        data_dir: root.join("instances").join("32"),
    };
    assert!(matches!(
        open_store(&cfg, &MemoryKeychain::new()),
        Err(StoreError::InvalidInstance)
    ));
}

#[test]
#[ignore = "touches the real OS keychain; run by hand"]
fn os_keychain_round_trip() {
    use dilla_client::keychain::OsKeychain;
    let kc = OsKeychain::new();
    let account = format!("test-{}", std::process::id());
    let secret: Vec<u8> = (0u8..32).collect();
    kc.set(&account, &secret).unwrap();
    assert_eq!(
        kc.get(&account).unwrap().unwrap().as_slice(),
        secret.as_slice()
    );
    kc.delete(&account).unwrap();
    assert!(kc.get(&account).unwrap().is_none());
    // Deleting an absent entry is not an error.
    kc.delete(&account).unwrap();
}
