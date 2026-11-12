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
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::http::routes::{self as r, NoBody, Raw};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Http, InstanceConfig};
use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState};
use dilla_client::store::open_store;
use dilla_core::cbor::{Encoder, decode_strict};

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

fn totp_code(secret_b32: &str, counter: u64) -> Res<String> {
    use hmac::{Hmac, KeyInit, Mac};
    let mut key = Vec::new();
    let (mut buf, mut bits) = (0u32, 0u32);
    for c in secret_b32.trim_end_matches('=').chars() {
        let v = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
            .find(c.to_ascii_uppercase())
            .ok_or("base32 secret")? as u32;
        buf = (buf << 5) | v;
        bits += 5;
        if bits >= 8 {
            bits -= 8;
            key.push((buf >> bits) as u8);
        }
    }
    let mut mac = Hmac::<sha1::Sha1>::new_from_slice(&key).map_err(|e| e.to_string())?;
    mac.update(&counter.to_be_bytes());
    let d = mac.finalize().into_bytes();
    let o = usize::from(d[d.len() - 1] & 0x0f);
    let word = u32::from_be_bytes([d[o] & 0x7f, d[o + 1], d[o + 2], d[o + 3]]);
    Ok(format!("{:06}", word % 1_000_000))
}

fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

struct Person {
    cfg: InstanceConfig,
    kc: Arc<MemoryKeychain>,
    user_id: [u8; 16],
    device_id: [u8; 16],
    token: String,
}

/// signup.ts `submit` from a fresh client core, then the device list and an authenticator app.
async fn sign_up(
    root: &Path,
    server: &str,
    control: &str,
    invite: &str,
    username: &str,
) -> Res<Person> {
    let e = |what: &str, err: &dyn std::fmt::Debug| format!("{what}: {err:?}");
    let mut instances = Instances::load(root).map_err(|x| e("instances", &x))?;
    let base = url::Url::parse(server).map_err(|x| e("server url", &x))?;
    let cfg = instances.add(base.clone()).map_err(|x| e("add", &x))?;
    let kc = Arc::new(MemoryKeychain::new());
    let mut core = open_store(&cfg, kc.as_ref()).map_err(|x| e("store", &x))?;

    let anon = ReqwestHttp::new(base.clone(), AuthToken::anonymous()).map_err(|x| e("http", &x))?;
    let Raw(doc) = anon
        .call::<r::GetInstance>(&[], NoBody)
        .await
        .map_err(|x| e("instance", &x))?;
    let (instance_id, signer) = decode_strict(&doc, |d| {
        let len = d.array_len()?;
        for _ in 0..3 {
            d.skip()?;
        }
        let id = d.bytes_exact::<16>()?;
        for _ in 4..10 {
            d.skip()?;
        }
        let signer = d.bytes_exact::<32>()?;
        for _ in 11..len {
            d.skip()?;
        }
        Ok((id, signer))
    })
    .map_err(|x| e("instance document", &x))?;
    core.signup_begin(&instance_id)
        .map_err(|x| e("signup begin", &x))?;
    let body = core
        .signup_request(
            invite,
            username,
            username,
            Some("correct horse battery staple"),
        )
        .map_err(|x| e("signup request", &x))?;
    let Raw(created) = anon
        .call::<r::PostAccount>(&[], Raw(body))
        .await
        .map_err(|x| e("account", &x))?;
    let (user_id, device_id, token, expires) = decode_strict(&created, |d| {
        let len = d.array_len()?;
        let u = d.bytes_exact::<16>()?;
        let dev = d.bytes_exact::<16>()?;
        let t = d.text()?.to_owned();
        let x = d.uint()?;
        for _ in 4..len {
            d.skip()?;
        }
        Ok((u, dev, t, x))
    })
    .map_err(|x| e("account answer", &x))?;
    core.session_store(&token, expires, expires)
        .map_err(|x| e("session", &x))?;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|x| e("clock", &x))?
        .as_secs();
    core.signup_complete(&user_id, username, now, &signer)
        .map_err(|x| e("signup complete", &x))?;

    let authed =
        ReqwestHttp::new(base, AuthToken::new(token.as_str())).map_err(|x| e("http", &x))?;
    authed
        .call::<r::PutDeviceList>(
            &[&hex(&user_id)],
            Raw(core.device_list_body().map_err(|x| e("device list", &x))?),
        )
        .await
        .map_err(|x| e("device list put", &x))?;
    core.device_list_published()
        .map_err(|x| e("device list published", &x))?;

    // An authenticator app, confirmed with the host's clock.
    let Raw(enrol) = authed
        .call::<r::TotpEnroll>(
            &[],
            Raw(cbor(|en| {
                en.array(1).null();
            })),
        )
        .await
        .map_err(|x| e("totp enroll", &x))?;
    let secret = decode_strict(&enrol, |d| {
        let len = d.array_len()?;
        let s = d.text()?.to_owned();
        for _ in 1..len {
            d.skip()?;
        }
        Ok(s)
    })
    .map_err(|x| e("totp secret", &x))?;
    let clock: serde_json::Value = serde_json::from_slice(
        &reqwest::get(format!("{control}/debug/clock"))
            .await
            .map_err(|x| e("clock get", &x))?
            .bytes()
            .await
            .map_err(|x| e("clock body", &x))?,
    )
    .map_err(|x| e("clock json", &x))?;
    let now = clock["now"].as_u64().ok_or("clock.now")?;
    let code = totp_code(&secret, now / 30)?;
    authed
        .call::<r::TotpConfirm>(
            &[],
            Raw(cbor(|en| {
                en.array(1).text(&code);
            })),
        )
        .await
        .map_err(|x| e("totp confirm", &x))?;
    // Forget the sign-up session: the client establishes its own from the device key.
    core.session_clear().map_err(|x| e("session clear", &x))?;
    drop(core);
    Ok(Person {
        cfg,
        kc,
        user_id,
        device_id,
        token,
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
    let person = rt.block_on(sign_up(root, server, control, invite, username))?;
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
