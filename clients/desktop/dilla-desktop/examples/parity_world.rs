//! The parity world: a real dillad with a channel-view scenario in it, for shooting the web client's reference
//! (`clients/desktop/parity/references/shoot-channel-view.mjs` runs this; see the README there).
//!
//! `cargo run -p dilla-desktop --example parity_world -- <scenario> <out-dir>` starts dilla-testhost serving
//! `packages/web/dist`, signs the scenario's people up, makes its servers and channels, writes `<out-dir>/state.json`
//! (the instance and Ada's throwaway secrets, the server and channel to open, the window) and prints
//! `SIGNIN <out-dir>`. The browser signs Ada in on a new device and opens the channel meanwhile, then sends `post` on
//! stdin: a device added later cannot read what was sent before it joined. The world posts the history, writes what
//! Ada's own headless client holds as `<out-dir>/fixture.json` (the GPUI side's input; no secrets), stops every client
//! (so only the browser is online), prints `READY <out-dir>` and keeps the host running until its stdin closes.
//!
//! Scenarios:
//! - `basic`: 1280x800; "Midgard Crew" (Ada, Björn, Mira), #general with twelve short messages (the text of the Task 3
//!   fixture without its code block and reactions).
//! - `wide`: 1440x900; "Midgard Crew" and, selected, "Northern Lights Collective" with eleven members (a two-digit count),
//!   a category with a text and a voice channel, and a long-named channel holding a message that wraps and a follow-up.

#[path = "../tests/support/world.rs"]
mod world;

use std::collections::BTreeMap;
use std::io::Read;
use std::time::Duration;

use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState};

struct Scenario {
    window: (u32, u32),
    /// Usernames and display names; the first is the viewer (Ada), whose browser shoots.
    people: &'static [(&'static str, &'static str)],
    /// How many of `people` post (each runs a headless client).
    posters: usize,
    /// Servers: name, members (indices into `people`, the owner first), extra channels.
    servers: Vec<(&'static str, Vec<usize>, Vec<world::ChannelSpec<'static>>)>,
    /// The server (index) and channel the view shows.
    open: (usize, &'static str),
    /// The history: (author index, body), oldest first, posted in `open`.
    messages: &'static [(usize, &'static str)],
}

const LONG_CHANNEL: &str = "release-coordination-and-long-running-discussions";

fn scenario(name: &str) -> Scenario {
    match name {
        "basic" => Scenario {
            window: (1280, 800),
            people: &[("ada", "Ada"), ("bjorn", "Björn"), ("mira", "Mira")],
            posters: 3,
            servers: vec![("Midgard Crew", vec![0, 1, 2], Vec::new())],
            open: (0, "general"),
            messages: &[
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
            ],
        },
        "wide" => Scenario {
            window: (1440, 900),
            people: &[
                ("ada", "Ada"),
                ("bjorn", "Björn"),
                ("mira", "Mira"),
                ("liv", "Liv"),
                ("sven", "Sven"),
                ("kari", "Kari"),
                ("ole", "Ole"),
                ("glenn", "Glenn"),
                ("ines", "Ines"),
                ("tove", "Tove"),
                ("ulf", "Ulf"),
            ],
            posters: 4,
            servers: vec![
                ("Midgard Crew", vec![0, 1, 2], Vec::new()),
                (
                    "Northern Lights Collective",
                    (0..11).collect(),
                    vec![
                        (0, LONG_CHANNEL, None),
                        (2, "projects", None),
                        (0, "desktop", Some("projects")),
                        (1, "standup", Some("projects")),
                    ],
                ),
            ],
            open: (1, LONG_CHANNEL),
            messages: &[
                (1, "morning! the desktop preview builds again"),
                (
                    2,
                    "I went through the whole release checklist for the preview this morning and everything that \
                     touches the timeline, the composer and the sign-in flow behaves the way the web client does, \
                     including the wrapping of long lines like this one, which keeps going well past the width of the \
                     message column so that it has to break onto a second and a third line before it ends",
                ),
                (2, "the only thing left is the member count in the header"),
                (0, "great, then the two-digit count is the last check"),
                (3, "eleven of us now, so it is two digits"),
            ],
        },
        other => panic!("unknown scenario {other}: basic or wide"),
    }
}

fn json_string(s: &str) -> String {
    serde_json::to_string(s).unwrap()
}

fn fixture_json(
    state: &AppState,
    open: ChannelId,
    communities: &[CommunityId],
    window: (u32, u32),
    now: u64,
) -> String {
    let user = state.account.user_id.expect("signed in");
    let open_community = state.channel(open).unwrap().community_id;
    let me = state.members[&open_community]
        .iter()
        .find(|m| m.user_id == user)
        .expect("own row");
    let mut out = String::from("{\n");
    out += &format!("  \"now\": {now},\n");
    out += &format!("  \"window\": [{}, {}],\n", window.0, window.1);
    out += &format!(
        "  \"user\": {{\"id\": \"{}\", \"username\": {}, \"display\": {}}},\n",
        user.hex(),
        json_string(&me.username),
        json_string(&me.display)
    );
    let rows: Vec<String> = communities
        .iter()
        .map(|id| {
            let c = state.communities.iter().find(|c| c.id == *id).unwrap();
            format!(
                "    {{\"id\": \"{}\", \"name\": {}, \"owner\": \"{}\"}}",
                c.id.hex(),
                json_string(&c.name),
                c.owner.hex()
            )
        })
        .collect();
    out += &format!("  \"communities\": [\n{}\n  ],\n", rows.join(",\n"));
    let channels: Vec<String> = state
        .channels
        .iter()
        .filter(|ch| communities.contains(&ch.community_id))
        .map(|ch| {
            format!(
                "    {{\"id\": \"{}\", \"community\": \"{}\", \"kind\": {}, \"mode\": {}, \"name\": {}, \"topic\": {}, \"parent\": {}, \"position\": {}}}",
                ch.id.hex(),
                ch.community_id.hex(),
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
    let mut members = BTreeMap::new();
    for id in communities {
        let rows: Vec<String> = state.members[id]
            .iter()
            .map(|m| {
                format!(
                    "      {{\"id\": \"{}\", \"username\": {}, \"display\": {}, \"kind\": {}}}",
                    m.user_id.hex(),
                    json_string(&m.username),
                    json_string(&m.display),
                    m.kind
                )
            })
            .collect();
        members.insert(id.hex(), rows.join(",\n"));
    }
    let members: Vec<String> = members
        .into_iter()
        .map(|(id, rows)| format!("    \"{id}\": [\n{rows}\n    ]"))
        .collect();
    out += &format!("  \"members\": {{\n{}\n  }},\n", members.join(",\n"));
    out += &format!("  \"open\": \"{}\",\n", open.hex());
    let items: Vec<String> = state.timelines[&open]
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
    let mut args = std::env::args().skip(1);
    let usage = "usage: parity_world <basic|wide> <out-dir>";
    let sc = scenario(&args.next().expect(usage));
    let out = std::path::PathBuf::from(args.next().expect(usage));
    std::fs::create_dir_all(&out).unwrap();
    let web = world::repo_root().join("packages/web/dist");
    assert!(
        web.join("index.html").exists(),
        "build the web client first: npm run build -w @dilla/web"
    );
    let host =
        tokio::task::spawn_blocking(move || world::start_host("parity", "127.0.0.1:0", Some(&web)))
            .await
            .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut people = Vec::new();
    for (username, display) in sc.people {
        people.push(world::sign_up(&host, &invite, username, display, "parity").await);
    }
    let servers = sc.servers.clone();
    let (host, people, communities) = tokio::task::spawn_blocking(move || {
        let mut ids = Vec::new();
        for (name, members, channels) in &servers {
            let owner = &people[members[0]];
            let others: Vec<&world::Person> = members[1..].iter().map(|m| &people[*m]).collect();
            ids.push(CommunityId(world::community_with(
                &host, owner, name, &others, channels,
            )));
        }
        for p in &people {
            world::clear_session(p);
        }
        (host, people, ids)
    })
    .await
    .unwrap();
    let open_community = communities[sc.open.0];
    let mut clients = Vec::new();
    for p in &people[..sc.posters] {
        clients.push(world::start(p).await);
    }
    let mut open = None;
    for client in &clients {
        let s = world::wait_for(client, Duration::from_secs(30), "the channel list", |s| {
            s.channel_named(open_community, sc.open.1).is_some()
        })
        .await;
        open = s.channel_named(open_community, sc.open.1).map(|c| c.id);
    }
    let open = open.unwrap();
    for client in &clients {
        client
            .open_channel(open)
            .await
            .unwrap()
            .expect("opens the channel");
    }
    let viewer = &people[0];
    let secrets = serde_json::json!({
        "publicUrl": host.public,
        "control": host.control,
        "username": viewer.username,
        "password": world::PASSWORD,
        "totpSecret": viewer.totp_secret,
        "totpLast": viewer.totp_last,
        "recoveryKey": viewer.recovery_key,
        "server": sc.servers[sc.open.0].0,
        "channel": sc.open.1,
        "window": [sc.window.0, sc.window.1],
        "lastMessage": sc.messages.last().unwrap().1,
    });
    std::fs::write(out.join("state.json"), secrets.to_string()).unwrap();
    println!("SIGNIN {}", out.display());
    let mut line = String::new();
    std::io::stdin().read_line(&mut line).unwrap();
    assert_eq!(line.trim(), "post", "expected `post` on stdin");
    for (n, (author, body)) in sc.messages.iter().enumerate() {
        clients[*author]
            .send_text(open, (*body).to_owned())
            .await
            .unwrap()
            .expect("queued");
        // Every client holds the message before the next one goes out, so the order is the scenario's.
        for client in &clients {
            world::wait_for(client, Duration::from_secs(20), "the message", |s| {
                s.timelines.get(&open).is_some_and(|t| {
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
    let members = sc.servers[sc.open.0].1.len();
    let state = world::wait_for(&clients[0], Duration::from_secs(20), "the members", |s| {
        communities.iter().enumerate().all(|(i, c)| {
            s.members
                .get(c)
                .is_some_and(|m| m.len() == sc.servers[i].1.len())
        }) && s.members[&open_community].len() == members
    })
    .await;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    std::fs::write(
        out.join("fixture.json"),
        fixture_json(&state, open, &communities, sc.window, now),
    )
    .unwrap();
    for client in clients {
        client.stop();
    }
    println!("READY {}", out.display());
    let mut sink = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut sink);
    drop((host, people));
}
