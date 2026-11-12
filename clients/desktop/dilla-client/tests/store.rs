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

/// The keychain account of instance `n` in `instances`' root.
fn acct(instances: &Instances, n: u8) -> String {
    keychain_account(instances.install_id(), InstanceId(n))
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

    assert_eq!(kc.accounts(), vec![acct(&instances, 0)]);
    assert_eq!(kc.get(&acct(&instances, 0)).unwrap().unwrap().len(), 32);

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
    let key = kc.get(&acct(&instances, 0)).unwrap().unwrap();
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
    assert_ne!(
        keychain_account(instances.install_id(), a.id),
        keychain_account(instances.install_id(), b.id)
    );

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
        vec![acct(&instances, 0), acct(&instances, 1)]
    );
    assert_ne!(
        *kc.get(&acct(&instances, 0)).unwrap().unwrap(),
        *kc.get(&acct(&instances, 1)).unwrap().unwrap()
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
    let old_key = kc.get(&acct(&instances, 7)).unwrap().unwrap();
    let again = instances.add(server()).unwrap();
    assert_eq!(again.id, InstanceId(7));
    let core = open_store(&again, &kc).unwrap();
    assert!(!settings_contain(&core, "from the removed instance"));
    // The removed instance's key was replaced: removal is a crypto-erase.
    let new_key = kc.get(&acct(&instances, 7)).unwrap().unwrap();
    assert_eq!(new_key.len(), 32);
    assert_ne!(*new_key, *old_key);
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
    let right = kc.get(&acct(&instances, 0)).unwrap().unwrap();
    let db = std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap();

    let mut wrong = right.to_vec();
    wrong[0] ^= 0x01;
    kc.set(&acct(&instances, 0), &wrong).unwrap();
    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Locked)));

    // A value of the wrong length is refused the same way.
    kc.set(&acct(&instances, 0), &right[..16]).unwrap();
    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Locked)));

    // The failed opens changed nothing: the database is byte-identical and opens with the right key.
    assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), db);
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);
    kc.set(&acct(&instances, 0), &right).unwrap();
    let core = open_store(&cfg, &kc).unwrap();
    assert!(settings_contain(&core, "kept through a failed open"));
}

#[test]
fn key_stored_but_database_never_created_gets_a_fresh_key() {
    // A crash after the key reached the keychain and before the database was published: the old key
    // protects nothing, so a fresh one replaces it and the store opens.
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    let key = [0x5au8; 32];
    kc.set(&acct(&instances, 0), &key).unwrap();
    // An abandoned half-built database from the same crash.
    std::fs::create_dir_all(&cfg.data_dir).unwrap();
    std::fs::write(cfg.data_dir.join("store.db.init"), b"half written").unwrap();
    std::fs::write(cfg.data_dir.join("store.db.init-journal"), b"half written").unwrap();

    let mut core = open_store(&cfg, &kc).unwrap();
    core.setting_put("k", "v1").unwrap();
    drop(core);
    let fresh = kc.get(&acct(&instances, 0)).unwrap().unwrap();
    assert_eq!(fresh.len(), 32);
    assert_ne!(fresh.as_slice(), &key);
    assert_eq!(kc.accounts(), vec![acct(&instances, 0)]);
    assert_eq!(files_in(&cfg.data_dir), vec![DB_FILE.to_string()]);
    // The fresh key is the one that keeps opening it.
    assert!(settings_contain(&open_store(&cfg, &kc).unwrap(), "v1"));
}

#[test]
fn busy_database_is_not_reported_as_locked() {
    // Another connection holds an exclusive lock: the key is fine, so this must not be `Locked`
    // (a UI may offer to reset a `Locked` store).
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());
    let key = kc.get(&acct(&instances, 0)).unwrap().unwrap();
    let hex: String = key.iter().map(|b| format!("{b:02x}")).collect();
    let holder = rusqlite::Connection::open(cfg.data_dir.join(DB_FILE)).unwrap();
    holder
        .execute_batch(&format!("PRAGMA key = \"x'{hex}'\"; BEGIN EXCLUSIVE;"))
        .unwrap();

    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Database)));

    holder.execute_batch("COMMIT").unwrap();
    drop(holder);
    assert!(open_store(&cfg, &kc).is_ok());
}

#[test]
fn empty_database_file_is_not_reported_as_locked() {
    // A truncated store.db says nothing about the key: `Database`, and it is not re-initialised.
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());
    std::fs::write(cfg.data_dir.join(DB_FILE), b"").unwrap();

    assert!(matches!(open_store(&cfg, &kc), Err(StoreError::Database)));
    assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), b"");
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
    kc.delete(&acct(&instances, 0)).unwrap();

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

#[test]
fn two_roots_sharing_a_keychain_never_touch_each_others_keys() {
    // Two data roots (another XDG_DATA_HOME, Flatpak beside native, a test harness) on one keychain.
    let kc = MemoryKeychain::new();
    let root_a = root();
    let root_b = root();
    let mut a = Instances::load(&root_a).unwrap();
    let mut b = Instances::load(&root_b).unwrap();
    assert_ne!(a.install_id(), b.install_id());
    let cfg_a = a.add(server()).unwrap();
    let cfg_b = b.add(server()).unwrap();
    assert_eq!(cfg_a.id, cfg_b.id);

    let mut core = open_store(&cfg_a, &kc).unwrap();
    core.setting_put("k", "root a").unwrap();
    drop(core);
    let key_a = kc.get(&acct(&a, 0)).unwrap().unwrap();
    let mut core = open_store(&cfg_b, &kc).unwrap();
    core.setting_put("k", "root b").unwrap();
    drop(core);

    // Root b's first open minted its own key under its own account; root a's is untouched.
    assert_eq!(kc.accounts().len(), 2);
    assert_eq!(*kc.get(&acct(&a, 0)).unwrap().unwrap(), *key_a);
    assert_ne!(*kc.get(&acct(&b, 0)).unwrap().unwrap(), *key_a);
    // Both keep opening, each with its own data.
    for _ in 0..2 {
        let core = open_store(&cfg_a, &kc).unwrap();
        assert!(settings_contain(&core, "root a") && !settings_contain(&core, "root b"));
        drop(core);
        let core = open_store(&cfg_b, &kc).unwrap();
        assert!(settings_contain(&core, "root b") && !settings_contain(&core, "root a"));
    }
}

#[test]
fn install_id_persists_across_loads() {
    let root_dir = root();
    let first = Instances::load(&root_dir).unwrap();
    let id = first.install_id().to_string();
    assert_eq!(id.len(), 32);
    assert!(id.bytes().all(|b| b.is_ascii_hexdigit()));
    // Saved by the load itself, before any instance exists.
    assert!(
        std::fs::read_to_string(root_dir.join("instances.json"))
            .unwrap()
            .contains(&id)
    );
    let mut again = Instances::load(&root_dir).unwrap();
    assert_eq!(again.install_id(), id);
    again.add(server()).unwrap();
    assert_eq!(Instances::load(&root_dir).unwrap().install_id(), id);

    // A list written before install ids existed gets one, persisted, and keeps its instances.
    let legacy = root();
    std::fs::create_dir_all(&legacy).unwrap();
    std::fs::write(
        legacy.join("instances.json"),
        br#"{"version":1,"instances":[{"id":3,"server":"https://chat.example.org/"}]}"#,
    )
    .unwrap();
    let loaded = Instances::load(&legacy).unwrap();
    assert_eq!(loaded.list().len(), 1);
    assert_eq!(loaded.list()[0].id, InstanceId(3));
    let minted = loaded.install_id().to_string();
    assert_eq!(Instances::load(&legacy).unwrap().install_id(), minted);
}

#[test]
fn missing_or_corrupt_install_id_never_rekeys_an_existing_store() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    let mut core = open_store(&cfg, &kc).unwrap();
    core.setting_put("k", "survives").unwrap();
    drop(core);
    let account = acct(&instances, 0);
    let key = kc.get(&account).unwrap().unwrap();
    let db = std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap();
    let list = std::fs::read(root.join("instances.json")).unwrap();

    let unchanged = |kc: &MemoryKeychain| {
        assert_eq!(kc.accounts(), vec![account.clone()]);
        assert_eq!(*kc.get(&account).unwrap().unwrap(), *key);
        assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), db);
    };

    // A malformed install id: open and load both refuse; nothing is minted or replaced.
    let text = String::from_utf8(list.clone()).unwrap();
    let corrupt = text.replace(instances.install_id(), "not-an-install-id");
    assert_ne!(corrupt, text);
    std::fs::write(root.join("instances.json"), &corrupt).unwrap();
    assert!(matches!(
        open_store(&cfg, &kc),
        Err(StoreError::CorruptInstanceList)
    ));
    assert!(matches!(
        Instances::load(&root),
        Err(StoreError::CorruptInstanceList)
    ));
    assert_eq!(
        std::fs::read_to_string(root.join("instances.json")).unwrap(),
        corrupt
    );
    unchanged(&kc);

    // A list stripped of its install id: open refuses rather than mint.
    let stripped = format!(
        r#"{{"version":1,"instances":[{{"id":0,"server":"{}"}}]}}"#,
        server()
    );
    std::fs::write(root.join("instances.json"), &stripped).unwrap();
    assert!(matches!(
        open_store(&cfg, &kc),
        Err(StoreError::CorruptInstanceList)
    ));
    unchanged(&kc);

    // The list is gone: open refuses. A new list gets a new install id; its first instance skips the
    // orphaned data dir, and the orphan's database and key are left alone.
    std::fs::remove_file(root.join("instances.json")).unwrap();
    assert!(matches!(
        open_store(&cfg, &kc),
        Err(StoreError::CorruptInstanceList)
    ));
    unchanged(&kc);
    let mut fresh = Instances::load(&root).unwrap();
    assert_ne!(fresh.install_id(), instances.install_id());
    let other = fresh.add(server()).unwrap();
    assert_eq!(other.id, InstanceId(1));
    assert!(matches!(
        open_store(&cfg, &kc),
        Err(StoreError::InvalidInstance)
    ));
    drop(open_store(&other, &kc).unwrap());
    assert_eq!(*kc.get(&account).unwrap().unwrap(), *key);
    assert_eq!(std::fs::read(cfg.data_dir.join(DB_FILE)).unwrap(), db);

    // Restoring the original list restores access.
    std::fs::write(root.join("instances.json"), &list).unwrap();
    assert!(settings_contain(
        &open_store(&cfg, &kc).unwrap(),
        "survives"
    ));
}

#[test]
fn remove_waits_for_the_open_lock_and_keeps_the_lock_file() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server()).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());
    let lock_file = root.join("instances").join("0.lock");
    assert!(lock_file.exists());

    // Another process is mid-open: it holds the instance lock.
    let held = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(&lock_file)
        .unwrap();
    held.lock().unwrap();
    let (tx, rx) = std::sync::mpsc::channel();
    let remover = std::thread::spawn(move || {
        let removed = instances.remove(InstanceId(0)).unwrap();
        tx.send(()).unwrap();
        (instances, removed)
    });
    assert!(
        rx.recv_timeout(std::time::Duration::from_millis(300))
            .is_err()
    );
    assert!(cfg.data_dir.join(DB_FILE).exists());
    held.unlock().unwrap();
    let (instances, removed) = remover.join().unwrap();
    assert!(removed.is_some());
    assert!(instances.list().is_empty());

    // The lock file is still there (the same file every opener locks), and the data dir is gone.
    assert!(lock_file.exists());
    assert!(!cfg.data_dir.exists());
    // An open of the removed instance is refused and does not recreate its data dir.
    assert!(matches!(
        open_store(&cfg, &kc),
        Err(StoreError::InvalidInstance)
    ));
    assert!(!cfg.data_dir.exists());
}
