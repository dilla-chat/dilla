//! A native peer for the TS <-> Rust interop e2e (`e2e/web/native-interop.spec.ts`): it drives `dilla_client::Client`
//! from a line protocol on stdin and answers on stdout, one line per command.
//!
//! ```text
//! start <instance-dir> <server-url> <control-url> <instance-invite> <community-hex> <community-invite> <username>
//!     -> ok user=<hex> device=<hex>
//! channel <community-hex> <name>        -> ok <channel-hex>            (waits for the channel list)
//! owner <community-hex>                 -> ok <user-hex>               (who owns the community, per this client's state)
//! send <channel-hex> <text...>          -> ok                          (once the instance stored the post)
//! wait <channel-hex> <text...> <ms>     -> ok author=<hex>             (a confirmed message from someone else)
//! quit                                  -> ok                          (then exits)
//! ```
//!
//! `start` signs up the way the app does (the instance's bootstrap invite, the device list, an authenticator app
//! confirmed with the host's clock: `tests/sync.rs` does the same), joins the community with the invite code the web
//! user made, and starts the client on a fresh store under `<instance-dir>`. Failures answer `err <reason>`.
//! Diagnostics go to stderr. This is test tooling: it holds no secrets beyond the throwaway account it creates.

use std::io::{BufRead, Write};
use std::path::Path;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState};
use dilla_client::testing::host::{Person, clear_session, sign_up_at};

type Res<T> = Result<T, String>;

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn unhex16(s: &str) -> Res<[u8; 16]> {
    if s.len() != 32 || !s.is_ascii() {
        return Err(format!("not a 32-digit hex id: {s}"));
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).map_err(|e| format!("{s}: {e}"))?;
    }
    Ok(out)
}

/// Signs up through the shared recipe (`dilla_client::testing::host`). A failing step panics there, on a task of its
/// own, so the panic comes back as this command's `err <reason>`.
async fn sign_up_checked(
    root: &Path,
    server: &str,
    control: &str,
    invite: &str,
    username: &str,
) -> Res<Person> {
    let (root, server, control, invite, username) = (
        root.to_owned(),
        server.to_owned(),
        control.to_owned(),
        invite.to_owned(),
        username.to_owned(),
    );
    let task = tokio::spawn(async move {
        let person = sign_up_at(&server, &control, &root, &invite, &username, &username).await;
        // The client establishes its own session from the device key.
        clear_session(&person);
        person
    });
    task.await.map_err(|e| {
        let why = e.try_into_panic().map_or_else(
            |e| e.to_string(),
            |p| {
                p.downcast_ref::<String>()
                    .cloned()
                    .or_else(|| p.downcast_ref::<&str>().map(|s| (*s).to_owned()))
                    .unwrap_or_else(|| "panicked".to_owned())
            },
        );
        format!("sign-up: {why}")
    })
}

/// Joins `community` with the web user's invite code (the sign-up session's token authorises it).
fn join(server: &str, p: &Person, community: [u8; 16], code: &str) -> Res<()> {
    use dilla_core::ids::DeviceId;
    use dilla_testkit::HttpDs;
    let mut ds = HttpDs::with_session(server, DeviceId::from_bytes(p.device_id), p.token.clone())
        .map_err(|e| format!("session: {e:?}"))?;
    ds.join_community(&community, code)
        .map_err(|e| format!("join: {e:?}"))
}

struct Peer {
    rt: tokio::runtime::Runtime,
    client: Client,
}

impl Peer {
    /// Polls the state until `pred` answers `Some`, at most `within`.
    fn until<T>(
        &self,
        within: Duration,
        what: &str,
        pred: impl Fn(&AppState) -> Option<T>,
    ) -> Res<T> {
        let mut rx = self.client.state();
        let deadline = Instant::now() + within;
        self.rt.block_on(async {
            loop {
                {
                    let s = rx.borrow_and_update().clone();
                    if let Some(v) = pred(&s) {
                        return Ok(v);
                    }
                }
                let left = deadline.saturating_duration_since(Instant::now());
                if left.is_zero() {
                    return Err(format!("timed out waiting for {what}"));
                }
                let _ = tokio::time::timeout(left, rx.changed()).await;
            }
        })
    }
}

fn start(args: &[&str]) -> Res<(Peer, String)> {
    let [dir, server, control, invite, community, code, username] = args else {
        return Err(
            "usage: start <dir> <server> <control> <invite> <community> <code> <username>".into(),
        );
    };
    let community = unhex16(community)?;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    let root = Path::new(dir);
    std::fs::create_dir_all(root).map_err(|e| e.to_string())?;
    let person = rt.block_on(sign_up_checked(root, server, control, invite, username))?;
    join(server, &person, community, code)?;
    // `Client::start` blocks on a runtime of its own: it is called outside this one.
    let client = Client::start(person.cfg.clone(), person.kc.clone())
        .map_err(|e| format!("client start: {e:?}"))?;
    let reply = format!(
        "user={} device={}",
        hex(&person.user_id),
        hex(&person.device_id)
    );
    Ok((Peer { rt, client }, reply))
}

fn channel(peer: &Peer, args: &[&str]) -> Res<String> {
    let [community, name] = args else {
        return Err("usage: channel <community-hex> <name>".into());
    };
    let community = CommunityId(unhex16(community)?);
    let id = peer.until(Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(community, name).map(|c| c.id)
    })?;
    // Joins (or registers) the channel's MLS group, so its timeline is kept.
    peer.rt
        .block_on(peer.client.open_channel(id))
        .map_err(|e| e.to_string())?
        .map_err(|e| format!("open: {e:?}"))?;
    Ok(hex(&id.0))
}

fn owner(peer: &Peer, rest: &str) -> Res<String> {
    let community = CommunityId(unhex16(rest.trim())?);
    peer.until(Duration::from_secs(30), "the community", |s| {
        s.communities
            .iter()
            .find(|c| c.id == community)
            .map(|c| hex(&c.owner.0))
    })
}

fn send(peer: &Peer, rest: &str) -> Res<()> {
    let (ch, text) = rest
        .split_once(' ')
        .ok_or("usage: send <channel-hex> <text>")?;
    let id = ChannelId(unhex16(ch)?);
    peer.rt
        .block_on(peer.client.send_text(id, text.to_owned()))
        .map_err(|e| e.to_string())?
        .map_err(|e| format!("send: {e:?}"))?;
    peer.until(Duration::from_secs(10), "the post to be stored", |s| {
        s.timelines
            .get(&id)?
            .items
            .iter()
            .any(|i| i.own && i.body == text && i.seq.is_some() && i.state == TimelineItemState::Ok)
            .then_some(())
    })
}

fn wait(peer: &Peer, rest: &str) -> Res<String> {
    let usage = "usage: wait <channel-hex> <text> <timeout-ms>";
    let (ch, tail) = rest.split_once(' ').ok_or(usage)?;
    let (text, ms) = tail.rsplit_once(' ').ok_or(usage)?;
    let id = ChannelId(unhex16(ch)?);
    let ms: u64 = ms.parse().map_err(|_| usage.to_owned())?;
    peer.until(Duration::from_millis(ms), "the message", |s| {
        s.timelines.get(&id)?.items.iter().find_map(|i| {
            (!i.own && i.body == text && i.state == TimelineItemState::Ok)
                .then(|| i.sender_user.map(|u| hex(&u.0)).unwrap_or_default())
        })
    })
}

fn main() {
    let stdin = std::io::stdin();
    let mut out = std::io::stdout().lock();
    let mut peer: Option<Peer> = None;
    let mut say = |line: String| {
        let _ = writeln!(out, "{line}");
        let _ = out.flush();
    };
    for line in stdin.lock().lines() {
        let Ok(line) = line else { break };
        let line = line.trim_end_matches('\r');
        let (cmd, rest) = line.split_once(' ').unwrap_or((line, ""));
        if cmd == "quit" {
            if let Some(p) = peer.take() {
                p.client.stop();
                p.rt.shutdown_background();
            }
            say("ok".into());
            return;
        }
        let answer: Res<String> = match (cmd, peer.as_ref()) {
            ("start", None) => {
                let args: Vec<&str> = rest.split(' ').collect();
                start(&args).map(|(p, reply)| {
                    peer = Some(p);
                    reply
                })
            }
            ("start", Some(_)) => Err("already started".into()),
            (_, None) => Err("not started".into()),
            ("channel", Some(p)) => channel(p, &rest.split(' ').collect::<Vec<_>>()),
            ("owner", Some(p)) => owner(p, rest),
            ("send", Some(p)) => send(p, rest).map(|()| String::new()),
            ("wait", Some(p)) => wait(p, rest).map(|author| format!("author={author}")),
            (other, _) => Err(format!("unknown command {other}")),
        };
        match answer {
            Ok(s) if s.is_empty() => say("ok".into()),
            Ok(s) => say(format!("ok {s}")),
            Err(e) => say(format!("err {}", e.replace('\n', " "))),
        }
    }
}
