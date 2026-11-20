//! Scheduled announcements (Task 28; W/screens/commands/bot-handlers.ts:31-39, :139-189, :376-470): the Bot keeps a
//! community's schedules and the web reads, saves and deletes them through sealed operations 50, 51 and 52.
//!
//! - [`child_id`]: the request id of one step of a command (`childId`): four FNV-1a lanes over `"{parent}:{label}"`, so a
//!   resend of a step is the same request and the Bot answers what it answered.
//! - [`decode_list`] and [`encode_schedule`]: the list page the Bot answers and the one schedule document a save carries.
//! - [`Bot::list_announcements`], [`Bot::save_announcement`] and [`Bot::delete_announcement`]: the three operations with
//!   the web's answers and copy. A save or a deletion that may or may not have taken effect is an [`Failure`] with
//!   `uncertain` set: the editor closes and a resend of the same `parent` settles it, the Bot answering from its record
//!   of that request (it keeps each request's answers for five minutes and never runs one twice).
//!
//! A schedule's text is a decrypted post: the types holding it give its length only in Debug (R20, R21).

use std::sync::{Arc, Mutex};

use crate::features::bot::Bot;
use crate::features::bot::memory::{AnnouncementAttempt, CommandMemory};
use crate::features::bot_admin::{BotArg, BotOperations, BotRequest, BotResult, OpError};
use crate::gateway::frames::Value;
use crate::state::bot::ResultStatus;
use crate::state::{ChannelId, CommunityId, Redacted, hex};

/// The most a schedule's text may hold, in UTF-8 bytes.
pub const TEXT_MAX_BYTES: usize = 1800;
/// The most schedules the Bot lists, and how many one page holds.
const LIST_MAX: u64 = 128;
const PAGE_MAX: usize = 8;

/// The v1 operations: list, save and delete.
pub const LIST: u64 = 50;
pub const SAVE: u64 = 51;
pub const DELETE: u64 = 52;

/// When a schedule runs (the wire's `"one_off" | "daily" | "weekly"`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ScheduleKind {
    OneOff,
    Daily,
    Weekly,
}

impl ScheduleKind {
    /// The wire's word, which the summary shows too.
    pub fn wire(self) -> &'static str {
        match self {
            Self::OneOff => "one_off",
            Self::Daily => "daily",
            Self::Weekly => "weekly",
        }
    }

    fn of(wire: &str) -> Option<Self> {
        Some(match wire {
            "one_off" => Self::OneOff,
            "daily" => Self::Daily,
            "weekly" => Self::Weekly,
            _ => return None,
        })
    }
}

/// One schedule as the Bot lists it.
#[derive(Clone, PartialEq, Eq)]
pub struct ScheduleRow {
    pub id: [u8; 16],
    pub channel: ChannelId,
    pub text: String,
    pub kind: ScheduleKind,
    pub timezone: String,
    /// `YYYY-MM-DD` for a one-off, else empty.
    pub date: String,
    pub hour: u8,
    pub minute: u8,
    pub weekday: u8,
    pub enabled: bool,
    pub revision: u64,
}

impl std::fmt::Debug for ScheduleRow {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ScheduleRow")
            .field("id", &hex(&self.id))
            .field("channel", &self.channel)
            .field("text", &Redacted(&self.text))
            .field("kind", &self.kind)
            .field("enabled", &self.enabled)
            .field("revision", &self.revision)
            .finish_non_exhaustive()
    }
}

/// What a save carries (the editor's value).
#[derive(Clone, PartialEq, Eq)]
pub struct Schedule {
    pub channel: ChannelId,
    pub text: String,
    pub kind: ScheduleKind,
    pub timezone: String,
    /// `YYYY-MM-DD` for a one-off, else empty.
    pub date: String,
    pub hour: u8,
    pub minute: u8,
    pub weekday: u8,
}

impl std::fmt::Debug for Schedule {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Schedule")
            .field("channel", &self.channel)
            .field("text", &Redacted(&self.text))
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

/// A list page the web would refuse; the words are the web's.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{0}")]
pub struct Invalid(pub &'static str);

/// bot-handlers.ts `childId`: four FNV-1a 32-bit lanes seeded `0x811c9dc5 ^ (seed + 1) * 0x9e3779b1` over the UTF-8 of
/// `"{parent}:{label}"`, as 32 hex digits packed to bytes; all zero becomes `…01`.
pub fn child_id(parent_hex: &str, label: &str) -> [u8; 16] {
    let bytes = format!("{parent_hex}:{label}").into_bytes();
    let mut out = [0u8; 16];
    for seed in 0u32..4 {
        let mut hash = 0x811c_9dc5u32 ^ (seed + 1).wrapping_mul(0x9e37_79b1);
        for byte in &bytes {
            hash ^= u32::from(*byte);
            hash = hash.wrapping_mul(0x0100_0193);
        }
        out[seed as usize * 4..seed as usize * 4 + 4].copy_from_slice(&hash.to_be_bytes());
    }
    if out == [0; 16] {
        out[15] = 1;
    }
    out
}

/// `decodeAnnouncementList` (:139-162): `[1, offset, total <= 128, rows <= 8]`, each row 11 fields. Answers the page's rows
/// and the list's total. A page with no revision is refused.
pub fn decode_list(
    data: &[u8],
    revision: Option<u64>,
    offset: u64,
) -> Result<(Vec<ScheduleRow>, u64), Invalid> {
    revision.ok_or(Invalid("The Bot announcement list has no revision."))?;
    let malformed = Invalid("Malformed announcement list.");
    let Value::Array(page) = Value::decode(data).map_err(|_| malformed)? else {
        return Err(malformed);
    };
    let [
        Value::Uint(1),
        Value::Uint(at),
        Value::Uint(total),
        Value::Array(items),
    ] = page.as_slice()
    else {
        return Err(malformed);
    };
    if *at != offset || *total > LIST_MAX || items.len() > PAGE_MAX {
        return Err(malformed);
    }
    if offset > *total || items.len() as u64 > total - offset {
        return Err(Invalid("Malformed announcement list page."));
    }
    let rows = items.iter().map(row_of).collect::<Result<Vec<_>, _>>()?;
    Ok((rows, *total))
}

fn row_of(item: &Value) -> Result<ScheduleRow, Invalid> {
    let bad = Invalid("Malformed announcement row.");
    let Value::Array(fields) = item else {
        return Err(bad);
    };
    let [
        Value::Bytes(id),
        Value::Bytes(channel),
        Value::Text(text),
        Value::Text(kind),
        Value::Text(timezone),
        Value::Text(date),
        Value::Uint(hour),
        Value::Uint(minute),
        Value::Uint(weekday),
        Value::Uint(enabled),
        Value::Uint(revision),
    ] = fields.as_slice()
    else {
        return Err(bad);
    };
    let id: [u8; 16] = id.as_slice().try_into().map_err(|_| bad)?;
    let channel: [u8; 16] = channel.as_slice().try_into().map_err(|_| bad)?;
    if id == [0; 16]
        || channel == [0; 16]
        || text.trim().is_empty()
        || text.len() > TEXT_MAX_BYTES
        || *hour > 23
        || *minute > 59
        || *weekday > 6
        || *enabled > 1
        || !(1..=i64::MAX as u64).contains(revision)
    {
        return Err(bad);
    }
    let kind = ScheduleKind::of(kind).ok_or(bad)?;
    Ok(ScheduleRow {
        id,
        channel: ChannelId(channel),
        text: text.clone(),
        kind,
        timezone: timezone.clone(),
        date: date.clone(),
        hour: *hour as u8,
        minute: *minute as u8,
        weekday: *weekday as u8,
        enabled: *enabled == 1,
        revision: *revision,
    })
}

/// The schedule document of a save (:437-438): `[1, id, channel, text, kind, timezone, date, hour, minute, weekday,
/// enabled, item revision]`. `date` is sent empty unless the schedule is a one-off.
pub fn encode_schedule(id: [u8; 16], s: &Schedule, enabled: bool, item_revision: u64) -> Vec<u8> {
    let date = if s.kind == ScheduleKind::OneOff {
        s.date.clone()
    } else {
        String::new()
    };
    Value::Array(vec![
        Value::Uint(1),
        Value::Bytes(id.to_vec()),
        Value::Bytes(s.channel.0.to_vec()),
        Value::Text(s.text.clone()),
        Value::Text(s.kind.wire().to_owned()),
        Value::Text(s.timezone.clone()),
        Value::Text(date),
        Value::Uint(u64::from(s.hour)),
        Value::Uint(u64::from(s.minute)),
        Value::Uint(u64::from(s.weekday)),
        Value::Uint(u64::from(enabled)),
        Value::Uint(item_revision),
    ])
    .encode()
}

/// `announcementSummary` (:187-189): `{text} — {kind}, {timezone} {HH}:{MM}`, then ` (disabled)` for a disabled one.
pub fn summary(row: &ScheduleRow) -> String {
    format!(
        "{} — {}, {} {:02}:{:02}{}",
        row.text,
        row.kind.wire(),
        row.timezone,
        row.hour,
        row.minute,
        if row.enabled { "" } else { " (disabled)" }
    )
}

/// `confirmedAnnouncementMutation` (:164-170): a revision past the one the request quoted, and an empty array as its data.
pub fn confirmed(result: &BotResult, previous: u64) -> bool {
    result.revision.is_some_and(|r| r > previous)
        && matches!(Value::decode(&result.data), Ok(Value::Array(a)) if a.is_empty())
}

/// How a save or a deletion ended when it did not. `status` is Failed for a refusal, and Cancelled ("Not confirmed") when
/// the mutation may or may not have taken effect: the same request id then settles it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Failure {
    pub status: ResultStatus,
    pub text: String,
}

impl Failure {
    /// Whether the Bot may have acted (the editor closes and keeps its request id).
    pub fn uncertain(&self) -> bool {
        self.status == ResultStatus::Cancelled
    }

    fn failed(text: &str) -> Self {
        Self {
            status: ResultStatus::Failed,
            text: text.to_owned(),
        }
    }

    fn unsure(text: &str) -> Self {
        Self {
            status: ResultStatus::Cancelled,
            text: text.to_owned(),
        }
    }
}

const PERMISSION: &str = "Manage Community permission is required.";

/// The id of a request for `label`'s step of the command `parent`.
fn request_of(parent: [u8; 16], label: &str) -> [u8; 16] {
    child_id(&hex(&parent), label)
}

/// `listAnnouncements` (:171-186) over `ops`: pages of operation 50 until the list's total, quoting each page's offset in
/// its request id. Answers the rows and the list's revision, which a save or a deletion quotes.
pub async fn list_over(
    ops: &BotOperations,
    parent: [u8; 16],
) -> Result<(Vec<ScheduleRow>, u64), String> {
    let mut rows = Vec::new();
    let (mut offset, mut expected, mut revision) = (0u64, None::<u64>, 0u64);
    loop {
        let result = ops
            .request(BotRequest {
                operation: LIST,
                args: vec![BotArg::Uint(offset)],
                request_id: Some(request_of(parent, &format!("announcement:list:{offset}"))),
                ..BotRequest::default()
            })
            .await
            .map_err(|e| e.to_string())?;
        if result.status != 0 {
            return Err(if result.status == 2 {
                PERMISSION.to_owned()
            } else {
                "The announcement list is unavailable.".to_owned()
            });
        }
        let (page, total) =
            decode_list(&result.data, result.revision, offset).map_err(|e| e.to_string())?;
        let page_revision = result.revision.unwrap_or_default();
        if expected.is_some_and(|t| t != total) || (expected.is_some() && page_revision != revision)
        {
            return Err(
                "The announcement list changed between pages. Refresh it before editing."
                    .to_owned(),
            );
        }
        expected = Some(total);
        revision = page_revision;
        offset += page.len() as u64;
        let empty = page.is_empty();
        rows.extend(page);
        if offset >= total {
            return Ok((rows, revision));
        }
        if empty {
            return Err("The announcement list page made no progress.".to_owned());
        }
    }
}

const MAY_HAVE_BEEN_SENT: &str = "This schedule mutation may have been sent. Its original request and body remain reserved; retry this same request to reconcile it.";

/// Where a command's schedule mutation is remembered (Task 30): the client's command memory, and the community its
/// command runs in. The attempt's key is the command's request id (`parent`) and this community.
#[derive(Clone, Copy)]
pub struct Attempts<'a> {
    pub memory: &'a Mutex<CommandMemory>,
    pub community: CommunityId,
}

impl Attempts<'_> {
    fn with<T>(&self, f: impl FnOnce(&mut CommandMemory) -> T) -> T {
        f(&mut self
            .memory
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner))
    }

    fn get(&self, parent: [u8; 16]) -> Option<AnnouncementAttempt> {
        self.with(|m| m.announcements.get(&(parent, self.community)).cloned())
    }

    fn drop(&self, parent: [u8; 16]) {
        self.with(|m| m.drop_announcement((parent, self.community)));
    }
}

/// Sends `attempt` again as it went (its own request id, arguments and revision): the Bot answers what it did with it and
/// never runs it twice. The web's `api.read` resends what its page sent; here the memory holds what was sent.
async fn resend(ops: &BotOperations, attempt: &AnnouncementAttempt) -> Result<BotResult, OpError> {
    ops.request(BotRequest {
        operation: attempt.operation,
        args: attempt.args.clone(),
        revision: Some(attempt.revision),
        request_id: Some(attempt.request),
        ..BotRequest::default()
    })
    .await
}

/// `runAnnouncementMutation` (bot-handlers.ts:107-137): the `stage` mutation (`operation` with `args` at the list's
/// `revision`) of the command `parent`, over the attempt store. A first run keeps the attempt, marks it sent and sends it;
/// a refusal takes it away. An attempt already sent is reconciled: its own request goes again, whatever this run asked,
/// and the Bot answers what it did. An attempt bound to another operation or revision is not replaced.
async fn mutate(
    ops: &BotOperations,
    attempts: Attempts<'_>,
    parent: [u8; 16],
    stage: &str,
    (operation, args, revision): (u64, Vec<BotArg>, u64),
) -> Result<BotResult, Failure> {
    let held = attempts.get(parent);
    if let Some(a) = &held
        && (a.operation != operation || a.revision != revision)
    {
        if !a.sent || a.operation != operation {
            return Err(Failure::unsure(
                "This announcement attempt is bound to a different schedule revision. Reconcile it before starting another command.",
            ));
        }
        return resend(ops, a)
            .await
            .map_err(|_| Failure::unsure("The prior announcement result is still pending."));
    }
    let mut attempt = held.unwrap_or_else(|| AnnouncementAttempt {
        operation,
        request: request_of(parent, &format!("announcement:{stage}")),
        args,
        revision,
        sent: false,
    });
    if attempt.sent {
        return resend(ops, &attempt).await.map_err(|e| {
            Failure::unsure(&format!(
                "Could not reconcile the prior schedule mutation: {e}"
            ))
        });
    }
    attempt.sent = true;
    attempts.with(|m| m.keep_announcement((parent, attempts.community), attempt.clone()));
    resend(ops, &attempt).await.map_err(|e| match e {
        // Refused before the Bot took it: nothing happened, and the attempt goes (only a `BotOperationRefusedError`
        // deletes it, bot-handlers.ts:132-135).
        OpError::Refused { .. } => {
            attempts.drop(parent);
            Failure::failed(&e.to_string())
        }
        // No answer, an accepted answer that cannot be read (R30), or any other throw: the attempt is sent and stays,
        // as the web's handler answers uncertain for a sent attempt (:452-457).
        OpError::Uncertain { .. } | OpError::Invalid => Failure::unsure(MAY_HAVE_BEEN_SENT),
    })
}

/// A mutation's answer settles its attempt: an accepted (confirmed) one and any refusal take it away; an answer that may
/// or may not have taken effect (a malformed receipt, status 5) keeps it for the resend.
fn settle_attempt(attempts: Attempts<'_>, parent: [u8; 16], answer: &Result<String, Failure>) {
    if answer.as_ref().err().is_none_or(|f| !f.uncertain()) {
        attempts.drop(parent);
    }
}

/// The text a settled save or deletion ends with: `done` when the Bot confirmed it, else the web's words for what it
/// answered; `deleting` picks the deletion's copy.
fn ended(
    result: &BotResult,
    list_revision: u64,
    deleting: bool,
    done: &str,
) -> Result<String, Failure> {
    let (accepted, ambiguous, changed, refused) = if deleting {
        (
            "The Bot accepted deletion, but its revision receipt was malformed. Refresh the list before retrying.",
            "The Bot reported an ambiguous deletion. Retry this same attempt to reconcile it; deletion was not resent.",
            "The schedule changed before deletion. Refresh the announcement list and choose it again.",
            "The announcement could not be deleted.",
        )
    } else {
        (
            "The Bot accepted the schedule, but its revision receipt was malformed. Refresh the list before retrying.",
            "The Bot reported an ambiguous schedule save. Retry this same attempt to reconcile it; the schedule was not resent.",
            "The schedule changed before saving. Refresh the announcement list and try again.",
            "The announcement could not be saved.",
        )
    };
    match result.status {
        0 if confirmed(result, list_revision) => Ok(done.to_owned()),
        0 => Err(Failure::unsure(accepted)),
        5 => Err(Failure::unsure(ambiguous)),
        4 => Err(Failure::failed(changed)),
        2 => Err(Failure::failed(PERMISSION)),
        _ => Err(Failure::failed(refused)),
    }
}

/// `save`'s request over `ops` (:437-461): operation 51 with `[existing id | null, the schedule document]` at the list's
/// revision, under the one request of `parent`. A new schedule's id is derived from `parent` too, so a resend carries the
/// same document.
pub async fn save_over(
    ops: &BotOperations,
    attempts: Attempts<'_>,
    parent: [u8; 16],
    existing: Option<&ScheduleRow>,
    s: &Schedule,
    list_revision: u64,
) -> Result<String, Failure> {
    let id = existing.map_or_else(|| request_of(parent, "announcement:id"), |r| r.id);
    let document = encode_schedule(
        id,
        s,
        existing.is_none_or(|r| r.enabled),
        existing.map_or(1, |r| r.revision + 1),
    );
    let args = vec![
        existing.map_or(BotArg::Null, |r| BotArg::Bytes(r.id.to_vec())),
        BotArg::Bytes(document),
    ];
    let result = mutate(ops, attempts, parent, "save", (SAVE, args, list_revision)).await?;
    let answer = ended(
        &result,
        list_revision,
        false,
        if existing.is_some() {
            "Announcement updated."
        } else {
            "Announcement scheduled."
        },
    );
    settle_attempt(attempts, parent, &answer);
    answer
}

/// The deletion's request over `ops` (:418-427): operation 52 with `[id, the row's revision]` at the list's revision.
pub async fn delete_over(
    ops: &BotOperations,
    attempts: Attempts<'_>,
    parent: [u8; 16],
    row: &ScheduleRow,
    list_revision: u64,
) -> Result<String, Failure> {
    let args = vec![BotArg::Bytes(row.id.to_vec()), BotArg::Uint(row.revision)];
    let result = mutate(
        ops,
        attempts,
        parent,
        "delete",
        (DELETE, args, list_revision),
    )
    .await?;
    let answer = ended(
        &result,
        list_revision,
        true,
        "Scheduled announcement deleted.",
    );
    settle_attempt(attempts, parent, &answer);
    answer
}

/// Reconciles the sent attempt of the command `parent` (bot-handlers.ts:386-400): its own request goes again and the
/// Bot's answer ends it, in the reconcile's words. `None` when no attempt of `parent` was sent (the command runs as a new
/// one).
pub async fn reconcile_over(
    ops: &BotOperations,
    attempts: Attempts<'_>,
    parent: [u8; 16],
) -> Option<Result<String, Failure>> {
    let held = attempts.get(parent).filter(|a| a.sent)?;
    let stage = if held.operation == DELETE {
        "delete"
    } else {
        "save"
    };
    let result = match mutate(
        ops,
        attempts,
        parent,
        stage,
        (held.operation, held.args.clone(), held.revision),
    )
    .await
    {
        Ok(result) => result,
        Err(f) => return Some(Err(f)),
    };
    let answer = match result.status {
        0 if confirmed(&result, held.revision) => Ok(if held.operation == DELETE {
            "Scheduled announcement deleted."
        } else if held.args.first() == Some(&BotArg::Null) {
            "Announcement scheduled."
        } else {
            "Announcement updated."
        }
        .to_owned()),
        0 => Err(Failure::unsure(
            "The Bot accepted the schedule change, but its revision receipt was malformed. Refresh the list before retrying.",
        )),
        5 => Err(Failure::unsure(
            "The Bot reported an ambiguous schedule mutation. Retry this same attempt to reconcile it; the schedule was not resent.",
        )),
        4 => Err(Failure::failed(
            "The schedule changed before saving. Refresh the announcement list and try again.",
        )),
        2 => Err(Failure::failed(PERMISSION)),
        _ => Err(Failure::failed("The announcement could not be saved.")),
    };
    settle_attempt(attempts, parent, &answer);
    Some(answer)
}

/// Whether the command `parent` has a schedule mutation out in `community` that may have taken effect (bot-handlers.ts
/// :382-385: it is reconciled before anything else, and the editor's fields are locked while it is held).
pub fn sent_attempt(
    memory: &Mutex<CommandMemory>,
    parent: [u8; 16],
    community: CommunityId,
) -> bool {
    Attempts { memory, community }
        .get(parent)
        .is_some_and(|a| a.sent)
}

/// Reads the schedules and lands them in `state.bot.announcements`.
async fn read_and_publish(
    inner: &Arc<crate::client::Inner>,
    ops: &BotOperations,
    community: CommunityId,
    parent: [u8; 16],
) -> Result<(Vec<ScheduleRow>, u64), String> {
    let (rows, revision) = list_over(ops, parent).await?;
    let published = rows.clone();
    inner.publish(move |s| {
        s.bot.announcements.insert(community, published);
    });
    Ok((rows, revision))
}

impl Bot {
    /// This community's Bot operations over this client's transport.
    fn announcement_operations(&self, community: CommunityId) -> Arc<BotOperations> {
        Arc::new(BotOperations::new(community, self.transport()))
    }

    /// Reads every scheduled announcement of `community` (operation 50) and lands them in `state.bot.announcements` for
    /// the chips' suggestions. `parent` names the command the pages belong to.
    pub async fn list_announcements(
        &self,
        community: CommunityId,
        parent: [u8; 16],
    ) -> Result<(Vec<ScheduleRow>, u64), String> {
        let ops = self.announcement_operations(community);
        let inner = self.inner.clone();
        match self
            .inner
            .spawn(async move { Ok(read_and_publish(&inner, &ops, community, parent).await) })
            .await
        {
            Ok(Ok(read)) => read,
            _ => Err("The announcement list is unavailable.".to_owned()),
        }
    }

    /// [`Bot::list_announcements`] without waiting: the chips read the schedules once their field is first offered.
    /// `failed` runs if the read did not land.
    pub fn read_announcements(
        &self,
        community: CommunityId,
        parent: [u8; 16],
        failed: impl FnOnce() + Send + 'static,
    ) {
        let ops = self.announcement_operations(community);
        let inner = self.inner.clone();
        drop(self.inner.spawn(async move {
            if read_and_publish(&inner, &ops, community, parent)
                .await
                .is_err()
            {
                failed();
            }
            Ok(())
        }));
    }

    /// Saves `s` as a new announcement, or over `existing` (operation 51); the words of the answer are the web's.
    pub async fn save_announcement(
        &self,
        community: CommunityId,
        parent: [u8; 16],
        existing: Option<&ScheduleRow>,
        s: &Schedule,
        list_revision: u64,
    ) -> Result<String, Failure> {
        let ops = self.announcement_operations(community);
        let memory = self.memory();
        let (existing, s) = (existing.cloned(), s.clone());
        let answer = self
            .run_mutation(async move {
                let attempts = Attempts {
                    memory: &memory,
                    community,
                };
                save_over(&ops, attempts, parent, existing.as_ref(), &s, list_revision).await
            })
            .await;
        self.forget_listed(community, &answer);
        answer
    }

    /// Deletes `row` (operation 52).
    pub async fn delete_announcement(
        &self,
        community: CommunityId,
        parent: [u8; 16],
        row: &ScheduleRow,
        list_revision: u64,
    ) -> Result<String, Failure> {
        let ops = self.announcement_operations(community);
        let memory = self.memory();
        let row = row.clone();
        let answer = self
            .run_mutation(async move {
                let attempts = Attempts {
                    memory: &memory,
                    community,
                };
                delete_over(&ops, attempts, parent, &row, list_revision).await
            })
            .await;
        self.forget_listed(community, &answer);
        answer
    }

    /// Reconciles the sent schedule mutation of the command `parent` in `community` ([`reconcile_over`]); `None` when
    /// there is none.
    pub async fn reconcile_announcement(
        &self,
        community: CommunityId,
        parent: [u8; 16],
    ) -> Option<Result<String, Failure>> {
        if !sent_attempt(&self.memory(), parent, community) {
            return None;
        }
        let ops = self.announcement_operations(community);
        let memory = self.memory();
        let answer = self
            .run_mutation(async move {
                let attempts = Attempts {
                    memory: &memory,
                    community,
                };
                reconcile_over(&ops, attempts, parent)
                    .await
                    .unwrap_or_else(|| Err(Failure::unsure(MAY_HAVE_BEEN_SENT)))
            })
            .await;
        self.forget_listed(community, &answer);
        Some(answer)
    }

    /// Whether the command `parent` holds a sent schedule mutation in `community` ([`sent_attempt`]).
    pub fn announcement_pending(&self, community: CommunityId, parent: [u8; 16]) -> bool {
        sent_attempt(&self.memory(), parent, community)
    }

    /// What the chips hold of `community`'s schedules goes once a save or a deletion may have changed them (it was
    /// accepted, or may have been): the next offer reads them again.
    fn forget_listed(&self, community: CommunityId, answer: &Result<String, Failure>) {
        if answer.as_ref().err().is_some_and(|f| !f.uncertain()) {
            return;
        }
        self.inner.publish(move |s| {
            s.bot.announcements.remove(&community);
        });
    }

    /// Runs `work` on the client's runtime. A runtime that stopped leaves the mutation unsure: it may have been sent.
    async fn run_mutation(
        &self,
        work: impl std::future::Future<Output = Result<String, Failure>> + Send + 'static,
    ) -> Result<String, Failure> {
        match self.inner.spawn(async move { Ok(work.await) }).await {
            Ok(Ok(answer)) => answer,
            _ => Err(Failure::unsure(
                "This schedule mutation may have been sent. Its original request and body remain reserved; retry this same request to reconcile it.",
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::features::bot_admin::{SealedAnswer, SealedBot, SealedCommand, SendError};
    use std::collections::VecDeque;
    use std::sync::Mutex;

    const COMMUNITY: CommunityId = CommunityId([9; 16]);

    fn row_value(id: u8, text: &str) -> Value {
        Value::Array(vec![
            Value::Bytes(vec![id; 16]),
            Value::Bytes(vec![2; 16]),
            Value::Text(text.into()),
            Value::Text("daily".into()),
            Value::Text("UTC".into()),
            Value::Text(String::new()),
            Value::Uint(9),
            Value::Uint(0),
            Value::Uint(3),
            Value::Uint(1),
            Value::Uint(4),
        ])
    }

    fn page(offset: u64, total: u64, rows: Vec<Value>) -> Vec<u8> {
        Value::Array(vec![
            Value::Uint(1),
            Value::Uint(offset),
            Value::Uint(total),
            Value::Array(rows),
        ])
        .encode()
    }

    /// A sealed answer of status 200: `[revision, data]`.
    fn answer(revision: Option<u64>, data: Vec<u8>) -> SealedAnswer {
        SealedAnswer {
            status: 200,
            body: Value::Array(vec![
                revision.map_or(Value::Null, Value::Uint),
                Value::Bytes(data),
            ])
            .encode(),
        }
    }

    /// The Bot refusing with `code` (a non-200 status).
    fn refusal(code: &str) -> SealedAnswer {
        SealedAnswer {
            status: 409,
            body: code.as_bytes().to_vec(),
        }
    }

    /// A transport that answers in order and records each command it was sent.
    struct Script {
        answers: Mutex<VecDeque<Result<SealedAnswer, SendError>>>,
        sent: Mutex<Vec<SealedCommand>>,
    }

    impl Script {
        fn new(answers: Vec<Result<SealedAnswer, SendError>>) -> Arc<Self> {
            Arc::new(Self {
                answers: Mutex::new(answers.into()),
                sent: Mutex::default(),
            })
        }
    }

    #[async_trait::async_trait]
    impl SealedBot for Script {
        async fn send(
            &self,
            _: CommunityId,
            command: SealedCommand,
        ) -> Result<SealedAnswer, SendError> {
            self.sent.lock().unwrap().push(command);
            self.answers
                .lock()
                .unwrap()
                .pop_front()
                .expect("an answer was scripted")
        }
    }

    fn run<T>(f: impl std::future::Future<Output = T>) -> T {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(f)
    }

    fn schedule() -> Schedule {
        Schedule {
            channel: ChannelId([2; 16]),
            text: "stand-up".into(),
            kind: ScheduleKind::Daily,
            timezone: "UTC".into(),
            date: "2026-12-01".into(),
            hour: 9,
            minute: 0,
            weekday: 3,
        }
    }

    fn attempts(memory: &Mutex<CommandMemory>) -> Attempts<'_> {
        Attempts {
            memory,
            community: COMMUNITY,
        }
    }

    /// Task 30 (bot-handlers.ts:107-137, :386-400): a sent attempt is reconciled by its own request, in the reconcile's
    /// words; an attempt bound to another revision is not replaced; a refusal takes the attempt away.
    #[test]
    fn a_sent_attempt_is_reconciled_by_its_own_request() {
        let empty = Value::Array(vec![]).encode();
        let memory = Mutex::default();
        let none = Script::new(vec![]);
        let ops = BotOperations::new(COMMUNITY, none.clone());
        assert_eq!(run(reconcile_over(&ops, attempts(&memory), [4; 16])), None);
        // An ambiguous deletion is held, sent.
        let script = Script::new(vec![
            Ok(refusal("E_UNAVAILABLE")),
            Ok(refusal("E_UNAVAILABLE")),
            Ok(answer(Some(8), vec![0xff])),
            Ok(answer(Some(8), empty)),
        ]);
        let ops = BotOperations::new(COMMUNITY, script.clone());
        let row = listed();
        assert!(
            run(delete_over(&ops, attempts(&memory), [4; 16], &row, 7))
                .unwrap_err()
                .uncertain()
        );
        // A save of the same command at another revision is refused before anything is sent.
        let other = run(save_over(
            &ops,
            attempts(&memory),
            [4; 16],
            None,
            &schedule(),
            9,
        ))
        .unwrap_err();
        assert!(other.uncertain());
        assert_eq!(
            other.text,
            "This announcement attempt is bound to a different schedule revision. Reconcile it before starting another command."
        );
        let reconcile = || run(reconcile_over(&ops, attempts(&memory), [4; 16])).unwrap();
        let ambiguous = reconcile().unwrap_err();
        assert!(ambiguous.uncertain());
        assert!(
            ambiguous
                .text
                .starts_with("The Bot reported an ambiguous schedule mutation.")
        );
        let malformed = reconcile().unwrap_err();
        assert!(
            malformed
                .text
                .starts_with("The Bot accepted the schedule change, but its revision receipt")
        );
        assert_eq!(
            reconcile(),
            Ok("Scheduled announcement deleted.".to_owned())
        );
        assert!(!sent_attempt(&memory, [4; 16], COMMUNITY));
        let sent = script.sent.lock().unwrap();
        assert_eq!(sent.len(), 4, "the refused save sent nothing");
        assert!(sent.iter().all(|c| c.request_id == sent[0].request_id));
        assert!(sent.iter().all(|c| c.args == sent[0].args));
        drop(sent);
        // A refusal on the first send takes the attempt away.
        let refused = Script::new(vec![Ok(refusal("E_CONFLICT"))]);
        let ops = BotOperations::new(COMMUNITY, refused);
        run(delete_over(&ops, attempts(&memory), [5; 16], &row, 7)).unwrap_err();
        assert!(!sent_attempt(&memory, [5; 16], COMMUNITY));
    }

    /// Task 30 fix round 1 (R30): a first save the Bot accepted with an answer that cannot be read is uncertain: the
    /// attempt stays sent, and the next run of the same command resends it under the same request instead of a new one.
    #[test]
    fn an_unreadable_first_answer_keeps_the_attempt() {
        let empty = Value::Array(vec![]).encode();
        let script = Script::new(vec![
            Ok(SealedAnswer {
                status: 200,
                body: vec![0xff],
            }),
            Ok(answer(Some(8), empty)),
        ]);
        let ops = BotOperations::new(COMMUNITY, script.clone());
        let memory = Mutex::default();
        let first = run(save_over(
            &ops,
            attempts(&memory),
            [4; 16],
            None,
            &schedule(),
            7,
        ))
        .unwrap_err();
        assert!(first.uncertain(), "{first:?}");
        assert!(
            first
                .text
                .starts_with("This schedule mutation may have been sent.")
        );
        assert!(sent_attempt(&memory, [4; 16], COMMUNITY), "kept, sent");
        assert_eq!(
            run(reconcile_over(&ops, attempts(&memory), [4; 16])),
            Some(Ok("Announcement scheduled.".to_owned()))
        );
        let sent = script.sent.lock().unwrap();
        assert_eq!(sent[0].request_id, sent[1].request_id);
        assert_eq!(sent[0].args, sent[1].args);
    }

    fn listed() -> ScheduleRow {
        let (mut rows, _) =
            decode_list(&page(0, 1, vec![row_value(1, "stand-up")]), Some(7), 0).unwrap();
        rows.remove(0)
    }

    /// bot-handlers.ts `childId`: the same ids as the web's function (the vectors were computed with it).
    #[test]
    fn child_id_matches_the_web() {
        let parent = "00112233445566778899aabbccddeeff";
        assert_eq!(
            hex(&child_id(parent, "announcement:save")),
            "0c063b0494e2958b2059b29ef049dae5"
        );
        assert_eq!(
            hex(&child_id(parent, "announcement:list:0")),
            "196ce6cfc39bc650f796a7e9b537d02a"
        );
    }

    /// :139-162: a page the web refuses is refused here; one valid row is read.
    #[test]
    fn decode_list_refuses_what_the_web_refuses() {
        let ok = || row_value(1, "stand-up");
        let with = |mutate: &dyn Fn(&mut Vec<Value>)| {
            let Value::Array(mut fields) = ok() else {
                unreachable!()
            };
            mutate(&mut fields);
            Value::Array(fields)
        };
        let bad = |data: Vec<u8>, revision: Option<u64>, offset: u64| {
            assert!(decode_list(&data, revision, offset).is_err());
        };
        bad(page(1, 1, vec![ok()]), Some(1), 0);
        bad(page(0, 129, vec![ok()]), Some(1), 0);
        bad(
            page(0, 20, (1..=9).map(|i| row_value(i, "x")).collect()),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[6] = Value::Uint(24))]),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[2] = Value::Text("  ".into()))]),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[0] = Value::Bytes(vec![0; 16]))]),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[3] = Value::Text("hourly".into()))]),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[10] = Value::Uint(0))]),
            Some(1),
            0,
        );
        bad(
            page(0, 1, vec![with(&|f| f[2] = Value::Text("é".repeat(901)))]),
            Some(1),
            0,
        );
        bad(page(0, 1, vec![ok(), ok()]), Some(1), 0);
        bad(page(0, 1, vec![ok()]), None, 0);
        let (rows, total) = decode_list(&page(0, 1, vec![ok()]), Some(5), 0).unwrap();
        assert_eq!((rows.len(), total), (1, 1));
        assert_eq!(rows[0].hour, 9);
        assert_eq!(rows[0].revision, 4);
        assert_eq!(summary(&rows[0]), "stand-up — daily, UTC 09:00");
        let mut off = rows[0].clone();
        off.enabled = false;
        assert_eq!(summary(&off), "stand-up — daily, UTC 09:00 (disabled)");
    }

    /// :437-438: the document of a save is the web's array of twelve; a daily one carries no date.
    #[test]
    fn encode_schedule_is_the_web_array() {
        let bytes = encode_schedule([5; 16], &schedule(), true, 3);
        let Value::Array(fields) = Value::decode(&bytes).unwrap() else {
            panic!("an array");
        };
        assert_eq!(fields.len(), 12);
        assert_eq!(fields[0], Value::Uint(1));
        assert_eq!(fields[1], Value::Bytes(vec![5; 16]));
        assert_eq!(fields[3], Value::Text("stand-up".into()));
        assert_eq!(fields[4], Value::Text("daily".into()));
        assert_eq!(
            fields[6],
            Value::Text(String::new()),
            "no date unless one-off"
        );
        assert_eq!(fields[10], Value::Uint(1));
        assert_eq!(fields[11], Value::Uint(3));
        let mut once = schedule();
        once.kind = ScheduleKind::OneOff;
        let Value::Array(fields) =
            Value::decode(&encode_schedule([5; 16], &once, false, 1)).unwrap()
        else {
            panic!("an array");
        };
        assert_eq!(fields[6], Value::Text("2026-12-01".into()));
        assert_eq!(fields[10], Value::Uint(0));
    }

    /// :164-170: confirmed only with a newer revision and an empty array.
    #[test]
    fn a_receipt_is_confirmed_by_a_newer_revision_and_an_empty_array() {
        let result = |revision: Option<u64>, data: Vec<u8>| BotResult {
            request_id: [1; 16],
            status: 0,
            revision,
            data,
        };
        let empty = Value::Array(vec![]).encode();
        assert!(confirmed(&result(Some(8), empty.clone()), 7));
        assert!(!confirmed(&result(Some(7), empty.clone()), 7));
        assert!(!confirmed(&result(None, empty), 7));
        assert!(!confirmed(&result(Some(8), vec![0xff]), 7));
        assert!(!confirmed(
            &result(Some(8), Value::Array(vec![Value::Uint(1)]).encode()),
            7
        ));
    }

    /// :447-461: an ambiguous save (status 5) is not resent as a new one; the second call sends the same request again
    /// and the Bot's answer settles it.
    #[test]
    fn save_reconciles_with_the_same_request() {
        let empty = Value::Array(vec![]).encode();
        let script = Script::new(vec![
            Ok(refusal("E_UNAVAILABLE")),
            Ok(answer(Some(8), empty)),
        ]);
        let ops = BotOperations::new(COMMUNITY, script.clone());
        let memory = Mutex::default();
        let first = run(save_over(
            &ops,
            attempts(&memory),
            [4; 16],
            None,
            &schedule(),
            7,
        ))
        .expect_err("ambiguous");
        assert!(first.uncertain());
        assert!(
            first
                .text
                .starts_with("The Bot reported an ambiguous schedule save.")
        );
        assert!(sent_attempt(&memory, [4; 16], COMMUNITY), "kept, sent");
        // Task 30: the resend carries the stored attempt, whatever this run's schedule says.
        let mut changed = schedule();
        changed.text = "changed".into();
        let second = run(save_over(
            &ops,
            attempts(&memory),
            [4; 16],
            None,
            &changed,
            7,
        ));
        assert_eq!(second, Ok("Announcement scheduled.".to_owned()));
        assert!(!sent_attempt(&memory, [4; 16], COMMUNITY), "settled");
        let sent = script.sent.lock().unwrap();
        assert_eq!(sent.len(), 2);
        assert_eq!(sent[0].request_id, sent[1].request_id);
        assert_eq!(
            sent[0].request_id,
            child_id(&hex(&[4; 16]), "announcement:save")
        );
        assert_eq!(sent[0].operation, 151);
        assert_eq!(
            sent[0].args[0],
            BotArg::Uint(7),
            "the list's revision first"
        );
        assert_eq!(sent[0].args[1], BotArg::Null, "a new schedule has no id");
        assert_eq!(sent[0].args, sent[1].args, "the same body");
    }

    /// :447-461 and :418-427: each answer's copy.
    #[test]
    fn the_answers_read_as_the_web_says() {
        let empty = Value::Array(vec![]).encode();
        let save = |answers: Vec<Result<SealedAnswer, SendError>>,
                    existing: Option<&ScheduleRow>| {
            let ops = BotOperations::new(COMMUNITY, Script::new(answers));
            let memory = Mutex::default();
            run(save_over(
                &ops,
                attempts(&memory),
                [4; 16],
                existing,
                &schedule(),
                7,
            ))
        };
        assert_eq!(
            save(vec![Ok(answer(Some(8), empty.clone()))], Some(&listed())),
            Ok("Announcement updated.".to_owned())
        );
        let unsure = |r: Result<String, Failure>| {
            let f = r.expect_err("not done");
            assert!(f.uncertain(), "{f:?}");
            f.text
        };
        assert_eq!(
            unsure(save(vec![Ok(answer(Some(8), vec![0xff]))], None)),
            "The Bot accepted the schedule, but its revision receipt was malformed. Refresh the list before retrying."
        );
        assert!(
            unsure(save(
                vec![Err(SendError::Timeout), Err(SendError::Timeout)],
                None
            ))
            .starts_with("This schedule mutation may have been sent.")
        );
        let failed = |code: &str| {
            let f = save(vec![Ok(refusal(code))], None).expect_err("refused");
            assert_eq!(f.status, ResultStatus::Failed);
            f.text
        };
        assert_eq!(
            failed("E_CONFLICT"),
            "The schedule changed before saving. Refresh the announcement list and try again."
        );
        assert_eq!(
            failed("E_FORBIDDEN"),
            "Manage Community permission is required."
        );
        assert_eq!(failed("E_INTERNAL"), "The announcement could not be saved.");

        let row = listed();
        let delete = |answers: Vec<Result<SealedAnswer, SendError>>| {
            let script = Script::new(answers);
            let ops = BotOperations::new(COMMUNITY, script.clone());
            let memory = Mutex::default();
            let r = run(delete_over(&ops, attempts(&memory), [4; 16], &row, 7));
            (r, script.sent.lock().unwrap().clone())
        };
        let (done, sent) = delete(vec![Ok(answer(Some(8), empty.clone()))]);
        assert_eq!(done, Ok("Scheduled announcement deleted.".to_owned()));
        assert_eq!(sent[0].operation, 152);
        assert_eq!(
            sent[0].args,
            [BotArg::Uint(7), BotArg::Bytes(vec![1; 16]), BotArg::Uint(4)],
            "the list's revision, the id, the row's revision"
        );
        assert_eq!(
            delete(vec![Ok(refusal("E_UNAVAILABLE"))])
                .0
                .unwrap_err()
                .text,
            "The Bot reported an ambiguous deletion. Retry this same attempt to reconcile it; deletion was not resent."
        );
        assert_eq!(
            delete(vec![Ok(refusal("E_CONFLICT"))]).0.unwrap_err().text,
            "The schedule changed before deletion. Refresh the announcement list and choose it again."
        );
        assert_eq!(
            delete(vec![Ok(refusal("E_INTERNAL"))]).0.unwrap_err().text,
            "The announcement could not be deleted."
        );
    }

    /// :171-186: the pages are read until the total, each under its own request; a list that moves between pages, or
    /// that stalls, is refused; a refusal reads as the web says.
    #[test]
    fn the_list_is_read_page_by_page() {
        let rows = |from: u8, n: u8| {
            (from..from + n)
                .map(|i| row_value(i, "x"))
                .collect::<Vec<_>>()
        };
        let script = Script::new(vec![
            Ok(answer(Some(3), page(0, 9, rows(1, 8)))),
            Ok(answer(Some(3), page(8, 9, rows(9, 1)))),
        ]);
        let ops = BotOperations::new(COMMUNITY, script.clone());
        let (all, revision) = run(list_over(&ops, [4; 16])).unwrap();
        assert_eq!((all.len(), revision), (9, 3));
        let sent = script.sent.lock().unwrap();
        assert_eq!(sent[0].args, [BotArg::Uint(0)]);
        assert_eq!(sent[1].args, [BotArg::Uint(8)]);
        assert_eq!(
            sent[1].request_id,
            child_id(&hex(&[4; 16]), "announcement:list:8")
        );
        drop(sent);
        let read = |answers| {
            let ops = BotOperations::new(COMMUNITY, Script::new(answers));
            run(list_over(&ops, [4; 16])).unwrap_err()
        };
        assert_eq!(
            read(vec![
                Ok(answer(Some(3), page(0, 9, rows(1, 8)))),
                Ok(answer(Some(4), page(8, 9, rows(9, 1)))),
            ]),
            "The announcement list changed between pages. Refresh it before editing."
        );
        assert_eq!(
            read(vec![
                Ok(answer(Some(3), page(0, 9, rows(1, 8)))),
                Ok(answer(Some(3), page(8, 9, vec![]))),
            ]),
            "The announcement list page made no progress."
        );
        assert_eq!(
            read(vec![Ok(refusal("E_FORBIDDEN"))]),
            "Manage Community permission is required."
        );
        assert_eq!(
            read(vec![Ok(refusal("E_INTERNAL"))]),
            "The announcement list is unavailable."
        );
    }

    /// R20/R21: a schedule's text is decrypted content; Debug gives its length only.
    #[test]
    fn a_schedule_keeps_its_text_out_of_debug() {
        let mut s = schedule();
        s.text = "the secret plan".into();
        let mut r = listed();
        r.text = "the secret plan".into();
        for shown in [
            format!("{s:?}"),
            format!("{s:#?}"),
            format!("{r:?}"),
            format!("{r:#?}"),
        ] {
            assert!(!shown.contains("secret"), "{shown}");
        }
        assert!(format!("{r:?}").contains("<15 bytes>"));
    }
}
