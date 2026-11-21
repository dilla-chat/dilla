//! Seeds the polls of a fixture into the real dillad. Owned by Task 20; no other file does. Task 40 seeds them through the
//! server's test Bot (`ServerSpec::bot`, seed/bot.rs).
//!
//! A poll is the Bot's record, so its creator's client (the viewer, as the GPUI mapping has it: the card offers Close poll)
//! asks the Bot for it (operation 70) and the Bot posts it: the spec's message with a `poll` is not sent as text. Each voter
//! then reads the poll from the Bot and votes from their own client (73), and `closed` has the creator close it (74).

use std::collections::{BTreeMap, HashMap};
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::utilities::polls::{PollCreate, final_poll_marker};
use dilla_client::state::{
    AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState,
};
use dilla_client::testing::host::bot::wait_for_catalog;
use dilla_client::testing::host::{self as world, Person};
use dilla_client::testing::spec::{MessageSpec, PollSpec, ScreenFixture, derive_id, unhex16};

/// The duration a fixture's poll runs (the form's default, 24h), as `fixture/polls.rs` draws it.
const DURATION: u64 = 86_400;
/// How long the Bot's post may take to reach a client, and a client to hold the Bot's catalog.
const WAIT: Duration = Duration::from_secs(30);
const CATALOG: Duration = Duration::from_secs(90);
/// How often, and how long after each try, the Bot is asked for a poll again.
const RETRIES: u32 = 12;
const RETRY_AFTER: Duration = Duration::from_secs(5);

/// A Bot person's post of a poll that is never verified (the web's give-up path, Task 39): its body is what the Bot posts for
/// a poll (cmd/dillabot/utility_runtime.go: question, answers, "Vote with /poll vote." and "Closes:" in blank-line separated
/// paragraphs, then the marker line). The world has the real Bot make that poll, and the shooter leaves the browser's read of
/// it to be refused (`pollGivesUp`), so the web draws the post's own text. The question and the answers, `None` for any other
/// message.
fn unverified_poll(f: &ScreenFixture, m: &MessageSpec) -> Option<PollSpec> {
    if m.poll.is_some()
        || !super::is_server_bot(f, &m.sender)
        || final_poll_marker(&m.body).is_none()
    {
        return None;
    }
    let mut paragraphs = m.body.split("\n\n");
    let question = paragraphs.next()?.to_owned();
    let options: Vec<String> = paragraphs.next()?.lines().map(str::to_owned).collect();
    Some(PollSpec {
        question,
        options,
        votes: Vec::new(),
        closed: false,
    })
}

/// Whether the fixture holds a poll post the browser gives up on (see [`unverified_poll`]).
pub(super) fn polls_give_up(f: &ScreenFixture) -> bool {
    f.world
        .messages
        .iter()
        .any(|m| unverified_poll(f, m).is_some())
}

/// Names the facet when the fixture asks for it and this world cannot seed it: a poll needs the Bot of its server, with
/// the polls capability and its channel marked.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    for m in w
        .messages
        .iter()
        .filter(|m| m.poll.is_some() || unverified_poll(f, m).is_some())
    {
        let ready = w.channel(&m.channel).is_some_and(|(s, c)| {
            s.bot.as_ref().is_some_and(|b| {
                b.capabilities.iter().any(|n| n == "polls") && b.bot_channels.contains(&c.id)
            })
        });
        if !ready {
            out.push(format!(
                "polls: {} needs a server `bot` with the polls capability and its channel as a Bot channel",
                m.key
            ));
        }
    }
}

/// Whether the fixture holds a poll, which the Bot must be enabled for before the history is posted.
pub(super) fn needs_bot_first(f: &ScreenFixture) -> bool {
    f.world
        .messages
        .iter()
        .any(|m| m.poll.is_some() || unverified_poll(f, m).is_some())
}

/// Posts `m` when it is a poll: the viewer's client creates it through the Bot, which posts it, and every voter votes (and
/// the creator closes it when the spec says so) before the answer, the id of the Bot's post. `None` for any other message.
pub(super) async fn post(
    f: &ScreenFixture,
    m: &MessageSpec,
    clients: &[(&str, Client)],
    ids: &BTreeMap<String, String>,
    channel: ChannelId,
    posted: &HashMap<String, MessageId>,
) -> Option<Result<MessageId, dilla_client::ClientError>> {
    let unverified = unverified_poll(f, m);
    let poll = m.poll.as_ref().or(unverified.as_ref())?;
    let client_of = |who: &str| {
        &clients
            .iter()
            .find(|(id, _)| *id == who)
            .unwrap_or_else(|| panic!("{who} holds no client (the seed plan adds the voters)"))
            .1
    };
    let (server, _) = f.world.channel(&m.channel).expect("validated");
    let community = CommunityId(unhex16(&ids[&format!("server:{}", server.id)]).expect("hex"));
    let thread = m.thread_of.as_ref().map(|k| {
        *posted
            .get(k)
            .unwrap_or_else(|| panic!("`{k}` is posted before its poll"))
    });
    let creator = client_of(&f.viewer);
    wait_for_catalog(creator, community.0, CATALOG).await;
    // Every distinct voter once, in the order they first vote, with all of their answers (the Bot keeps one selection).
    let mut voters: Vec<(&str, Vec<usize>)> = Vec::new();
    for v in &poll.votes {
        match voters.iter_mut().find(|(who, _)| *who == v.by) {
            Some((_, choices)) => choices.push(v.option),
            None => voters.push((&v.by, vec![v.option])),
        }
    }
    let input = PollCreate {
        channel: channel.0,
        thread: thread.map(|t| t.0),
        question: poll.question.clone(),
        answers: poll.options.clone(),
        multi: voters.iter().any(|(_, c)| c.len() > 1),
        duration: DURATION,
    };
    // The world moved the host's clock to this message's time, which every connection (the Bot's too) answers by
    // reconnecting; the Bot may not have its channel's group back when the first ask arrives, and says so. The request id
    // is the poll's own, so asking again settles the same poll rather than making a second.
    let request = derive_id("poll", &m.key);
    let mut tries = 0;
    let created = loop {
        match creator
            .utilities()
            .create_poll(community, input.clone(), request)
            .await
        {
            Ok(created) => break created,
            Err(e) if tries < RETRIES => {
                tries += 1;
                eprintln!("poll {}: not made yet ({e:?}), asking again", m.key);
                tokio::time::sleep(RETRY_AFTER).await;
            }
            Err(e) => panic!("poll {}: the Bot refused it ({e:?})", m.key),
        }
    };
    let post = held_item(creator, channel, thread, created.id)
        .await
        .msg_id
        .expect("found by it");
    for (who, choices) in &voters {
        let voter = client_of(who);
        wait_for_catalog(voter, community.0, CATALOG).await;
        let item = held_item(voter, channel, thread, created.id).await;
        let utilities = voter.utilities();
        utilities
            .read_poll(community, channel, thread, &item)
            .await
            .unwrap_or_else(|e| panic!("poll {}: reading it ({e:?})", m.key));
        // The poll is held once the client's state shows it: a vote asked before that finds none.
        let message = item.msg_id.expect("found by it");
        world::wait_for(voter, WAIT, "the poll held", |s| {
            s.bot.polls.contains_key(&message)
        })
        .await;
        utilities
            .vote(
                community,
                message,
                (channel, thread),
                choices.clone(),
                dilla_client::features::bot_admin::new_request_id(),
            )
            .await
            .unwrap_or_else(|e| panic!("poll {}: a vote ({e:?})", m.key));
    }
    if poll.closed {
        let item = held_item(creator, channel, thread, created.id).await;
        let utilities = creator.utilities();
        utilities
            .read_poll(community, channel, thread, &item)
            .await
            .unwrap_or_else(|e| panic!("poll {}: reading it ({e:?})", m.key));
        let message = item.msg_id.expect("found by it");
        world::wait_for(creator, WAIT, "the poll held", |s| {
            s.bot.polls.contains_key(&message)
        })
        .await;
        utilities
            .close(
                community,
                message,
                (channel, thread),
                dilla_client::features::bot_admin::new_request_id(),
            )
            .await
            .unwrap_or_else(|e| panic!("poll {}: closing it ({e:?})", m.key));
    }
    Some(Ok(post))
}

/// The Bot's post of poll `id` as `client` holds it (its final line names the poll). A thread's post is not a row of the
/// channel: the client reads the thread `thread` (opening it) and finds the post among its replies.
async fn held_item(
    client: &Client,
    channel: ChannelId,
    thread: Option<MessageId>,
    id: [u8; 16],
) -> TimelineItem {
    let Some(root) = thread else {
        let state = world::wait_for(client, WAIT, "the poll's post", |s| {
            find(s, channel, id).is_some()
        })
        .await;
        return find(&state, channel, id).expect("waited for");
    };
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        // The read is one page of the thread's newest replies; the post may not have reached the worker yet.
        let _ = client.threads().open(channel, root).await;
        let state = client.state().borrow().clone();
        let found = state
            .threads
            .threads
            .get(&root)
            .and_then(|d| d.replies.iter().find(|i| is_poll_post(i, id)).cloned());
        if let Some(item) = found {
            return item;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "timed out waiting for the poll's post in its thread"
        );
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
}

fn is_poll_post(i: &TimelineItem, id: [u8; 16]) -> bool {
    i.state == TimelineItemState::Ok && i.msg_id.is_some() && final_poll_marker(&i.body) == Some(id)
}

fn find(state: &AppState, channel: ChannelId, id: [u8; 16]) -> Option<TimelineItem> {
    state
        .timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| is_poll_post(i, id))
        .cloned()
}

/// Seeds the facet once the history is posted: nothing more (a poll is made with its post).
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
