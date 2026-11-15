//! Task 2 (screens plan): the device's KeyPackages against a real dillad (`account/keypackages.ts`, controller.ts
//! `topUpKeyPackages`). A sign-up publishes none; every ready tops the directory up to the instance's target once it
//! is below the refill threshold, so the device can be added to a group by the Welcome of someone else's commit.

#[path = "support/world.rs"]
mod world;

use std::time::Duration;

use dilla_core::ids::DeviceId;
use dilla_testkit::DeliveryService;
use dilla_testkit::HttpDs;
use world::*;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn refills_key_packages_below_threshold() {
    let (host, alice, bob, _c) = two_people("kp-refill").await;
    let (target, threshold) = key_package_limits(&host).await;
    assert!(threshold > 1 && target >= threshold);
    assert_eq!(
        key_package_count(&host, &alice).await,
        0,
        "a sign-up publishes none"
    );
    let a = start(&alice).await;
    // The first ready publishes the full set.
    poll(Duration::from_secs(20), "the first top-up", || async {
        (key_package_count(&host, &alice).await == target).then_some(())
    })
    .await;
    // Another device takes all but one of them (each Add of this device to a group consumes one).
    let ds_host = host.public.clone();
    let (bob_device, bob_token, alice_device) = (bob.device_id, bob.token.clone(), alice.device_id);
    tokio::task::spawn_blocking(move || {
        let mut ds =
            HttpDs::with_session(&ds_host, DeviceId::from_bytes(bob_device), bob_token).unwrap();
        for _ in 0..target - 1 {
            ds.take_key_package(&DeviceId::from_bytes(alice_device))
                .expect("a key package");
        }
    })
    .await
    .unwrap();
    assert_eq!(key_package_count(&host, &alice).await, 1);
    // One ready: the device tops up from the count the ready reports.
    let readies = a.state().borrow().connection.readies;
    a.disconnect();
    a.reconnect();
    wait_for(&a, Duration::from_secs(20), "a new ready", |s| {
        s.connection.readies > readies
    })
    .await;
    poll(
        Duration::from_secs(20),
        "the top-up after the ready",
        || async { (key_package_count(&host, &alice).await == target).then_some(()) },
    )
    .await;
    let status = a.key_packages();
    assert_eq!(status.count, Some(target as u32));
    assert_eq!(status.last_error, None);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn fresh_device_can_be_added_by_welcome() {
    let (host, alice, bob, c) = two_people("kp-welcome").await;
    let (target, _) = key_package_limits(&host).await;
    // Alice's fresh device comes online and publishes its KeyPackages; it opens no channel.
    let a = start(&alice).await;
    let channel = general(&a, c).await;
    poll(Duration::from_secs(20), "alice's key packages", || async {
        (key_package_count(&host, &alice).await == target).then_some(())
    })
    .await;
    // Bob registers the channel's group: the instance proposes an Add of every other eligible device that holds a
    // KeyPackage, Bob commits it, and its Welcome brings Alice's device in.
    let b = start(&bob).await;
    general(&b, c).await;
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("bob opens the channel");
    b.send_text(channel, "after the welcome".into())
        .await
        .unwrap()
        .expect("queued");
    // The instance holds a Welcome for Alice's device: Bob's commit carried the instance's Add of it, which only a
    // device holding a KeyPackage gets.
    let socket = Socket::open(&host, &alice).await;
    poll(
        Duration::from_secs(30),
        "a welcome for alice's device",
        || async {
            let (status, body) = socket
                .request("GET", "/v1/welcomes?after=0&limit=64".into(), Vec::new())
                .await;
            assert_eq!(status, 200);
            let rows = dilla_core::cbor::decode_strict(&body, |d| {
                let n = d.array_len()?;
                for _ in 0..n {
                    d.skip()?;
                }
                Ok(n)
            })
            .unwrap();
            (rows > 0).then_some(())
        },
    )
    .await;
    drop(socket);
    // Alice's device learns the channel's group (the channel list read again, as a community change does) and enters
    // it by that Welcome, without opening the channel itself; it reads Bob's message.
    a.refresh();
    wait_for(
        &a,
        Duration::from_secs(30),
        "alice reads bob's message",
        |s| bodies_from(s, channel, bob.user_id) == ["after the welcome"],
    )
    .await;
    assert!(
        key_package_count(&host, &alice).await < target,
        "the Add consumed one of alice's KeyPackages"
    );
}
