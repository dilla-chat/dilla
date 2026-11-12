//! Task 2 (screens plan): the account's backup objects against a real dillad (`ensureBackups` and `repairBackupState`
//! of `account/enrol.ts`, in controller.ts `onReady`'s order: ensure, refresh the own device list, repair). The fake-port
//! cases of `enrol.test.ts` are `account::backups`' unit tests; these run the same steps against the instance.

#[path = "support/world.rs"]
mod world;

use std::time::Duration;

use dilla_client::account::backups::{Sealed, decode_sealed};
use dilla_client::http::routes::{self as r, NoBody, Raw};
use dilla_client::seams::Http;
use dilla_client::store::open_store;
use dilla_core::cbor::decode_strict;
use world::*;

/// The device's own sealed objects, read from its store (its client stopped or not started).
fn sealed(p: &Person) -> Sealed {
    let core = open_store(&p.cfg, p.kc.as_ref()).unwrap();
    decode_sealed(&core.sealed_objects().unwrap()).unwrap()
}

/// The kinds the instance holds (`GET /v1/backups`), over the sign-up session.
async fn kinds(host: &Host, p: &Person) -> Vec<u64> {
    let Raw(raw) = authed(host, p)
        .call::<r::ListBackups>(&[], NoBody)
        .await
        .expect("list backups");
    decode_strict(&raw, |d| {
        let n = d.array_len()?;
        let mut out = Vec::new();
        for _ in 0..n {
            let len = d.array_len()?;
            out.push(d.uint()?);
            for _ in 1..len {
                d.skip()?;
            }
        }
        Ok(out)
    })
    .unwrap()
}

/// The stored object of `kind`, or `None`.
async fn stored(host: &Host, p: &Person, kind: u8) -> Option<Vec<u8>> {
    let reply = authed(host, p)
        .call::<r::GetBackup>(&[&kind.to_string()], NoBody)
        .await
        .expect("get backup");
    if reply.status == 404 {
        return None;
    }
    Some(
        decode_strict(&reply.body, |d| {
            let len = d.array_len()?;
            let object = d.bytes()?.to_vec();
            for _ in 1..len {
                d.skip()?;
            }
            Ok(object)
        })
        .unwrap(),
    )
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn ensure_backups_uploads_missing_state() {
    let (host, alice, _bob, _c) = two_people("bk-ensure").await;
    let own = sealed(&alice);
    let (root, state) = (own.root.clone().unwrap(), own.state.clone().unwrap());
    assert!(
        kinds(&host, &alice).await.is_empty(),
        "the instance holds no backup yet"
    );
    let a = start(&alice).await;
    // The first ready's pass uploads what the instance is missing: the root, then the state.
    poll(Duration::from_secs(20), "both objects stored", || async {
        let k = kinds(&host, &alice).await;
        (k.contains(&0) && k.contains(&1)).then_some(())
    })
    .await;
    assert_eq!(stored(&host, &alice, 0).await.unwrap(), root);
    assert_eq!(stored(&host, &alice, 1).await.unwrap(), state);
    poll(Duration::from_secs(10), "the pass recorded", || async {
        a.backups().last_pass.is_some().then_some(())
    })
    .await;
    let status = a.backups();
    assert_eq!(status.ensure_error, None, "{status:?}");
    assert_eq!(status.refresh_error, None, "{status:?}");
    assert!(!a.backup_root_mismatch());
    stop(a).await;
    assert!(
        sealed(&alice).state_uploaded,
        "the state is marked uploaded"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn repair_backup_state_after_list_refresh() {
    let (host, alice, _bob, _c) = two_people("bk-repair").await;
    let state = sealed(&alice).state.unwrap();
    let a = start(&alice).await;
    poll(Duration::from_secs(20), "the state stored", || async {
        (stored(&host, &alice, 1).await.as_deref() == Some(state.as_slice())).then_some(())
    })
    .await;
    // A stolen session replaces the state object with junk (BACKUPS-RECOVERY-03). The listing still shows a state, and
    // this device's own is marked uploaded, so only the repair (after the own list is refreshed) can put it back.
    let junk = cbor(|e| {
        e.array(3).uint(1).bytes(&[0x0e; 12]).bytes(&[0x0f; 16]);
    });
    authed(&host, &alice)
        .call::<r::PutBackup>(
            &["1"],
            Raw(cbor(|e| {
                e.array(1).bytes(&junk);
            })),
        )
        .await
        .expect("junk stored");
    assert_eq!(stored(&host, &alice, 1).await.unwrap(), junk);
    // The next ready repairs it.
    let readies = a.state().borrow().connection.readies;
    a.disconnect();
    a.reconnect();
    wait_for(&a, Duration::from_secs(20), "a new ready", |s| {
        s.connection.readies > readies
    })
    .await;
    poll(Duration::from_secs(20), "the state repaired", || async {
        (stored(&host, &alice, 1).await.as_deref() == Some(state.as_slice())).then_some(())
    })
    .await;
    let status = a.backups();
    assert_eq!(status.refresh_error, None, "{status:?}");
    assert_eq!(
        a.state().borrow().account.phase,
        dilla_client::state::AccountPhase::Ready,
        "a listed device stays signed in"
    );
}
