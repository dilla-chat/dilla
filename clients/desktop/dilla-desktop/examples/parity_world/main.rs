//! The parity world: a real dillad with a screen fixture's world in it, for shooting the web client's reference
//! (`clients/desktop/parity/shoot.mjs` runs this; see `parity/references/linux/README.md`).
//!
//! `cargo run -p dilla-desktop --example parity_world -- <fixture.json> <out-dir>` reads a screen fixture
//! (`dilla_client::testing::spec`), starts dilla-testhost serving `packages/web/dist`, signs the world's people up,
//! makes its servers and channels and writes `<out-dir>/state.json` (the viewer's throwaway secrets, the channels the
//! browser visits so it can read their history, the window, the host's clock and every local id's real id) before it
//! prints `SIGNIN <out-dir>`. The browser signs the viewer in on a new device and visits those channels meanwhile, then
//! sends `post` on stdin: a device added later cannot read what was sent before it joined. The world posts the history
//! in time order, writes `<out-dir>/world.json` (the reference's sidecar: the fixture's digest, the clock shift and the
//! real ids), stops every client (so only the browser is online), prints `READY <out-dir>` and keeps the host running
//! until its stdin closes. Every line it logs for the shooters begins `FIXTURE`: the same line the GPUI driver's
//! `fixture` command answers (`shoot.mjs --check` prints it too).
//!
//! Time: the host's clock is frozen and only moves forward from the moment the host starts, so a fixture's absolute
//! times are not reachable. The world lifts the whole fixture by `shift` seconds (every message's `ts` and the clock)
//! and records it in the sidecar; the GPUI side lifts its frame by the same amount.
//!
//! Scope: plain text messages in text channels. A fixture that asks for more (reactions, replies, threads,
//! attachments, polls, edits, pins, DMs, calls, roles, bots, statuses) is refused by name until the area that owns the
//! facet adds its seeding in its own file under `seed/` (one file per area; `seed/mod.rs` only lists them).

use std::collections::BTreeMap;
use std::io::Read;
use std::time::Duration;

use dilla_client::state::{ChannelId, CommunityId, TimelineItemState};
use dilla_client::testing::host as world;
use dilla_client::testing::spec::{self, ChannelKind, ScreenFixture, ServerSpec};

mod seed;

/// The server's channels the world makes: every non-system one but the first `general`, which the world makes itself,
/// each at the fixture's `position` when it gives one (the web client orders a server's channels by position, then id).
fn extra_channels(server: &ServerSpec) -> Vec<world::PlacedChannel<'_>> {
    let made: Vec<_> = server.channels.iter().filter(|c| !c.system).collect();
    assert!(
        made.first()
            .is_some_and(|c| c.kind == ChannelKind::Text && c.name == "general"),
        "server {}: the world makes #general first, so the first non-system channel must be the text channel `general`",
        server.id
    );
    made[1..]
        .iter()
        .map(|c| {
            let parent = c.parent.as_deref().map(|p| {
                server
                    .channels
                    .iter()
                    .find(|x| x.id == p)
                    .expect("validated parent")
                    .name
                    .as_str()
            });
            (
                (u64::from(c.kind.wire()), c.name.as_str(), parent),
                c.position.map(u64::from),
            )
        })
        .collect()
}

/// internal/gateway/presence.go `presenceIdleAfter`: a device with no input for this long is idle.
const PRESENCE_IDLE_S: u64 = 10 * 60;
/// What the browser's sign-in may move the host clock by (shoot.mjs `nextCode` steps it 30 s at a time).
const SIGN_IN_CLOCK_S: u64 = 5 * 60;

/// The host's clock, unix seconds (a blocking call on a thread of its own).
fn read_clock(host: &world::Host) -> u64 {
    tokio::task::block_in_place(|| world::clock_now(host))
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let mut args = std::env::args().skip(1);
    let usage = "usage: parity_world <fixture.json> <out-dir>";
    let fixture_path = args.next().expect(usage);
    let out = std::path::PathBuf::from(args.next().expect(usage));
    let text = std::fs::read_to_string(&fixture_path).expect("the fixture file reads");
    let f = ScreenFixture::parse(&text).unwrap_or_else(|e| panic!("{fixture_path}: {e}"));
    let digest = spec::digest(text.as_bytes());
    println!(
        "{}",
        f.fixture_line(&digest).replacen("fixture ", "FIXTURE ", 1)
    );
    let missing = seed::unseedable(&f);
    assert!(
        missing.is_empty(),
        "the parity world cannot seed {} yet (see `seed/` in this example)",
        missing.join(", ")
    );
    std::fs::create_dir_all(&out).unwrap();
    let web = world::repo_root().join("packages/web/dist");
    assert!(
        web.join("index.html").exists(),
        "build the web client first: npm run build -w @dilla/web"
    );
    // A fixture whose viewer joins a call in the browser needs the host's LiveKit (seed/calls.rs).
    let sfu = seed::needs_sfu(&f);
    let host = tokio::task::spawn_blocking(move || {
        if sfu {
            world::start_host_with_sfu("parity", Some(&web))
        } else {
            world::start_host("parity", "127.0.0.1:0", Some(&web))
        }
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut people: Vec<(String, world::Person)> = Vec::new();
    for p in &f.world.people {
        let person = world::sign_up(&host, &invite, p.username(), &p.display, "parity").await;
        people.push((p.id.clone(), person));
    }
    let person_of = |id: &str| &people.iter().find(|(p, _)| p == id).expect("validated").1;
    let communities: Vec<(String, CommunityId)> = {
        let mut made = Vec::new();
        for server in &f.world.servers {
            let owner = person_of(&server.owner);
            let others: Vec<&world::Person> = server
                .members
                .iter()
                .filter(|m| **m != server.owner)
                .map(|m| person_of(m))
                .collect();
            let channels = extra_channels(server);
            let id = tokio::task::block_in_place(|| {
                world::community_placed(&host, owner, &server.name, &others, &channels)
            });
            made.push((server.id.clone(), CommunityId(id)));
        }
        for (_, p) in &people {
            world::clear_session(p);
        }
        made
    };
    seed::before_sign_in(&f, &people).await;
    // A headless client for the viewer (who must hold every group the browser reads, and reads the channel list) and for
    // everyone who posts: an empty world has only the viewer's.
    let plan = f.seed_plan();
    let mut clients = Vec::new();
    for id in &plan.clients {
        clients.push((id.as_str(), world::start(person_of(id)).await));
    }
    // The real ids of every server and channel: by name within the server.
    let mut ids: BTreeMap<String, String> = BTreeMap::new();
    for (id, p) in &people {
        ids.insert(format!("person:{id}"), world::hex(&p.user_id));
    }
    let first = &clients[0].1;
    for (server, community) in &communities {
        ids.insert(format!("server:{server}"), community.hex());
        let spec_server = f.world.servers.iter().find(|s| s.id == *server).unwrap();
        for c in &spec_server.channels {
            let community = *community;
            let name = c.name.clone();
            let state = world::wait_for(
                first,
                Duration::from_secs(30),
                "the channel list",
                move |s| s.channel_named(community, &name).is_some(),
            )
            .await;
            let real = state.channel_named(community, &c.name).unwrap().id;
            ids.insert(format!("channel:{server}/{}", c.id), real.hex());
        }
    }
    // What the browser must find made before it signs in, with the ids its route names (the DMs, seed/dms.rs).
    seed::before_browser(&f, &clients, &mut ids).await;
    let channel_real = |path: &str| -> ChannelId {
        ChannelId(spec::unhex16(&ids[&format!("channel:{path}")]).expect("hex"))
    };
    // Every channel the browser visits (the history's, and the route's own even when it is empty): each client opens it
    // (joins its group).
    for path in &plan.visit {
        for (_, client) in &clients {
            client
                .open_channel(channel_real(path))
                .await
                .unwrap()
                .expect("opens the channel");
        }
    }
    let visit: Vec<serde_json::Value> = plan
        .visit
        .iter()
        .map(|path| {
            let (server, channel) = f.world.channel(path).expect("validated");
            serde_json::json!({"server": server.name, "channel": channel.name})
        })
        .collect();
    let first_ts = f
        .world
        .messages
        .iter()
        .map(|m| m.ts)
        .min()
        .unwrap_or(f.clock);
    let host_now = read_clock(&host);
    // The host's clock only moves forward: lift the fixture past it, with a minute to spare for what is still to be set up,
    // and past the gateway's idle time (`presenceIdleAfter`), so the browser's one input while it signs in and visits
    // (App.tsx reports activity at most every 5 s of `Date.now()`, which the frozen page clock never passes again) is
    // always older than that at the frame: the viewer is idle in every reference, as the GPUI mapping draws it
    // (`fixture/members.rs`). Whether that input reached the gateway before its socket was ready used to decide it. Five
    // more minutes cover the clock the sign-in moves (the shooter steps it 30 s per authenticator code it needs).
    let shift = (host_now + 60 + PRESENCE_IDLE_S + SIGN_IN_CLOCK_S).saturating_sub(first_ts);
    let viewer = person_of(&f.viewer);
    let secrets = serde_json::json!({
        "publicUrl": host.public,
        "control": host.control,
        "username": viewer.username,
        "password": world::PASSWORD,
        "totpSecret": viewer.totp_secret,
        "totpLast": viewer.totp_last,
        "recoveryKey": viewer.recovery_key,
        // The host's invite, for a fixture whose browser makes an account of its own (shoot.mjs `{invite}`).
        "invite": invite,
        "visit": visit,
        "window": [f.window.0, f.window.1],
        "now": f.clock + shift,
        "shift": shift,
        "ids": ids,
        "lastMessage": f.world.messages.iter().max_by_key(|m| m.ts).map(|m| m.body.clone()),
    });
    std::fs::write(out.join("state.json"), secrets.to_string()).unwrap();
    println!("SIGNIN {}", out.display());
    let mut line = String::new();
    std::io::stdin().read_line(&mut line).unwrap();
    assert_eq!(line.trim(), "post", "expected `post` on stdin");
    // History in time order (the file's order within a tie).
    let mut order: Vec<&spec::MessageSpec> = f.world.messages.iter().collect();
    order.sort_by_key(|m| m.ts);
    let mut counts: BTreeMap<&str, usize> = BTreeMap::new();
    let mut posted: std::collections::HashMap<String, dilla_client::state::MessageId> =
        Default::default();
    for m in order {
        let at = m.ts + shift;
        let now = read_clock(&host);
        assert!(
            at >= now,
            "message {} is {} s before the host's clock",
            m.key,
            now - at
        );
        if at > now {
            tokio::task::block_in_place(|| world::advance_clock(&host, at - now));
        }
        let channel = channel_real(&m.channel);
        let client = &clients.iter().find(|(id, _)| *id == m.sender).unwrap().1;
        let sent = match seed::post(m, client, channel, &posted).await {
            Some(sent) => sent,
            None => client.send_text(channel, m.body.clone()).await.unwrap(),
        };
        posted.insert(m.key.clone(), sent.expect("queued"));
        let n = counts.entry(&m.channel).or_default();
        // A thread reply is not a row of the channel: it is not counted, and the clients get a moment to hold it.
        if m.thread_of.is_some() {
            tokio::time::sleep(Duration::from_millis(2500)).await;
            continue;
        }
        *n += 1;
        let want = *n;
        // Every client holds the message before the next one goes out, so the order is the fixture's.
        for (_, c) in &clients {
            world::wait_for(c, Duration::from_secs(20), "the message", |s| {
                s.timelines.get(&channel).is_some_and(|t| {
                    t.items
                        .iter()
                        .filter(|i| i.state == TimelineItemState::Ok && i.seq.is_some())
                        .count()
                        >= want
                })
            })
            .await;
        }
    }
    seed::seed(&f, &people, &clients, &host, &ids).await;
    // The clock at the frame: the fixture's, lifted.
    let target = f.clock + shift;
    let now = read_clock(&host);
    if target > now {
        tokio::task::block_in_place(|| world::advance_clock(&host, target - now));
    }
    let sidecar = spec::Shot {
        digest,
        shift,
        hand_migrated: false,
        ids: ids.clone(),
    };
    std::fs::write(
        out.join("world.json"),
        serde_json::to_string_pretty(&sidecar).unwrap(),
    )
    .unwrap();
    // A client the browser still needs stays online until the end (seed/dms.rs: the browser joins a DM's group when it
    // opens the DM, and a member device online admits it); every other one stops.
    let mut online = Vec::new();
    for (id, client) in clients {
        if seed::keep_online(&f, id) {
            online.push(client);
        } else {
            client.stop();
        }
    }
    println!("READY {}", out.display());
    let mut sink = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut sink);
    for client in online {
        client.stop();
    }
    drop((host, people));
}
