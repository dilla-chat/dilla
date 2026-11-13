//! Polls (packages/client-core/src/bot/utilities.ts, packages/web/src/screens/commands/PollCard.tsx): the Bot's post
//! carries a final `[dilla-poll:v1:<id>]` line; the card shows only once the authenticated Bot author, that marker and the
//! Bot's durable record agree. Here: the marker, the canonical payloads and the card's model.

use crate::gateway::frames::Value;
use crate::state::bot::PollView;

/// The poll durations the Bot accepts, seconds: 1h, 4h, 8h, 24h, 3d, 7d.
pub const POLL_DURATIONS: [u64; 6] = [3600, 14_400, 28_800, 86_400, 259_200, 604_800];

/// A poll payload refusal (`BotUtilitiesError.code`).
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum PollError {
    #[error("invalid-poll-question")]
    Question,
    #[error("invalid-poll-answers")]
    Answers,
    #[error("invalid-poll-duration")]
    Duration,
    #[error("poll-payload-too-large")]
    TooLarge,
    #[error("invalid-poll-selection")]
    Selection,
    #[error("invalid-response")]
    Response,
}

/// `finalPollMarker`: the poll id a post's last line names, or `None`.
pub fn final_poll_marker(body: &str) -> Option<[u8; 16]> {
    let last = body.rsplit('\n').next()?.trim_end_matches('\r');
    let hex = last.strip_prefix("[dilla-poll:v1:")?.strip_suffix(']')?;
    if hex.len() != 32 || !hex.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

/// The text of a poll question or answer: not blank, at most `max` characters.
fn valid_text(s: &str, max: usize) -> bool {
    !s.trim().is_empty() && s.chars().count() <= max
}

/// The form's input (`PollCreatePayloadInput`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PollCreate {
    pub channel: [u8; 16],
    pub thread: Option<[u8; 16]>,
    pub question: String,
    pub answers: Vec<String>,
    pub multi: bool,
    pub duration: u64,
}

/// `encodePollCreatePayload`: `[1, channel, thread | null, question, answers, multi, duration]`, at most 1800 bytes.
pub fn encode_poll_create(input: &PollCreate) -> Result<Vec<u8>, PollError> {
    if input.channel == [0; 16] || input.thread == Some([0; 16]) {
        return Err(PollError::Response);
    }
    if !valid_text(&input.question, 300) {
        return Err(PollError::Question);
    }
    if !(2..=10).contains(&input.answers.len()) || !input.answers.iter().all(|a| valid_text(a, 100))
    {
        return Err(PollError::Answers);
    }
    let mut unique = input.answers.clone();
    unique.sort();
    unique.dedup();
    if unique.len() != input.answers.len() {
        return Err(PollError::Answers);
    }
    if !POLL_DURATIONS.contains(&input.duration) {
        return Err(PollError::Duration);
    }
    let payload = Value::Array(vec![
        Value::Uint(1),
        Value::Bytes(input.channel.to_vec()),
        input
            .thread
            .map_or(Value::Null, |t| Value::Bytes(t.to_vec())),
        Value::Text(input.question.clone()),
        Value::Array(input.answers.iter().cloned().map(Value::Text).collect()),
        Value::Uint(u64::from(input.multi)),
        Value::Uint(input.duration),
    ])
    .encode();
    if payload.len() > 1800 {
        return Err(PollError::TooLarge);
    }
    Ok(payload)
}

/// `encodePollSelection`: the chosen answers (0..=9, unique, at most ten); empty withdraws the vote.
pub fn encode_poll_selection(selection: &[usize]) -> Result<Vec<u8>, PollError> {
    if selection.len() > 10 || selection.iter().any(|&s| s > 9) {
        return Err(PollError::Selection);
    }
    let mut unique = selection.to_vec();
    unique.sort_unstable();
    unique.dedup();
    if unique.len() != selection.len() {
        return Err(PollError::Selection);
    }
    Ok(Value::Array(selection.iter().map(|&s| Value::Uint(s as u64)).collect()).encode())
}

/// One answer row of a card (`PollCardOption`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CardOption {
    pub text: String,
    /// Whole percent of all votes.
    pub pct: u32,
    pub mine: bool,
    /// The most votes; drawn as the winner once the poll is closed.
    pub winner: bool,
    pub voters: Vec<String>,
}

/// Each answer's share of the votes, whole percent (rounded), and whether it has the most votes.
pub fn card_options(poll: &PollView) -> Vec<CardOption> {
    let total: u64 = poll.counts.iter().sum();
    let most = poll.counts.iter().copied().max().unwrap_or(0);
    poll.answers
        .iter()
        .enumerate()
        .map(|(i, text)| {
            let count = poll.counts.get(i).copied().unwrap_or(0);
            let pct = (count * 100 + total / 2)
                .checked_div(total)
                .map_or(0, |p| u32::try_from(p).unwrap_or(100));
            CardOption {
                text: text.clone(),
                pct,
                mine: poll.my_choices.as_ref().is_some_and(|c| c.contains(&i)),
                winner: count == most && most > 0,
                voters: poll.previews.get(i).cloned().unwrap_or_default(),
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    //! Ported from utilities.test.ts (payloads) and PollCard.test.tsx (the marker).
    use super::*;
    use crate::state::{ChannelId, UserId};

    fn create(answers: &[&str]) -> PollCreate {
        PollCreate {
            channel: [1; 16],
            thread: None,
            question: "Lunch?".into(),
            answers: answers.iter().map(|a| (*a).to_owned()).collect(),
            multi: false,
            duration: 86_400,
        }
    }

    #[test]
    fn the_marker_is_the_final_line_only() {
        let id = "0123456789abcdef0123456789abcdef";
        assert!(final_poll_marker(&format!("Lunch?\n[dilla-poll:v1:{id}]")).is_some());
        assert!(final_poll_marker(&format!("[dilla-poll:v1:{id}]\nforged")).is_none());
        assert!(final_poll_marker(&format!("[dilla-poll:v1:{}]", id.to_uppercase())).is_none());
        assert!(final_poll_marker("[dilla-poll:v1:00]").is_none());
        assert!(final_poll_marker(&format!("x\r\n[dilla-poll:v1:{id}]")).is_some());
    }

    #[test]
    fn create_payload_rules() {
        let ok = encode_poll_create(&create(&["the bridge", "the harbour"])).unwrap();
        assert_eq!(ok[0], 0x87);
        assert_eq!(
            encode_poll_create(&create(&["one"])),
            Err(PollError::Answers)
        );
        assert_eq!(
            encode_poll_create(&create(&["a", "a"])),
            Err(PollError::Answers)
        );
        assert_eq!(
            encode_poll_create(&create(&["a", " "])),
            Err(PollError::Answers)
        );
        let mut long = create(&["a", "b"]);
        long.question = "q".repeat(301);
        assert_eq!(encode_poll_create(&long), Err(PollError::Question));
        let mut odd = create(&["a", "b"]);
        odd.duration = 7200;
        assert_eq!(encode_poll_create(&odd), Err(PollError::Duration));
        let big: Vec<String> = (0..10).map(|i| format!("{i}{}", "é".repeat(99))).collect();
        let mut too_large = create(&[]);
        too_large.answers = big;
        assert_eq!(encode_poll_create(&too_large), Err(PollError::TooLarge));
    }

    #[test]
    fn selection_rules() {
        assert_eq!(encode_poll_selection(&[]).unwrap(), vec![0x80]);
        assert_eq!(encode_poll_selection(&[1, 3]).unwrap(), vec![0x82, 1, 3]);
        assert_eq!(encode_poll_selection(&[1, 1]), Err(PollError::Selection));
        assert_eq!(encode_poll_selection(&[10]), Err(PollError::Selection));
    }

    #[test]
    fn card_percentages_and_winner() {
        let poll = PollView {
            id: [2; 16],
            channel: ChannelId([1; 16]),
            thread: None,
            creator: UserId([3; 16]),
            question: "Raid?".into(),
            answers: vec!["a".into(), "b".into(), "c".into()],
            multi: false,
            created_at: 1,
            due_at: 86_401,
            closed: true,
            counts: vec![1, 2, 0],
            my_choices: Some(vec![1]),
            revision: 1,
            previews: vec![
                vec!["Ada".into()],
                vec!["Mira".into(), "Björn".into()],
                vec![],
            ],
            distinct: 3,
            capped: false,
        };
        let o = card_options(&poll);
        assert_eq!(o.iter().map(|o| o.pct).collect::<Vec<_>>(), [33, 67, 0]);
        assert_eq!(
            o.iter().map(|o| o.winner).collect::<Vec<_>>(),
            [false, true, false]
        );
        assert!(o[1].mine && !o[0].mine);
    }
}
