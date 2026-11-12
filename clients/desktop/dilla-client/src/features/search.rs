//! Area **search** (Task 23): local search over the messages this device holds.
//!
//! Ports: packages/client-core/src/state/search.ts (the hit decoding and its bounds), the worker's `search` command
//! (packages/client-core/src/worker/controller.ts) and core `search.rs`.
//!
//! The query never leaves the device: the core searches its own store (`ClientCore::search`, scope `all`, over the
//! displayable folded text), the hits are resolved against the channels this client lists, and the answer is published
//! into [`SearchState`]. Nothing is sent to the instance, as in the web client. A request older than the newest one never
//! publishes ([`SearchState::request`], the web's `searchGate`).

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use dilla_core::client::ClientCore;
use tokio::sync::oneshot;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::state::search::{SearchHit, SearchState};
use crate::state::{AppState, ChannelId, CommunityId, MessageId, TimelineItemState, UserId};
use crate::sync::api::try_core;
use crate::sync::value::{DecodeError, Rows, at_least, bin16, decode, opt, text, uint};

/// A page of hits: the core's limit and the one `decodeSearchHits` enforces.
pub const MAX_HITS: usize = 50;
/// A snippet's length in characters (`decodeSearchHits`).
pub const MAX_SNIPPET_CHARS: usize = 160;
/// The query's length in bytes once trimmed (`validateSearch`).
pub const MAX_QUERY_BYTES: usize = 128;

/// The newest request number, process-wide: a request is newer than every earlier one of any client.
static NEXT_REQUEST: AtomicU64 = AtomicU64::new(1);

/// The search area's handle for search: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Search {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Search`] handle.
    pub fn search(&self) -> Search {
        Search {
            inner: self.inner().clone(),
        }
    }
}

/// A hit as the core answers it, before it is resolved to a channel.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RawHit {
    pub group: [u8; 16],
    pub seq: u64,
    pub snippet: String,
    pub thread_root: Option<[u8; 16]>,
}

/// `decodeSearchHits`: `[[group, seq, snippet, rank, thread_root?]]`, at most 50 rows; rank is 0 and a snippet at most 160
/// characters. Four elements from a core before web3.7; a fifth names the thread root; six is malformed.
pub fn decode_hits(raw: &[u8]) -> Result<Vec<RawHit>, DecodeError> {
    let rows = Rows::of(raw)?;
    if rows.iter().len() > MAX_HITS {
        return Err(DecodeError("hit limit"));
    }
    rows.iter()
        .map(|row| {
            let a = at_least(row, 4)?;
            if a.len() > 5 {
                return Err(DecodeError("hit length"));
            }
            let snippet = text(&a[2])?;
            if snippet.chars().count() > MAX_SNIPPET_CHARS {
                return Err(DecodeError("snippet limit"));
            }
            if uint(&a[3])? != 0 {
                return Err(DecodeError("rank"));
            }
            Ok(RawHit {
                group: bin16(&a[0])?,
                seq: uint(&a[1])?,
                snippet,
                thread_root: match a.get(4) {
                    Some(v) => opt(v, bin16)?,
                    None => None,
                },
            })
        })
        .collect()
}

/// What the query must be to be searched at all (`validateSearch`): 1 to 128 UTF-8 bytes once trimmed.
pub fn searchable(query: &str) -> bool {
    let n = query.trim().len();
    (1..=MAX_QUERY_BYTES).contains(&n)
}

/// A user's name as the palette shows it (`nameOf`): the display name, else the username, from any community's member
/// list.
pub fn name_of(state: &AppState, user: UserId) -> Option<String> {
    state
        .members
        .values()
        .flatten()
        .find(|m| m.user_id == user)
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        })
}

/// A row's `(seq, recv_ts, sender)` from a core timeline row (`[seq, _, recv_ts, status, reason, sender, ...]`).
fn row_facts(row: &Value) -> Option<(u64, u64, Option<[u8; 16]>)> {
    let a = at_least(row, 6).ok()?;
    Some((
        uint(&a[0]).ok()?,
        uint(&a[2]).ok()?,
        opt(&a[5], bin16).ok().flatten(),
    ))
}

/// The `(recv_ts, sender)` of the row at `seq` of `group`'s main timeline, or (for a reply) of the thread under `root`
/// (the worker's `rowAt`).
fn row_at(
    core: &mut ClientCore,
    group: [u8; 16],
    seq: u64,
    root: Option<[u8; 16]>,
) -> Option<(u64, Option<[u8; 16]>)> {
    let find = |rows: &[Value]| {
        rows.iter()
            .filter_map(row_facts)
            .find(|(s, _, _)| *s == seq)
            .map(|(_, ts, who)| (ts, who))
    };
    match root {
        None => {
            let raw = core.timeline(&group, seq + 1, 1).ok()?;
            let rows = Rows::of(&raw).ok()?;
            find(&rows.iter().cloned().collect::<Vec<_>>())
        }
        Some(root) => {
            let raw = core.thread_timeline(&group, &root, seq + 1, 1).ok()?;
            let v = decode(&raw).ok()?;
            let parts = at_least(&v, 2).ok()?;
            find(parts[1].as_array()?)
        }
    }
}

impl Search {
    /// Searches the messages this device holds for `query` (scope everywhere; two or more characters is the palette's
    /// own gate) and publishes the hits. The answer arrives once the request is published or dropped as stale.
    pub fn query(&self, query: &str) -> oneshot::Receiver<Result<(), ClientError>> {
        let query = query.trim().to_owned();
        let request = NEXT_REQUEST.fetch_add(1, Ordering::SeqCst);
        let shown = query.clone();
        self.inner.publish(move |s| {
            s.search = SearchState {
                request,
                query: shown,
                hits: Vec::new(),
                loading: true,
            };
        });
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let hits = if searchable(&query) {
                find(&inner, &query).await?
            } else {
                Vec::new()
            };
            inner.publish(move |s| {
                if s.search.request == request {
                    s.search.hits = hits;
                    s.search.loading = false;
                }
            });
            Ok(())
        })
    }

    /// Forgets the newest search (the palette closed): its query and hits are dropped, and an answer still on its way is
    /// ignored.
    pub fn clear(&self) {
        let request = NEXT_REQUEST.fetch_add(1, Ordering::SeqCst);
        self.inner.publish(move |s| {
            s.search = SearchState {
                request,
                ..SearchState::default()
            };
        });
    }
}

/// Runs the core's search and resolves its hits.
async fn find(inner: &Arc<Inner>, query: &str) -> Result<Vec<SearchHit>, ClientError> {
    let core = inner.engine().core().clone();
    let q = query.to_owned();
    let raw = try_core(&core, move |c| c.search(2, None, &q, None, MAX_HITS as u32))
        .await
        .map_err(|e| ClientError::Sync(e.to_string()))?;
    // A query the core refuses (more than 8 terms, no letters or digits) finds nothing, as the web page shows "nothing
    // matches".
    let Ok(raw) = raw else {
        return Ok(Vec::new());
    };
    let raw_hits = decode_hits(&raw).map_err(|e| ClientError::Sync(e.to_string()))?;
    let groups: HashMap<[u8; 16], _> = inner
        .engine()
        .groups()
        .await
        .into_iter()
        .filter(|g| g.kind == 0)
        .map(|g| (g.group_id, g))
        .collect();
    let snapshot = inner.snapshot();
    // Where each hit goes: a channel this client lists, or a direct message (a group with no community).
    let mut placed = Vec::new();
    for h in raw_hits {
        let Some(g) = groups.get(&h.group) else {
            continue;
        };
        let channel = ChannelId(g.target_id);
        let community = match g.community_id {
            Some(c) => {
                let known = snapshot
                    .channel(channel)
                    .is_some_and(|ch| ch.community_id == CommunityId(c));
                if !known {
                    continue;
                }
                Some(CommunityId(c))
            }
            None => None,
        };
        placed.push((h, channel, community));
    }
    let wanted: Vec<([u8; 16], u64, Option<[u8; 16]>)> = placed
        .iter()
        .map(|(h, _, _)| (h.group, h.seq, h.thread_root))
        .collect();
    let rows = try_core(&core, move |c| {
        wanted
            .into_iter()
            .map(|(g, seq, root)| row_at(c, g, seq, root))
            .collect::<Vec<_>>()
    })
    .await
    .map_err(|e| ClientError::Sync(e.to_string()))?;
    Ok(placed
        .into_iter()
        .zip(rows)
        .map(|((h, channel, community), row)| SearchHit {
            channel,
            community,
            seq: h.seq,
            snippet: h.snippet,
            author: row
                .and_then(|(_, who)| who)
                .and_then(|u| name_of(&snapshot, UserId(u))),
            ts: row.map(|(ts, _)| ts),
            thread_root: h.thread_root.map(MessageId),
        })
        .collect())
}

/// A character as the core's search folds it: lower case, accents removed (Latin text; the core's own table is longer,
/// and only the core searches a real store).
fn fold_char(c: char) -> char {
    match c {
        'à'..='å' | 'ā' | 'ă' | 'ą' => 'a',
        'ç' | 'ć' | 'ĉ' | 'ċ' | 'č' => 'c',
        'ď' => 'd',
        'è'..='ë' | 'ē' | 'ĕ' | 'ė' | 'ę' | 'ě' => 'e',
        'ĝ' | 'ğ' | 'ġ' | 'ģ' => 'g',
        'ì'..='ï' | 'ĩ' | 'ī' | 'ĭ' | 'į' => 'i',
        'ñ' | 'ń' | 'ņ' | 'ň' => 'n',
        'ò'..='ö' | 'ō' | 'ŏ' | 'ő' => 'o',
        'ŕ' | 'ŗ' | 'ř' => 'r',
        'ś' | 'ŝ' | 'ş' | 'š' => 's',
        'ţ' | 'ť' => 't',
        'ù'..='ü' | 'ũ' | 'ū' | 'ŭ' | 'ů' | 'ű' | 'ų' => 'u',
        'ý' | 'ÿ' | 'ŷ' => 'y',
        'ź' | 'ż' | 'ž' => 'z',
        other => other,
    }
}

/// The search terms of `input`: lower case, folded, split at everything that is not a letter or a digit (core `tokenize`).
pub fn terms(input: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut term = String::new();
    for c in input.chars().flat_map(char::to_lowercase) {
        let c = fold_char(c);
        if c.is_alphanumeric() {
            term.push(c);
        } else if !term.is_empty() {
            out.push(std::mem::take(&mut term));
        }
    }
    if !term.is_empty() {
        out.push(term);
    }
    out
}

/// The hits of `query` over the messages `state` holds, with the core's matching (every query term is the prefix of a term
/// of the message), for a state with no client behind it (a screen fixture). Conversations by descending id, newest message
/// first, as the core orders its groups. A live client never calls this: it asks the core ([`Search::query`]).
pub fn local_hits(state: &AppState, query: &str) -> Vec<SearchHit> {
    if !searchable(query) {
        return Vec::new();
    }
    let wanted = terms(query);
    if wanted.is_empty() || wanted.len() > 8 {
        return Vec::new();
    }
    let mut channels: Vec<_> = state.timelines.iter().collect();
    channels.sort_by(|a, b| b.0.cmp(a.0));
    let mut hits = Vec::new();
    for (channel, timeline) in channels {
        let community = state.channel(*channel).map(|c| c.community_id);
        for (index, item) in timeline.items.iter().enumerate().rev() {
            if item.state != TimelineItemState::Ok || item.body.is_empty() {
                continue;
            }
            let have = terms(&item.body);
            if !wanted.iter().all(|w| have.iter().any(|h| h.starts_with(w))) {
                continue;
            }
            hits.push(SearchHit {
                channel: *channel,
                community,
                seq: item.seq.unwrap_or(index as u64 + 1),
                snippet: item.body.chars().take(MAX_SNIPPET_CHARS).collect(),
                author: item.sender_user.and_then(|u| name_of(state, u)),
                ts: Some(item.ts),
                thread_root: None,
            });
            if hits.len() == MAX_HITS {
                return hits;
            }
        }
    }
    hits
}

#[cfg(test)]
mod tests {
    use super::*;

    fn gid() -> [u8; 16] {
        [7; 16]
    }

    fn hit_row(extra: Vec<Value>) -> Vec<u8> {
        let mut r = vec![
            Value::Bytes(gid().to_vec()),
            Value::Uint(2),
            Value::Text("boat".into()),
            Value::Uint(0),
        ];
        r.extend(extra);
        Value::Array(vec![Value::Array(r)]).encode()
    }

    /// state/search.test.ts `rejects unbounded hits and snippets`.
    #[test]
    fn hits_decode_with_their_bounds() {
        let four = decode_hits(&hit_row(vec![])).unwrap();
        assert_eq!(
            four,
            vec![RawHit {
                group: gid(),
                seq: 2,
                snippet: "boat".into(),
                thread_root: None
            }]
        );
        // web3.7 fix round 1 (M3): a fifth element names the hit's thread root; six is malformed.
        let five = decode_hits(&hit_row(vec![Value::Bytes(gid().to_vec())])).unwrap();
        assert_eq!(five[0].thread_root, Some(gid()));
        assert!(decode_hits(&hit_row(vec![Value::Bytes(gid().to_vec()), Value::Null])).is_err());
        let fifty_one = Value::Array(
            (0..51)
                .map(|_| {
                    Value::Array(vec![
                        Value::Bytes(gid().to_vec()),
                        Value::Uint(2),
                        Value::Text("boat".into()),
                        Value::Uint(0),
                    ])
                })
                .collect(),
        )
        .encode();
        assert!(decode_hits(&fifty_one).is_err());
        let one = |snippet: String, rank: u64| {
            Value::Array(vec![Value::Array(vec![
                Value::Bytes(gid().to_vec()),
                Value::Uint(2),
                Value::Text(snippet),
                Value::Uint(rank),
            ])])
            .encode()
        };
        assert!(decode_hits(&one("a".repeat(161), 0)).is_err());
        assert!(decode_hits(&one("a".repeat(160), 0)).is_ok());
        assert!(decode_hits(&one("boat".into(), 1)).is_err());
    }

    /// state/search.test.ts `checks UTF-8 query bytes`.
    #[test]
    fn a_query_is_one_to_128_utf8_bytes_trimmed() {
        assert!(!searchable("   "));
        assert!(searchable("  boat "));
        assert!(searchable(&"å".repeat(64)));
        assert!(!searchable(&"å".repeat(65)));
    }

    #[test]
    fn terms_fold_case_and_accents_and_split_on_punctuation() {
        assert_eq!(terms("Café-lamp, ÅNGSTRÖM!"), ["cafe", "lamp", "angstrom"]);
        assert_eq!(terms("  ?! "), Vec::<String>::new());
    }
}
