//! Task 27's client tests for the instances area (standard procedure S2): the address rules of the desktop shell
//! (`packages/desktop/test/policy.test.ts` of the former Electron shell, tag desktop-v0.1.0-beta.5), the list's cross-process lock, no resurrection of a removed id, the window
//! hold, and the crypto-erase on removal. They need no dillad: an instance is a directory, a list and a keychain entry.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use dilla_client::instances::address::{canonical_instance, instance_input, parse_invite};
use dilla_client::instances::{InstanceError, Instances, MAX_INSTANCES, delete_instance_key};
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{InstanceId, Keychain};
use dilla_client::store::{StoreError, open_store, store_keychain_account};

static NEXT: AtomicU64 = AtomicU64::new(0);

fn root() -> PathBuf {
    let p = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!(
        "instances-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let _ = std::fs::remove_dir_all(&p);
    p
}

fn url(s: &str) -> url::Url {
    url::Url::parse(s).unwrap()
}

fn canon(s: &str) -> String {
    canonical_instance(s, false).unwrap().to_string()
}

fn settings_contain(core: &dilla_core::client::ClientCore, needle: &str) -> bool {
    let cbor = core.settings().unwrap();
    cbor.windows(needle.len()).any(|w| w == needle.as_bytes())
}

/// `DILLA_DATA_DIR`: an absolute override is the root; a relative one is refused; unset or empty, the OS data dir's
/// `Dilla Preview`. Two roots are two profiles: each mints its own install id, so their keychain accounts never meet.
#[test]
fn the_data_dir_override_picks_the_root_and_profiles_do_not_share_keys() {
    use dilla_client::instances::{PRODUCT_DIR, root_from};
    use dilla_client::store::keychain_account;
    let os = Some(PathBuf::from("/home/u/.local/share"));
    let other = root();
    assert_eq!(
        root_from(Some(other.clone().into_os_string()), os.clone()).unwrap(),
        other
    );
    assert_eq!(
        root_from(None, os.clone()).unwrap(),
        Path::new("/home/u/.local/share").join(PRODUCT_DIR)
    );
    assert_eq!(
        root_from(Some("".into()), os.clone()).unwrap(),
        Path::new("/home/u/.local/share").join(PRODUCT_DIR)
    );
    assert!(matches!(
        root_from(Some("profile-b".into()), os),
        Err(StoreError::NoDataDir)
    ));
    assert!(matches!(root_from(None, None), Err(StoreError::NoDataDir)));

    let (a, b) = (root(), root());
    let a = Instances::load(&a).unwrap();
    let b = Instances::load(&b).unwrap();
    assert_ne!(a.install_id(), b.install_id());
    let id = InstanceId(0);
    assert_ne!(
        keychain_account(a.install_id(), id),
        keychain_account(b.install_id(), id)
    );
}

/// `instance identity includes scheme, port and the entire canonical URL` (policy.test.ts).
#[test]
fn instance_identity_is_the_canonical_url() {
    assert_eq!(canon("https://EXAMPLE.com:443"), "https://example.com/");
    assert_eq!(
        canon("https://example.com/team"),
        "https://example.com/team/"
    );
    let all: std::collections::HashSet<String> = [
        "https://example.com/",
        "https://example.com:8443/",
        "https://example.com/a/",
        "https://example.com/b/",
    ]
    .iter()
    .map(|u| canon(u))
    .collect();
    assert_eq!(all.len(), 4);
}

/// `instances reject credentials, URL confusion and insecure non-loopback origins` (policy.test.ts).
#[test]
fn addresses_reject_confusion() {
    for bad in [
        "http://example.com",
        "file:///etc/passwd",
        "https://a@b/",
        "https://b/?token=x",
        "https://b/#x",
        // The strictest of the two ports (Task 25's invite links): an empty `?` or `#` refuses too.
        "https://b/?",
        "https://b/#",
        " https://b/",
        "https://b/\\evil",
        "https://b/\n",
        "https://b/%2fother",
        "http://127.0.0.1:8443/",
        "https://example.com/assets/",
        "https://host./",
        "not a url",
        "",
    ] {
        assert!(canonical_instance(bad, false).is_err(), "{bad:?}");
    }
    assert_eq!(
        canonical_instance("http://127.0.0.1:8443/", true)
            .unwrap()
            .as_str(),
        "http://127.0.0.1:8443/"
    );
    assert!(canonical_instance("http://127.0.0.1.evil/", true).is_err());
    assert!(canonical_instance(&format!("https://h/{}", "a".repeat(2100)), false).is_err());
}

/// `instanceInput` (policy.test.ts): a pasted page of the web app names its server; an invite page carries its code.
#[test]
fn pasted_pages_name_their_server() {
    let i = instance_input("https://h.test/i/abc_DEF-123", false).unwrap();
    assert_eq!(i.instance.as_str(), "https://h.test/");
    assert_eq!(i.invite.as_deref(), Some("abc_DEF-123"));
    let i = instance_input("https://h.test/welcome", false).unwrap();
    assert_eq!((i.instance.as_str(), i.invite), ("https://h.test/", None));
    let i = instance_input("https://h.test/", false).unwrap();
    assert_eq!((i.instance.as_str(), i.invite), ("https://h.test/", None));
    let channel = format!("https://h.test/c/{}/{}", "a".repeat(32), "b".repeat(32));
    assert_eq!(canon(&channel), "https://h.test/");
    assert_eq!(canon("https://h.test/settings/voice"), "https://h.test/");
    // A path that is not a page of the app is a base path.
    assert_eq!(canon("https://h.test/team"), "https://h.test/team/");
}

/// `invites carry only a validated HTTPS instance and code` (policy.test.ts).
#[test]
fn invites_carry_only_an_instance_and_a_code() {
    let ok = parse_invite("dilla://invite/example.com/ABCD-EFGH").unwrap();
    assert_eq!(ok.instance.as_str(), "https://example.com/");
    assert_eq!(ok.invite.as_deref(), Some("ABCD-EFGH"));
    let ok = parse_invite("dilla://invite/chat.example:8443/abc_DEF").unwrap();
    assert_eq!(ok.instance.as_str(), "https://chat.example:8443/");
    for bad in [
        "dilla://invite/a@b/code",
        "dilla://invite/b/a/b",
        "dilla://invite/b/code?token=secret",
        "dilla://invite/b/code#x",
        "dilla://invite/b/%2e%2e",
        "dilla://invite/b/%2F",
        "dilla://auth/b/token",
        "dilla://invite/b/",
        "https://invite/b/code",
        "dilla://invite/b/code\n",
    ] {
        assert!(parse_invite(bad).is_err(), "{bad:?}");
    }
}

/// The 33rd instance is refused, and the list stays at 32.
#[test]
fn thirty_third_instance_is_refused() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    for n in 0..MAX_INSTANCES {
        instances
            .add(url(&format!("https://h{n}.example/")))
            .unwrap();
    }
    assert!(matches!(
        instances.add(url("https://one-more.example/")),
        Err(StoreError::TooManyInstances)
    ));
    assert_eq!(instances.list().len(), MAX_INSTANCES);
}

/// From `forwarding.spec.ts` "isolates storage for two complete URLs on the same host": two URLs on one host are two
/// instances with their own data dir, their own key, and nothing of one is readable through the other.
#[test]
fn two_instances_same_host_isolated() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let a = instances
        .add(canonical_instance("https://h.test/a", false).unwrap())
        .unwrap();
    let b = instances
        .add(canonical_instance("https://h.test/b", false).unwrap())
        .unwrap();
    assert_ne!(a.id, b.id);
    assert_ne!(a.data_dir, b.data_dir);
    assert_ne!(a.server, b.server);
    assert!(instances.find(&url("https://h.test/a/")).is_some());
    assert!(instances.find(&url("https://h.test/")).is_none());

    let kc = MemoryKeychain::new();
    let mut core_a = open_store(&a, &kc).unwrap();
    let core_b = open_store(&b, &kc).unwrap();
    core_a.setting_put("secret", "only in a").unwrap();
    assert!(settings_contain(&core_a, "only in a"));
    assert!(!settings_contain(&core_b, "only in a"));
    drop((core_a, core_b));
    assert_eq!(
        kc.accounts(),
        vec![
            store_keychain_account(&a).unwrap(),
            store_keychain_account(&b).unwrap(),
        ]
    );
    let key_a = kc
        .get(&store_keychain_account(&a).unwrap())
        .unwrap()
        .unwrap();
    let key_b = kc
        .get(&store_keychain_account(&b).unwrap())
        .unwrap()
        .unwrap();
    assert_ne!(*key_a, *key_b);
}

/// A removed id is never listed again by a stale in-memory list (a manager window drawn before the removal).
#[test]
fn removed_instance_not_resurrected() {
    let root = root();
    let mut first = Instances::load(&root).unwrap();
    first.add(url("https://one.example/")).unwrap();
    first.add(url("https://two.example/")).unwrap();
    // A second manager (another window or process) loads both, then the first removes instance 0.
    let mut stale = Instances::load(&root).unwrap();
    assert_eq!(stale.list().len(), 2);
    assert!(first.remove(InstanceId(0)).unwrap().is_some());

    // The stale list adds a server: the removed instance is not written back, and the new one takes the freed id.
    let added = stale.add(url("https://three.example/")).unwrap();
    assert_eq!(added.id, InstanceId(0));
    let on_disk = Instances::load(&root).unwrap();
    let servers: Vec<&str> = on_disk.list().iter().map(|c| c.server.as_str()).collect();
    assert_eq!(servers, ["https://three.example/", "https://two.example/"]);

    // Removing what another process already removed is a no-op, never an error and never a re-save of the entry.
    let mut other = Instances::load(&root).unwrap();
    let mut stale = Instances::load(&root).unwrap();
    other.remove(InstanceId(1)).unwrap();
    assert!(stale.remove(InstanceId(1)).unwrap().is_none());
    assert_eq!(stale.list().len(), 1);
    assert_eq!(Instances::load(&root).unwrap().list().len(), 1);
}

/// Several managers add at once: every addition lands, on its own id (the list's lock serialises the read-modify-write).
#[test]
fn concurrent_additions_are_all_kept() {
    let root = root();
    Instances::load(&root).unwrap();
    let threads: Vec<_> = (0..8)
        .map(|n| {
            let root = root.clone();
            std::thread::spawn(move || {
                let mut instances = Instances::load(&root).unwrap();
                instances
                    .add(url(&format!("https://t{n}.example/")))
                    .unwrap()
                    .id
            })
        })
        .collect();
    let mut ids: Vec<u8> = threads.into_iter().map(|t| t.join().unwrap().0).collect();
    ids.sort_unstable();
    assert_eq!(ids, (0..8).collect::<Vec<u8>>());
    let on_disk = Instances::load(&root).unwrap();
    assert_eq!(on_disk.list().len(), 8);
}

/// Two first launches share one install id.
#[test]
fn concurrent_first_loads_agree_on_the_install_id() {
    let root = root();
    let threads: Vec<_> = (0..6)
        .map(|_| {
            let root = root.clone();
            std::thread::spawn(move || Instances::load(&root).unwrap().install_id().to_owned())
        })
        .collect();
    let ids: Vec<String> = threads.into_iter().map(|t| t.join().unwrap()).collect();
    assert!(ids.windows(2).all(|w| w[0] == w[1]), "{ids:?}");
}

/// The window's hold: a second window of the same instance is refused, a removal is refused while it is open, and both
/// work again once the window closes.
#[test]
fn an_open_window_holds_its_instance() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(url("https://one.example/")).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&cfg, &kc).unwrap());

    let guard = instances.hold(cfg.id).unwrap();
    assert_eq!(guard.id(), cfg.id);
    assert!(matches!(instances.hold(cfg.id), Err(InstanceError::InUse)));
    // Another process's value of the list is refused the same way.
    let other = Instances::load(&root).unwrap();
    assert!(matches!(other.hold(cfg.id), Err(InstanceError::InUse)));
    assert!(matches!(
        instances.remove_erasing(cfg.id, &cfg.server, &kc),
        Err(InstanceError::InUse)
    ));
    assert_eq!(kc.accounts().len(), 1, "a refused removal keeps the key");
    assert!(cfg.data_dir.exists());

    // The client opens its store while the window holds the instance (the store's lock is a different one).
    drop(open_store(&cfg, &kc).unwrap());

    drop(guard);
    assert!(
        instances
            .remove_erasing(cfg.id, &cfg.server, &kc)
            .unwrap()
            .is_some()
    );
}

/// Removing an instance deletes its key from the keychain and its data dir: a crypto-erase.
#[test]
fn removing_an_instance_deletes_its_key() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let a = instances.add(url("https://one.example/")).unwrap();
    let b = instances.add(url("https://two.example/")).unwrap();
    let kc = MemoryKeychain::new();
    drop(open_store(&a, &kc).unwrap());
    drop(open_store(&b, &kc).unwrap());
    assert_eq!(kc.accounts().len(), 2);

    let removed = instances
        .remove_erasing(a.id, &a.server, &kc)
        .unwrap()
        .unwrap();
    assert_eq!(removed.id, a.id);
    assert_eq!(kc.accounts(), vec![store_keychain_account(&b).unwrap()]);
    assert!(!a.data_dir.exists());
    assert!(b.data_dir.exists());
    assert_eq!(instances.list().len(), 1);
    // Removing it again changes nothing.
    assert!(
        instances
            .remove_erasing(a.id, &a.server, &kc)
            .unwrap()
            .is_none()
    );
}

/// The key deletion on its own (the one function the controller rewires to Task 2's account at merge).
#[test]
fn delete_instance_key_needs_a_listed_instance_and_tolerates_a_missing_key() {
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(url("https://one.example/")).unwrap();
    let kc = MemoryKeychain::new();
    delete_instance_key(&kc, &cfg).unwrap();
    drop(open_store(&cfg, &kc).unwrap());
    assert_eq!(kc.accounts().len(), 1);
    delete_instance_key(&kc, &cfg).unwrap();
    assert!(kc.accounts().is_empty());
}

/// A keychain that cannot delete refuses the removal, and nothing is erased.
#[test]
fn a_keychain_that_cannot_delete_refuses_the_removal() {
    struct Stuck(MemoryKeychain);
    impl Keychain for Stuck {
        fn get(
            &self,
            a: &str,
        ) -> Result<Option<zeroize::Zeroizing<Vec<u8>>>, dilla_client::seams::KeychainError>
        {
            self.0.get(a)
        }
        fn set(&self, a: &str, s: &[u8]) -> Result<(), dilla_client::seams::KeychainError> {
            self.0.set(a, s)
        }
        fn delete(&self, _: &str) -> Result<(), dilla_client::seams::KeychainError> {
            Err(dilla_client::seams::KeychainError::Unavailable)
        }
    }
    let root = root();
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(url("https://one.example/")).unwrap();
    let kc = Stuck(MemoryKeychain::new());
    drop(open_store(&cfg, &kc).unwrap());
    assert!(matches!(
        instances.remove_erasing(cfg.id, &cfg.server, &kc),
        Err(InstanceError::Keychain)
    ));
    assert!(cfg.data_dir.exists());
    assert_eq!(instances.list().len(), 1);
}

/// An id removed elsewhere and reused for another server is never erased by a removal meant for the old one: the
/// removal carries the server the person saw, and refuses when the listed server differs. Neither the key nor the data
/// of the new instance is touched.
#[test]
fn a_reused_id_is_never_erased_by_a_stale_removal() {
    let root = root();
    let mut mine = Instances::load(&root).unwrap();
    let old = mine.add(url("https://old.example/")).unwrap();
    let kc = MemoryKeychain::new();
    // Another window removes the old instance and adds a new one, which takes the freed id.
    let mut other = Instances::load(&root).unwrap();
    other.remove(old.id).unwrap();
    let new = other.add(url("https://new.example/")).unwrap();
    assert_eq!(new.id, old.id);
    drop(open_store(&new, &kc).unwrap());
    assert_eq!(kc.accounts().len(), 1);

    assert!(matches!(
        mine.remove_erasing(old.id, &old.server, &kc),
        Err(InstanceError::Changed)
    ));
    assert!(matches!(
        mine.erase_key(old.id, &old.server, &kc, true),
        Err(InstanceError::Changed)
    ));
    assert!(matches!(
        mine.remove_verified(old.id, &old.server, true),
        Err(InstanceError::Changed)
    ));
    assert_eq!(kc.accounts().len(), 1, "the new instance's key stays");
    assert!(new.data_dir.exists());
    assert_eq!(Instances::load(&root).unwrap().list().len(), 1);
    // The removal meant for the new one works.
    assert!(
        mine.remove_erasing(new.id, &new.server, &kc)
            .unwrap()
            .is_some()
    );
}
