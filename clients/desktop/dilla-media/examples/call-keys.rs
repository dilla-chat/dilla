//! Prints the media keys of one real call group for the browser interop spec
//! (browser/desktop-web-call.spec.ts), as JSON on one line: alice is the desktop (her `ClientCore`'s
//! `call_media_epoch`), bob the web client (his `DillaGroup::media_epoch`), for the join epoch and
//! for the epoch after bob's commit (an UpdatePath). Test support only: the group lives in memory
//! (dilla-core's own test fixtures) and nothing is persisted.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

use client_support::{CALL, Core, RawPeer, call_raw_bob, decode_media_epoch};
use serde_json::{Value, json};

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn alice(a: &Core) -> Value {
    let m = decode_media_epoch(&a.core.call_media_epoch(&CALL).expect("the gate passes"));
    json!({
        "epoch": m.epoch, "baseKey": hex(&m.base_key), "selfLeaf": m.own_leaf,
        "roster": m.roster.iter().map(|(l, d, _)| json!({"leaf": l, "deviceId": hex(d)})).collect::<Vec<_>>(),
    })
}

fn bob(b: &RawPeer) -> Value {
    let m = b.group.as_ref().unwrap().media_epoch(&b.provider).unwrap();
    json!({
        "epoch": m.epoch, "baseKey": hex(&*m.base_key), "selfLeaf": m.own_leaf,
        "roster": m.roster.iter().map(|r| json!({"leaf": r.leaf_index, "deviceId": hex(r.device_id.as_bytes())})).collect::<Vec<_>>(),
    })
}

fn main() {
    let (mut call, mut a, mut b, _) = call_raw_bob();
    let first = json!({"desktop": alice(&a), "web": bob(&b)});
    b.self_update(&mut call);
    a.sync(&call);
    let second = json!({"desktop": alice(&a), "web": bob(&b)});
    println!(
        "{}",
        json!({
            "group": hex(&CALL),
            "desktopDevice": hex(&a.device),
            "webDevice": hex(&b.device),
            "epochs": [first, second],
        })
    );
}
