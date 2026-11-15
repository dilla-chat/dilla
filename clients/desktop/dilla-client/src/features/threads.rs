//! Area **threads** (Task 11): reply, open a thread, page it, separate thread drafts and thread mention badges.
//!
//! Ports: packages/web/src/thread-request.ts, packages/web/src/screens/conversation/ThreadView.tsx and the worker's
//! `openThread` / `closeThread` / `refreshThread` / `threadOf` (packages/client-core/src/worker/controller.ts).
//!
//! * A **reply** (`reply`) is a type-0 post whose `reply_to` names the original; it shows in the channel with a reply line.
//! * A **thread reply** (`send_in_thread`) is a type-0 post whose `thread_id` names a held root; the core keeps it out of
//!   the channel's timeline and lists it in the thread. The outbox row does not name its thread, so this keeps the
//!   pending ones itself ([`is_thread_send`]) until the core stores them (controller.ts `threadSends`).
//! * The **open thread** is read from the core's `thread_timeline` (one page of [`THREAD_PAGE`], older pages with
//!   [`Threads::load_earlier`]) and re-read whenever the client publishes a new state, so a peer's reply shows at once.
//! * **Summaries** (the count and last time under a root in the channel) are read from the channel timeline's 19th
//!   column for every channel the UI [tracks](Threads::track).
//! * **Mention badges** are the core's own: a thread reply that mentions this account counts in the channel's `unread`
//!   map like any other row (the core's `activity` counts a group's rows, thread replies included); the main timeline
//!   never lists the reply.
//! * **Drafts** are kept per thread in the instance's prefs (`threads.draft.<root>`), apart from the channel's draft.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, LazyLock, Mutex, Weak};
use std::time::Duration;

use tokio::sync::{oneshot, watch};

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::state::messages::{TimelineRow, build_timeline, decode_outbox, decode_timeline};
use crate::state::threads::{OpenThread, ReplyTarget, ThreadData, ThreadSummary};
use crate::state::{
    AppState, ChannelGroupState, ChannelId, MessageId, TIMELINE_PAGE, TimelineItem,
};
use crate::sync::api::try_core;
use crate::sync::value::{self, DecodeError, Rows, arr, at_least, bin16, opt, u53};

/// controller.ts `THREAD_PAGE`: the replies read for an open thread, and for each page after it.
pub const THREAD_PAGE: u32 = 200;

type Id = [u8; 16];

/// What one client keeps for this area. It is per client, not per process: two clients in one process (two instances) never
/// see each other's pending sends or loops.
#[derive(Default)]
pub(crate) struct PerClient {
    /// This device's own thread replies that the core has not stored yet: message id -> (thread root, group). The outbox
    /// row does not name its thread (controller.ts `threadSends`), so the pane asks here.
    sends: Mutex<HashMap<Id, (Id, Id)>>,
    /// The refresh loop runs.
    watching: AtomicBool,
    /// What the last refresh saw, so an unrelated publish reads nothing.
    last: Mutex<Option<Signature>>,
    /// Heavy reads (a channel timeline, a thread page): the cost the loop is held to.
    reads: AtomicUsize,
}

/// What a refresh depends on: the tracked channels' groups (their next seq moves with every row, a thread reply
/// included) and shown timelines, the open thread, and this client's pending thread sends.
#[derive(Clone, Debug, PartialEq, Eq)]
struct Signature {
    channels: Vec<ChannelLook>,
    open: Option<OpenThread>,
    pending: usize,
}

/// One tracked channel: its group's (next seq, state) and how many rows its timeline shows.
type ChannelLook = (ChannelId, Option<(u64, u8)>, usize);

impl PerClient {
    fn has_send(&self, msg: &Id) -> bool {
        self.sends.lock().is_ok_and(|s| s.contains_key(msg))
    }

    /// Drops the thread sends of `group` that the outbox no longer holds (stored or discarded).
    fn settle(&self, group: Id, outbox: &HashSet<Id>) {
        if let Ok(mut sends) = self.sends.lock() {
            sends.retain(|msg, (_, g)| *g != group || outbox.contains(msg));
        }
    }
}

type Clients = Vec<(Weak<Inner>, Arc<PerClient>)>;
static CLIENTS: LazyLock<Mutex<Clients>> = LazyLock::new(Mutex::default);

/// The state of `inner`'s client, made on first use.
fn per_client(inner: &Arc<Inner>) -> Arc<PerClient> {
    let mut clients = CLIENTS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    clients.retain(|(w, _)| w.strong_count() > 0);
    let mine = Arc::downgrade(inner);
    if let Some((_, state)) = clients.iter().find(|(w, _)| Weak::ptr_eq(w, &mine)) {
        return state.clone();
    }
    let state = Arc::new(PerClient::default());
    clients.push((mine, state.clone()));
    state
}

/// Whether the outbox row `msg` is a reply in a thread of any client of this process (message ids are random, so no row
/// of another client can match). The channel's timeline build (`state::messages::build_timeline`, Task 9) should leave
/// such a row out while it is pending; it does not call this yet (the controller adds the call at merge), so until then
/// this area's refresh loop takes the row out of the published timeline itself.
pub fn is_thread_send(msg: &[u8; 16]) -> bool {
    CLIENTS
        .lock()
        .is_ok_and(|c| c.iter().any(|(_, state)| state.has_send(msg)))
}

/// The threads area's handle for threads: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Threads {
    inner: Arc<Inner>,
    state: watch::Receiver<Arc<AppState>>,
    me: Arc<PerClient>,
}

impl Client {
    /// The [`Threads`] handle.
    pub fn threads(&self) -> Threads {
        Threads {
            inner: self.inner().clone(),
            state: self.state(),
            me: per_client(self.inner()),
        }
    }
}

type Done<T> = oneshot::Receiver<Result<T, ClientError>>;

fn sync_err(code: impl Into<String>) -> ClientError {
    ClientError::Sync(code.into())
}

fn decode_err(e: DecodeError) -> ClientError {
    sync_err(format!("E_CORE_DECODE: {e}"))
}

/// The prefs key of a thread's draft.
fn draft_key(root: MessageId) -> String {
    format!("threads.draft.{}", root.hex())
}

impl Threads {
    // ---- reading ----

    /// Keeps `channel`'s thread summaries fresh (the count under a root) and starts this client's refresh loop once. The
    /// UI calls it for the channel it shows; calling it again is free.
    pub fn track(&self, channel: ChannelId) {
        if !self.inner.snapshot().threads.tracked.contains(&channel) {
            self.inner.publish(move |s| {
                s.threads.tracked.insert(channel);
            });
        }
        self.start_loop();
        let (inner, me) = (self.inner.clone(), self.me.clone());
        drop(self.inner.spawn(async move {
            refresh(&inner, &me, Some(channel)).await;
            Ok(())
        }));
    }

    /// Stops keeping `channel`'s summaries (the channel closed).
    pub fn untrack(&self, channel: ChannelId) {
        self.inner.publish(move |s| {
            s.threads.tracked.remove(&channel);
            s.threads.summaries.remove(&channel);
        });
    }

    /// Reads `root`'s thread (the page of the newest [`THREAD_PAGE`] replies) and shows it: the aside opens at once and
    /// fills when the read answers. A second thread in the same client closes the first (the worker keeps one open).
    pub fn open(&self, channel: ChannelId, root: MessageId) -> Done<()> {
        let open = OpenThread { channel, root };
        self.inner.publish(move |s| {
            if let Some(old) = s.threads.open
                && old.root != root
            {
                s.threads.threads.remove(&old.root);
            }
            s.threads.open = Some(open);
            s.threads.last_open = Some(open);
            s.threads.tracked.insert(channel);
        });
        self.start_loop();
        let (inner, me) = (self.inner.clone(), self.me.clone());
        self.inner.spawn(async move {
            // A group this device does not hold (or cannot read) shows an empty thread, whose pane says the root cannot be
            // shown (`thread.rootMissing`), as the worker's `refreshThread` does for a null group.
            let read = read_thread(&inner, &me, open, None).await;
            let failed = read.as_ref().err().cloned();
            let data = read.unwrap_or(ThreadData {
                channel,
                root: None,
                replies: Vec::new(),
                has_earlier: false,
                count: 0,
            });
            inner.publish(move |s| {
                if s.threads.open == Some(open) {
                    s.threads.threads.insert(open.root, data);
                }
            });
            failed.map_or(Ok(()), Err)
        })
    }

    /// Closes the aside and forgets what it read; the thread stays the one [`ThreadState::last_open`] names.
    ///
    /// [`ThreadState::last_open`]: crate::state::threads::ThreadState::last_open
    pub fn close(&self) {
        self.inner.publish(|s| {
            if let Some(open) = s.threads.open.take() {
                s.threads.threads.remove(&open.root);
            }
        });
    }

    /// Reopens the thread the aside showed last (the header's "Threads" button), if any.
    pub fn reopen_last(&self) -> Done<()> {
        match self.inner.snapshot().threads.last_open {
            Some(last) => self.open(last.channel, last.root),
            None => {
                let (tx, rx) = oneshot::channel();
                let _ = tx.send(Ok(()));
                rx
            }
        }
    }

    /// The page of replies before the oldest one read of the open thread, prepended to it.
    pub fn load_earlier(&self) -> Done<()> {
        let (inner, me) = (self.inner.clone(), self.me.clone());
        self.inner.spawn(async move {
            let snap = inner.snapshot();
            let Some(open) = snap.threads.open else {
                return Ok(());
            };
            let Some(current) = snap.threads.threads.get(&open.root).cloned() else {
                return Ok(());
            };
            let Some(oldest) = current.replies.iter().find_map(|r| r.seq) else {
                return Ok(());
            };
            let older = read_thread(&inner, &me, open, Some(oldest)).await?;
            inner.publish(move |s| {
                if s.threads.open != Some(open) {
                    return;
                }
                if let Some(now) = s.threads.threads.get_mut(&open.root) {
                    let mut replies = older.replies;
                    replies.extend(std::mem::take(&mut now.replies));
                    now.replies = replies;
                    now.has_earlier = older.has_earlier;
                }
            });
            Ok(())
        })
    }

    /// Reads the tracked channels' summaries and the open thread again now.
    pub fn refresh(&self) -> Done<()> {
        let (inner, me) = (self.inner.clone(), self.me.clone());
        self.inner.spawn(async move {
            refresh(&inner, &me, None).await;
            Ok(())
        })
    }

    /// Tells the area what the viewer may do in `channel` (the roles area's resolved bits); `None` forgets it (unknown).
    pub fn set_channel_bits(&self, channel: ChannelId, bits: Option<u64>) {
        self.inner.publish(move |s| {
            match bits {
                Some(bits) => s.threads.channel_bits.insert(channel, bits),
                None => s.threads.channel_bits.remove(&channel),
            };
        });
    }

    // ---- replying ----

    /// Aims the composer at `target`: the reply bar shows it, and the next send of that channel is a reply to it.
    pub fn set_replying(&self, target: Option<ReplyTarget>) {
        self.inner.publish(move |s| s.threads.replying = target);
    }

    /// The reply target, and clears it: what a send takes along.
    pub fn take_replying(&self, channel: ChannelId) -> Option<ReplyTarget> {
        let target = self
            .inner
            .snapshot()
            .threads
            .replying
            .clone()
            .filter(|t| t.channel == channel)?;
        self.set_replying(None);
        Some(target)
    }

    // ---- sending ----

    /// Posts `text` in `channel` as a reply to `to`. Answers the message id once the post is in the outbox.
    pub fn reply(&self, channel: ChannelId, to: MessageId, text: String) -> Done<MessageId> {
        self.send(channel, Some(to), None, text)
    }

    /// Posts `text` in the thread of `root`.
    pub fn send_in_thread(
        &self,
        channel: ChannelId,
        root: MessageId,
        text: String,
    ) -> Done<MessageId> {
        self.send(channel, None, Some(root), text)
    }

    /// Edits the message `msg` (a thread reply or any own message) to `text`: a type-1 fold the core resolves.
    pub fn edit(&self, channel: ChannelId, msg: MessageId, text: String) -> Done<()> {
        self.fold(channel, 1, msg, text)
    }

    /// Deletes the message `msg`: a type-2 fold.
    pub fn delete(&self, channel: ChannelId, msg: MessageId) -> Done<()> {
        self.fold(channel, 2, msg, String::new())
    }

    fn send(
        &self,
        channel: ChannelId,
        reply_to: Option<MessageId>,
        thread: Option<MessageId>,
        text: String,
    ) -> Done<MessageId> {
        let (inner, me) = (self.inner.clone(), self.me.clone());
        self.inner.spawn(async move {
            let group = group_of(&inner, channel).await?;
            let request = request_of(0, reply_to, &text, thread);
            let msg = prepare(&inner, group, request).await?;
            if let Some(root) = thread {
                if let Ok(mut sends) = me.sends.lock() {
                    sends.insert(msg, (root.0, group));
                }
                // The pending reply shows in the pane now, not when the drain next publishes.
                refresh(&inner, &me, None).await;
            }
            inner.engine().request_drain(group);
            Ok(MessageId(msg))
        })
    }

    fn fold(&self, channel: ChannelId, kind: u64, msg: MessageId, text: String) -> Done<()> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let group = group_of(&inner, channel).await?;
            prepare(&inner, group, request_of(kind, Some(msg), &text, None)).await?;
            inner.engine().request_drain(group);
            Ok(())
        })
    }

    // ---- drafts ----

    /// The unsent text of `root`'s thread composer (empty when none).
    pub fn draft(&self, root: MessageId) -> String {
        self.inner
            .prefs()
            .get::<String>(&draft_key(root))
            .unwrap_or_default()
    }

    /// Keeps `text` as `root`'s draft; an empty one removes it. A thread's draft never touches the channel's.
    pub fn set_draft(&self, root: MessageId, text: &str) {
        let key = draft_key(root);
        let _ = if text.is_empty() {
            self.inner.prefs().remove(&key)
        } else {
            self.inner.prefs().set(&key, &text)
        };
    }

    // ---- the refresh loop ----

    /// Starts this client's loop once. A published state is an occasion to look again, after a debounce of
    /// [`DEBOUNCE`]: one read of the group list says whether a tracked channel's group moved (a thread reply moves it
    /// without changing any timeline), and only a change in what [`Signature`] holds is read in full, so a publish of
    /// something else (presence, a draft) reads no timeline and no thread. A read that changed nothing publishes nothing,
    /// so the loop settles.
    fn start_loop(&self) {
        if self.me.watching.swap(true, Ordering::AcqRel) {
            return;
        }
        let (weak, me) = (Arc::downgrade(&self.inner), self.me.clone());
        let mut rx = self.state.clone();
        drop(self.inner.spawn(async move {
            while rx.changed().await.is_ok() {
                tokio::time::sleep(DEBOUNCE).await;
                rx.borrow_and_update();
                let Some(inner) = weak.upgrade() else { break };
                let now = signature(&inner, &me).await;
                let changed = me
                    .last
                    .lock()
                    .map_or(true, |last| last.as_ref() != Some(&now));
                if changed {
                    if let Ok(mut last) = me.last.lock() {
                        *last = Some(now);
                    }
                    refresh(&inner, &me, None).await;
                }
            }
            Ok(())
        }));
    }

    /// How many heavy reads (a channel timeline or a thread page) this client has made: what the loop is held to.
    pub fn reads(&self) -> usize {
        self.me.reads.load(Ordering::Acquire)
    }

    /// How many of this client's own thread replies the core has not stored yet.
    pub fn pending_sends(&self) -> usize {
        self.me.sends.lock().map_or(0, |s| s.len())
    }
}

/// How long the loop waits after a published state before it looks.
pub const DEBOUNCE: Duration = Duration::from_millis(250);

/// The cheap look: the group list and the snapshot, no timeline and no thread.
async fn signature(inner: &Arc<Inner>, me: &PerClient) -> Signature {
    let snap = inner.snapshot();
    let groups = inner.engine().groups().await;
    let mut wanted: Vec<ChannelId> = snap.threads.tracked.iter().copied().collect();
    wanted.extend(snap.threads.open.map(|o| o.channel));
    wanted.sort();
    wanted.dedup();
    Signature {
        channels: wanted
            .into_iter()
            .map(|c| {
                let group = groups
                    .iter()
                    .find(|g| g.kind == 0 && g.target_id == c.0 && g.state != 4)
                    .map(|g| (g.next_seq, g.state));
                (
                    c,
                    group,
                    snap.timelines.get(&c).map_or(0, |t| t.items.len()),
                )
            })
            .collect(),
        open: snap.threads.open,
        pending: me.sends.lock().map_or(0, |s| s.len()),
    }
}

// ---- reading the core ----

/// The text group of `channel` as the engine holds it, or the reason there is none.
async fn group_of(inner: &Inner, channel: ChannelId) -> Result<[u8; 16], ClientError> {
    inner
        .engine()
        .groups()
        .await
        .into_iter()
        .find(|g| g.kind == 0 && g.target_id == channel.0 && g.state != 4)
        .map(|g| g.group_id)
        .ok_or(ClientError::UnknownChannel)
}

/// send.ts's request: `[type, reply_to|null, body, [attachments], thread_id|null]`.
fn request_of(
    kind: u64,
    reply_to: Option<MessageId>,
    body: &str,
    thread: Option<MessageId>,
) -> Vec<u8> {
    let id = |m: Option<MessageId>| m.map_or(Value::Null, |m| Value::Bytes(m.0.to_vec()));
    Value::Array(vec![
        Value::Uint(kind),
        id(reply_to),
        Value::Text(body.to_owned()),
        Value::Array(Vec::new()),
        id(thread),
    ])
    .encode()
}

/// `send_prepare` behind the trust retry ([`crate::sync::Engine::send_with_trust_retry`], controller.ts
/// `sendWithTrustRetry`): the post (or fold) goes to the group's outbox; answers its message id. A reply, a thread
/// reply, an edit and a delete right after a peer's new device joined are queued as a post is, never refused for it.
async fn prepare(
    inner: &Inner,
    group: [u8; 16],
    request: Vec<u8>,
) -> Result<[u8; 16], ClientError> {
    inner
        .engine()
        .send_with_trust_retry(group, zeroize::Zeroizing::new(request))
        .await
        .map_err(|e| match e {
            crate::sync::SyncError::Core(e) => sync_err(e.code),
            other => sync_err(other.code()),
        })
}

/// What one read of the open thread answered.
struct ThreadRead {
    root: Option<TimelineRow>,
    /// The root row's reply count (its 19th column).
    count: u32,
    /// Newest first, as the core answers.
    rows: Vec<TimelineRow>,
}

/// core-port.ts `threadTimeline`'s answer: `[root_row|null, [row...]]`, rows in the 19-column timeline shape.
fn decode_thread(raw: &[u8]) -> Result<ThreadRead, DecodeError> {
    let v = value::decode(raw)?;
    let a = arr(&v, 2)?;
    let rows = a[1].as_array().ok_or(DecodeError("thread rows"))?;
    let rows = decode_timeline(&Value::Array(rows.to_vec()).encode())?;
    let (root, count) = match &a[0] {
        Value::Null => (None, 0),
        row => {
            let one = decode_timeline(&Value::Array(vec![row.clone()]).encode())?
                .into_iter()
                .next();
            (one, thread_summary(row)?.map_or(0, |s| s.count))
        }
    };
    Ok(ThreadRead { root, count, rows })
}

/// The 19th column of a timeline row: `[count, last_ts]`, or null (and absent on a core before web3.7).
fn thread_summary(row: &Value) -> Result<Option<ThreadSummary>, DecodeError> {
    let a = at_least(row, 18)?;
    match a.get(18) {
        None => Ok(None),
        Some(v) => opt(v, |s| {
            let s = arr(s, 2)?;
            Ok(ThreadSummary {
                count: u32::try_from(u53(&s[0])?).unwrap_or(u32::MAX),
                last_ts: u53(&s[1])?,
            })
        }),
    }
}

/// The summaries of every root a channel timeline page holds.
fn decode_summaries(raw: &[u8]) -> Result<HashMap<MessageId, ThreadSummary>, DecodeError> {
    let mut out = HashMap::new();
    for row in Rows::of(raw)?.iter() {
        let a = at_least(row, 10)?;
        let (Some(id), Some(summary)) = (opt(&a[9], bin16)?, thread_summary(row)?) else {
            continue;
        };
        out.insert(MessageId(id), summary);
    }
    Ok(out)
}

/// Reads `open`'s thread: the newest [`THREAD_PAGE`] replies, or the page below `before`.
async fn read_thread(
    inner: &Inner,
    client: &PerClient,
    open: OpenThread,
    before: Option<u64>,
) -> Result<ThreadData, ClientError> {
    let group = group_of(inner, open.channel).await?;
    let root = open.root.0;
    let before = before.unwrap_or(0);
    client.reads.fetch_add(1, Ordering::AcqRel);
    let (raw, outbox) = try_core(inner.engine().core(), move |c| {
        (
            c.thread_timeline(&group, &root, before, THREAD_PAGE),
            c.outbox(&group),
        )
    })
    .await
    .map_err(|e| ClientError::Sync(e.to_string()))?;
    let read = decode_thread(&raw.map_err(|e| sync_err(e.code))?).map_err(decode_err)?;
    let outbox = outbox
        .ok()
        .and_then(|raw| decode_outbox(&raw).ok())
        .unwrap_or_default();
    // Only this thread's own pending replies (the outbox row names no thread): folds are never items.
    let pending: Vec<_> = {
        let sends = client.sends.lock().map_err(|_| ClientError::Stopped)?;
        outbox
            .into_iter()
            .filter(|row| sends.get(&row.msg_id) == Some(&(root, group)))
            .collect()
    };
    // Settle what the core stores now (controller.ts `settleThreadSends`): its row is a reply of the page.
    let snap = inner.snapshot();
    let me = (
        snap.account.user_id.map_or([0; 16], |u| u.0),
        snap.account.device_id.map_or([0; 16], |d| d.0),
    );
    let has_earlier = read.rows.len() >= THREAD_PAGE as usize;
    let mut rows: Vec<TimelineRow> = read.rows.into_iter().rev().collect();
    // The root heads the list, so `items[0]` is the root exactly when it is held.
    let root_held = read.root.is_some();
    if let Some(r) = read.root {
        rows.insert(0, r);
    }
    let group_state = snap
        .channel(open.channel)
        .map_or(ChannelGroupState::Active, |c| c.group);
    let timeline = build_timeline(
        open.channel,
        group_state,
        &rows,
        &pending,
        me.0,
        me.1,
        THREAD_PAGE,
    );
    let mut items = timeline.items;
    let root_item = if root_held && !items.is_empty() {
        Some(items.remove(0))
    } else {
        None
    };
    Ok(ThreadData {
        channel: open.channel,
        root: root_item,
        replies: items,
        has_earlier,
        count: read.count,
    })
}

/// Reads the summaries of the tracked channels (and `extra`) and the open thread, and publishes what changed.
async fn refresh(inner: &Arc<Inner>, me: &PerClient, extra: Option<ChannelId>) {
    let snap = inner.snapshot();
    if snap.account.phase != crate::state::AccountPhase::Ready {
        return;
    }
    let mut channels: Vec<ChannelId> = snap.threads.tracked.iter().copied().collect();
    channels.extend(extra);
    channels.extend(snap.threads.open.map(|o| o.channel));
    channels.sort();
    channels.dedup();
    for channel in channels {
        let Ok(group) = group_of(inner, channel).await else {
            continue;
        };
        me.reads.fetch_add(1, Ordering::AcqRel);
        let read = try_core(inner.engine().core(), move |c| {
            (c.timeline(&group, 0, TIMELINE_PAGE), c.outbox(&group))
        })
        .await;
        let Ok((raw, outbox)) = read else { continue };
        if let Some(outbox) = outbox.ok().and_then(|raw| decode_outbox(&raw).ok()) {
            me.settle(group, &outbox.iter().map(|r| r.msg_id).collect());
        }
        let Some(fresh) = raw.ok().and_then(|raw| decode_summaries(&raw).ok()) else {
            continue;
        };
        // A pending thread reply is not a row of the channel: the client's timeline build does not know the outbox row
        // names a thread, so it is taken out here, on the next state after the build put it in.
        if snap.timelines.get(&channel).is_some_and(|t| {
            t.items
                .iter()
                .any(|i| i.msg_id.is_some_and(|m| me.has_send(&m.0)))
        }) {
            inner.publish(move |s| {
                if let Some(t) = s.timelines.get_mut(&channel) {
                    t.items
                        .retain(|i| !i.msg_id.is_some_and(|m| is_thread_send(&m.0)));
                }
            });
        }
        if snap.threads.summaries.get(&channel) != Some(&fresh) {
            inner.publish(move |s| {
                s.threads.summaries.insert(channel, fresh);
            });
        }
    }
    if let Some(open) = snap.threads.open
        && let Ok(newest) = read_thread(inner, me, open, None).await
    {
        let merged = with_earlier(newest, snap.threads.threads.get(&open.root));
        if snap.threads.threads.get(&open.root) != Some(&merged) {
            inner.publish(move |s| {
                if s.threads.open == Some(open) {
                    s.threads.threads.insert(open.root, merged);
                }
            });
        }
    }
}

/// `newest` (the page just read) with the earlier pages `current` already holds in front of it.
fn with_earlier(mut newest: ThreadData, current: Option<&ThreadData>) -> ThreadData {
    let Some(current) = current else {
        return newest;
    };
    let first = newest.replies.iter().find_map(|r| r.seq);
    let mut front: Vec<TimelineItem> = current
        .replies
        .iter()
        .filter(|r| matches!((r.seq, first), (Some(a), Some(b)) if a < b))
        .cloned()
        .collect();
    if !front.is_empty() {
        front.append(&mut newest.replies);
        newest.replies = front;
        newest.has_earlier = current.has_earlier;
    }
    newest
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;

    fn row(seq: u64, msg: u8, thread: Option<(u64, u64)>) -> Value {
        let mut cols = vec![
            Value::Uint(seq),
            Value::Uint(1),
            Value::Uint(100 + seq),
            Value::Uint(0),
            Value::Text(String::new()),
            Value::Bytes(vec![7; 16]),
            Value::Bytes(vec![8; 16]),
            Value::Uint(0),
            Value::Null,
            Value::Bytes(vec![msg; 16]),
            Value::Uint(0),
            Value::Text(format!("body {seq}")),
            Value::Uint(0),
            Value::Null,
            Value::Array(Vec::new()),
            Value::Uint(0),
            Value::Array(Vec::new()),
            Value::Uint(0),
        ];
        cols.push(thread.map_or(Value::Null, |(c, t)| {
            Value::Array(vec![Value::Uint(c), Value::Uint(t)])
        }));
        Value::Array(cols)
    }

    #[test]
    fn summaries_come_from_the_nineteenth_column() {
        let raw = Value::Array(vec![row(1, 1, Some((3, 900))), row(2, 2, None)]).encode();
        let got = decode_summaries(&raw).unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(
            got[&MessageId([1; 16])],
            ThreadSummary {
                count: 3,
                last_ts: 900
            }
        );
    }

    #[test]
    fn a_thread_answer_has_its_root_and_the_roots_count() {
        let raw = Value::Array(vec![
            row(1, 1, Some((2, 50))),
            Value::Array(vec![row(3, 3, None), row(2, 2, None)]),
        ])
        .encode();
        let read = decode_thread(&raw).unwrap();
        assert_eq!(read.root.unwrap().seq, 1);
        assert_eq!(read.count, 2);
        assert_eq!(read.rows.iter().map(|r| r.seq).collect::<Vec<_>>(), [3, 2]);
        // A thread whose root is not held answers null.
        let raw = Value::Array(vec![Value::Null, Value::Array(Vec::new())]).encode();
        let read = decode_thread(&raw).unwrap();
        assert!(read.root.is_none() && read.rows.is_empty());
    }

    #[test]
    fn a_core_before_web3_7_has_no_summary() {
        let mut r = row(1, 1, None);
        let Value::Array(cols) = &mut r else { panic!() };
        cols.pop();
        assert_eq!(thread_summary(&r).unwrap(), None);
    }

    #[test]
    fn the_send_request_carries_the_reply_and_the_thread() {
        let r = value::decode(&request_of(
            0,
            Some(MessageId([1; 16])),
            "hi",
            Some(MessageId([2; 16])),
        ))
        .unwrap();
        let a = arr(&r, 5).unwrap();
        assert_eq!(a[0], Value::Uint(0));
        assert_eq!(a[1], Value::Bytes(vec![1; 16]));
        assert_eq!(a[2], Value::Text("hi".into()));
        assert_eq!(a[4], Value::Bytes(vec![2; 16]));
        let plain = value::decode(&request_of(0, None, "x", None)).unwrap();
        let a = arr(&plain, 5).unwrap();
        assert_eq!((&a[1], &a[4]), (&Value::Null, &Value::Null));
    }

    /// Two clients in one process: one's settling never drops the other's pending sends.
    #[test]
    fn a_clients_thread_sends_are_its_own() {
        let (a, b) = (PerClient::default(), PerClient::default());
        let msg = [0xaa; 16];
        a.sends.lock().unwrap().insert(msg, ([1; 16], [9; 16]));
        b.sends
            .lock()
            .unwrap()
            .insert([0xbb; 16], ([1; 16], [9; 16]));
        assert!(a.has_send(&msg) && !b.has_send(&msg));
        // b's outbox read (the same group id, say) holds neither: it settles its own and leaves a's alone.
        b.settle([9; 16], &HashSet::new());
        assert!(a.has_send(&msg), "a's pending send stays");
        assert!(!b.has_send(&[0xbb; 16]), "b's stored send is forgotten");
        a.settle([9; 16], &HashSet::new());
        assert!(!a.has_send(&msg), "the core stored a's");
    }

    #[test]
    fn a_draft_key_is_per_thread_and_names_no_secret() {
        let k = draft_key(MessageId([0xab; 16]));
        assert_eq!(k, format!("threads.draft.{}", "ab".repeat(16)));
        assert!(!crate::prefs::names_a_secret(&k));
    }

    fn reply(seq: u64) -> TimelineItem {
        let rows =
            decode_timeline(&Value::Array(vec![row(seq, seq as u8, None)]).encode()).unwrap();
        build_timeline(
            ChannelId([1; 16]),
            ChannelGroupState::Active,
            &rows,
            &[],
            [0; 16],
            [0; 16],
            THREAD_PAGE,
        )
        .items
        .remove(0)
    }

    fn data(seqs: &[u64], has_earlier: bool) -> ThreadData {
        ThreadData {
            channel: ChannelId([1; 16]),
            root: None,
            replies: seqs.iter().map(|s| reply(*s)).collect(),
            has_earlier,
            count: seqs.len() as u32,
        }
    }

    /// Paging: the newest page read again keeps the earlier pages the person loaded in front of it.
    #[test]
    fn a_reread_keeps_the_earlier_pages_in_front_of_the_newest() {
        let loaded = data(&[1, 2, 3, 4], false);
        let merged = with_earlier(data(&[3, 4, 5], true), Some(&loaded));
        let seqs: Vec<_> = merged.replies.iter().filter_map(|r| r.seq).collect();
        assert_eq!(seqs, [1, 2, 3, 4, 5]);
        assert!(!merged.has_earlier, "the loaded pages reached the start");
        // Nothing earlier is held: the page stands as read.
        let fresh = with_earlier(data(&[3, 4], true), Some(&data(&[3], true)));
        assert_eq!(fresh.replies.len(), 2);
        assert!(fresh.has_earlier);
        assert_eq!(with_earlier(data(&[9], false), None).replies.len(), 1);
    }
}
