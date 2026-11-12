//! The parity world: a real dillad with the channel-view fixture in it, for shooting the web client's reference
//! (`clients/desktop/parity/references/shoot-channel-view.mjs` runs this; see the README there).
//!
//! `cargo run -p dilla-desktop --example parity_world -- <out-dir>` starts dilla-testhost serving
//! `packages/web/dist`, signs up Ada, Björn and Mira, makes the community "Midgard Crew" with #general, posts the fixture's
//! messages from the three of them in order, stops their clients (so nobody is online but the browser that signs in
//! later) and writes:
//!
//! - `<out-dir>/state.json`: the instance URL, the control URL and Ada's password, TOTP secret and recovery key (throwaway
//!   secrets: keep `<out-dir>` in an ignored path);
//! - `<out-dir>/fixture.json`: what Ada's client holds (community, channels, members, the #general timeline), the input
//!   of the GPUI channel view in the parity test. It holds no secret.
//!
//! Once the accounts and the channel exist it writes `state.json`, prints `SIGNIN <out-dir>` and waits for a line
//! `post` on stdin (the browser signs in meanwhile); then it posts the history, writes `fixture.json`, prints
//! `READY <out-dir>` and keeps the host running until its stdin closes.

#[path = "../tests/support/world.rs"]
mod world;

use std::io::Read;
use std::time::Duration;

use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState};

/// The messages, oldest first: (author index, body). The text of `experiments/gpui-pixel-proof/fixture.json` (Task 3),
/// without its code block and reactions (the slice's timeline draws plain text).
const MESSAGES: &[(usize, &str)] = &[
    (0, "morning, the release branch is green again"),
    (1, "nice. did the voice e2e pass on all three platforms?"),
    (0, "linux and windows yes, macos is still running"),
    (
        2,
        "I pushed the fix for the sealed-command retry\nit needs a second pair of eyes before merge",
    ),
    (1, "on it, give me ten minutes"),
    (2, "the core of the change is the outbox"),
    (0, "reads fine to me, the outbox keeps the order"),
    (1, "approved, merging after ci"),
    (2, "thanks both"),
    (0, "ci is green, the tag goes out this afternoon"),
    (1, "I will tell the homelab session to upgrade CT 144"),
    (2, "perfect, nothing else from me"),
];

fn json_string(s: &str) -> String {
    serde_json::to_string(s).unwrap()
}

fn fixture_json(state: &AppState, community: CommunityId, general: ChannelId, now: u64) -> String {
    let user = state.account.user_id.expect("signed in");
    let members = &state.members[&community];
    let me = members.iter().find(|m| m.user_id == user).expect("own row");
    let mut out = String::from("{\n");
    out += &format!("  \"now\": {now},\n");
    out += &format!(
        "  \"user\": {{\"id\": \"{}\", \"username\": {}, \"display\": {}}},\n",
        user.hex(),
        json_string(&me.username),
        json_string(&me.display)
    );
    let c = state
        .communities
        .iter()
        .find(|c| c.id == community)
        .unwrap();
    out += &format!(
        "  \"communities\": [{{\"id\": \"{}\", \"name\": {}, \"owner\": \"{}\"}}],\n",
        c.id.hex(),
        json_string(&c.name),
        c.owner.hex()
    );
    let channels: Vec<String> = state
        .channels
        .iter()
        .filter(|ch| ch.community_id == community)
        .map(|ch| {
            format!(
                "    {{\"id\": \"{}\", \"kind\": {}, \"mode\": {}, \"name\": {}, \"topic\": {}, \"parent\": {}, \"position\": {}}}",
                ch.id.hex(),
                ch.kind,
                ch.mode,
                json_string(&ch.name),
                json_string(&ch.topic),
                ch.parent_id.map_or("null".to_owned(), |p| format!("\"{}\"", p.hex())),
                ch.position
            )
        })
        .collect();
    out += &format!("  \"channels\": [\n{}\n  ],\n", channels.join(",\n"));
    let rows: Vec<String> = members
        .iter()
        .map(|m| {
            format!(
                "    {{\"id\": \"{}\", \"username\": {}, \"display\": {}, \"kind\": {}}}",
                m.user_id.hex(),
                json_string(&m.username),
                json_string(&m.display),
                m.kind
            )
        })
        .collect();
    out += &format!("  \"members\": [\n{}\n  ],\n", rows.join(",\n"));
    out += &format!("  \"open\": \"{}\",\n", general.hex());
    let items: Vec<String> = state.timelines[&general]
        .items
        .iter()
        .map(|i| {
            format!(
                "    {{\"key\": {}, \"sender\": \"{}\", \"ts\": {}, \"own\": {}, \"body\": {}}}",
                json_string(&i.key),
                i.sender_user.map(|u| u.hex()).unwrap_or_default(),
                i.ts,
                i.own,
                json_string(&i.body)
            )
        })
        .collect();
    out += &format!("  \"messages\": [\n{}\n  ]\n}}\n", items.join(",\n"));
    out
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let out = std::path::PathBuf::from(
        std::env::args()
            .nth(1)
            .expect("usage: parity_world <out-dir>"),
    );
    std::fs::create_dir_all(&out).unwrap();
    let web = world::repo_root().join("packages/web/dist");
    assert!(
        web.join("index.html").exists(),
        "build the web client first: npm run build -w @dilla/web"
    );
    let listen = std::env::var("PARITY_WORLD_LISTEN").unwrap_or_else(|_| "127.0.0.1:0".into());
    let host =
        tokio::task::spawn_blocking(move || world::start_host("parity", &listen, Some(&web)))
            .await
            .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = world::sign_up(&host, &invite, "ada", "Ada", "parity").await;
    let bo = world::sign_up(&host, &invite, "bjorn", "Björn", "parity").await;
    let cy = world::sign_up(&host, &invite, "mira", "Mira", "parity").await;
    let (host, ada, bo, cy, community) = tokio::task::spawn_blocking(move || {
        let c = world::community(&host, &ada, "Midgard Crew", &[&bo, &cy]);
        for p in [&ada, &bo, &cy] {
            world::clear_session(p);
        }
        (host, ada, bo, cy, c)
    })
    .await
    .unwrap();
    let community = CommunityId(community);
    let people = [&ada, &bo, &cy];
    let mut clients = Vec::new();
    for p in people {
        clients.push(world::start(p).await);
    }
    let mut general = None;
    for client in &clients {
        let s = world::wait_for(client, Duration::from_secs(30), "the channel list", |s| {
            s.channel_named(community, "general").is_some()
        })
        .await;
        general = s.channel_named(community, "general").map(|c| c.id);
    }
    let general = general.unwrap();
    for client in &clients {
        client
            .open_channel(general)
            .await
            .unwrap()
            .expect("opens #general");
    }
    // The browser that shoots the reference signs in now, before the history: a device added later cannot read what was
    // sent before it joined (forward secrecy), the same order the UX world keeps for Ada's phone.
    let secrets = serde_json::json!({
        "publicUrl": host.public,
        "control": host.control,
        "username": ada.username,
        "password": world::PASSWORD,
        "totpSecret": ada.totp_secret,
        "totpLast": ada.totp_last,
        "recoveryKey": ada.recovery_key,
        "server": "Midgard Crew",
    });
    std::fs::write(out.join("state.json"), secrets.to_string()).unwrap();
    println!("SIGNIN {}", out.display());
    let mut line = String::new();
    std::io::stdin().read_line(&mut line).unwrap();
    assert_eq!(line.trim(), "post", "expected `post` on stdin");
    for (n, (author, body)) in MESSAGES.iter().enumerate() {
        clients[*author]
            .send_text(general, (*body).to_owned())
            .await
            .unwrap()
            .expect("queued");
        // Every client holds the message before the next one goes out, so the order is the fixture's.
        for client in &clients {
            world::wait_for(client, Duration::from_secs(20), "the message", |s| {
                s.timelines.get(&general).is_some_and(|t| {
                    t.items
                        .iter()
                        .filter(|i| i.state == TimelineItemState::Ok && i.seq.is_some())
                        .count()
                        > n
                })
            })
            .await;
        }
    }
    let state = world::wait_for(&clients[0], Duration::from_secs(20), "the members", |s| {
        s.members.get(&community).is_some_and(|m| m.len() == 3)
    })
    .await;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    std::fs::write(
        out.join("fixture.json"),
        fixture_json(&state, community, general, now),
    )
    .unwrap();
    for client in clients {
        client.stop();
    }
    println!("READY {}", out.display());
    let mut sink = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut sink);
    drop((host, ada, bo, cy));
}
