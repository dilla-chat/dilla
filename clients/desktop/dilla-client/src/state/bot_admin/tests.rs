//! Ports of `bot/admin.test.ts`, `bot/provider-status.test.ts`, `bot/moderation-log.test.ts` and the decoding and
//! playhead cases of `voice/music.test.ts`: the same inputs, the same outputs.
use super::*;

fn b(n: u8) -> Value {
    Value::Bytes(vec![n; 16])
}
fn u(n: u64) -> Value {
    Value::Uint(n)
}
fn t(s: &str) -> Value {
    Value::Text(s.to_owned())
}
fn arr_of(items: Vec<Value>) -> Value {
    Value::Array(items)
}
fn community() -> CommunityId {
    CommunityId([0x11; 16])
}
fn hex_of(n: u8) -> String {
    hex(&[n; 16])
}

#[test]
fn sealed_operations_are_the_catalogs() {
    // Welcome settings are app operations 101 and 102; moderation evidence is 161; the status read is itself.
    assert_eq!([1, 2, 61].map(sealed_operation), [101, 102, 161]);
    assert_eq!(sealed_operation(BOT_STATUS_READ), 94);
    assert_eq!([71, 10, 84].map(sealed_operation), [91, 92, 93]);
    assert_eq!(sealed_operation(PROVIDER_STATUS), 124);
}

#[test]
fn the_status_read_round_trips() {
    let status = BotPageStatus {
        needs_re_add: vec![ChannelId([0x70; 16])],
        music_starts: vec![MusicStart {
            first_frame_ms: 640,
            cold: true,
        }],
    };
    assert_eq!(
        decode_bot_status(&encode_bot_status(&status)).unwrap(),
        status
    );
}

#[test]
fn the_status_is_decoded_strictly() {
    let enc = |v: Value| v.encode();
    assert!(decode_bot_status(&enc(arr_of(vec![u(2), arr_of(vec![]), arr_of(vec![])]))).is_err());
    assert!(
        decode_bot_status(&enc(arr_of(vec![
            u(1),
            arr_of(vec![Value::Bytes(vec![1])]),
            arr_of(vec![])
        ])))
        .is_err()
    );
    let twenty_one = (0..21).map(|_| arr_of(vec![u(1), u(0)])).collect();
    assert!(
        decode_bot_status(&enc(arr_of(vec![u(1), arr_of(vec![]), arr_of(twenty_one)]))).is_err()
    );
    assert!(
        decode_bot_status(&enc(arr_of(vec![
            u(1),
            arr_of(vec![]),
            arr_of(vec![arr_of(vec![u(1), u(2)])])
        ])))
        .is_err()
    );
}

#[test]
fn the_webhook_list_rows_decode() {
    let body = arr_of(vec![arr_of(vec![
        b(0x77),
        b(0x88),
        t("CI"),
        Value::Null,
        u(1),
        u(1_800_000_000),
        Value::Null,
    ])])
    .encode();
    assert_eq!(
        decode_bot_webhooks(&body).unwrap(),
        vec![BotWebhook {
            webhook_id: [0x77; 16],
            channel_id: ChannelId([0x88; 16]),
            name: "CI".to_owned(),
            avatar: None,
            allow_everyone: true,
            created_at: 1_800_000_000,
            rotated_at: None,
        }]
    );
}

#[test]
fn the_webhook_label_line_is_read_and_every_other_text_left_alone() {
    let id = "77".repeat(16);
    let avatar = "a7".repeat(32);
    let label = format!("[dilla-webhook:v1:{id}:Terraria%3A Hardcore:{avatar}]");
    assert_eq!(
        decode_webhook_label(&format!("boss down\n{label}")),
        Some(WebhookLabel {
            text: "boss down".to_owned(),
            webhook_id: id.clone(),
            name: "Terraria: Hardcore".to_owned(),
            avatar_id: Some(avatar),
        })
    );
    let plain = decode_webhook_label(&format!("hi\n[dilla-webhook:v1:{id}:CI]")).unwrap();
    assert_eq!(
        (plain.text.as_str(), plain.name.as_str(), plain.avatar_id),
        ("hi", "CI", None)
    );
    // A defanged marker, a label that is not the last line, and a malformed id are not labels.
    assert_eq!(
        decode_webhook_label(&format!("(dilla-webhook:v1:{id}:CI]")),
        None
    );
    assert_eq!(
        decode_webhook_label(&format!("[dilla-webhook:v1:{id}:CI]\nmore")),
        None
    );
    assert_eq!(decode_webhook_label("x\n[dilla-webhook:v1:zz:CI]"), None);
    // A name that is not a URI component is no label either.
    assert_eq!(
        decode_webhook_label(&format!("x\n[dilla-webhook:v1:{id}:%ZZ]")),
        None
    );
}

fn overview_v1() -> Vec<u8> {
    arr_of(vec![u(1), b(1), u(0), u(1), u(0)]).encode()
}
fn overview_v2(ops: Vec<u64>) -> Vec<u8> {
    arr_of(vec![
        u(2),
        b(1),
        u(0),
        u(1),
        u(0),
        arr_of(ops.into_iter().map(u).collect()),
    ])
    .encode()
}

#[test]
fn the_overview_capability_is_read_as_the_web_client_reads_it() {
    assert_eq!(decode_music_provider_capability(&overview_v1()), Ok(false));
    assert_eq!(
        decode_music_provider_capability(&overview_v2(vec![23, 24, 1])),
        Ok(true)
    );
    assert_eq!(
        decode_music_provider_capability(&overview_v2(vec![23, 1])),
        Ok(false)
    );
    let short = arr_of(vec![u(2), b(1), u(0), u(1), u(0)]).encode();
    assert!(decode_music_provider_capability(&short).is_err());
    let flag2 = arr_of(vec![u(1), b(1), u(0), u(2), u(0)]).encode();
    assert!(decode_music_provider_capability(&flag2).is_err());
    let zero_id = arr_of(vec![u(1), b(0), u(0), u(1), u(0)]).encode();
    assert!(decode_music_provider_capability(&zero_id).is_err());
    assert!(decode_music_provider_capability(&overview_v2(vec![24, 24])).is_err());
    assert!(decode_music_provider_capability(&overview_v2((0..33).collect())).is_err());
    assert!(decode_music_provider_capability(&overview_v2(vec![256])).is_err());
    assert!(decode_music_provider_capability(&[0u8; 129]).is_err());
}

#[test]
fn the_provider_status_decodes_with_its_reserved_slots() {
    let status = |setup: u64, reason: u64| {
        arr_of(vec![
            u(1),
            b(1),
            u(1_800_000_000),
            u(0),
            u(0),
            u(0),
            u(0),
            u(setup),
            u(reason),
        ])
        .encode()
    };
    let ok = decode_music_provider_status(&status(1, 0)).unwrap();
    assert_eq!(ok.youtube_playback, ProviderSetup::Configured);
    assert_eq!(ok.youtube_reason, ProviderReason::None);
    assert_eq!(
        decode_music_provider_status(&status(2, 8))
            .unwrap()
            .youtube_reason,
        ProviderReason::DependenciesMissing
    );
    // Reason ids 1 to 7 are retired and refused; so is any id past the last.
    assert!(decode_music_provider_status(&status(1, 3)).is_err());
    assert!(decode_music_provider_status(&status(1, 11)).is_err());
    assert!(decode_music_provider_status(&status(3, 0)).is_err());
    assert!(decode_music_provider_status(&[0u8; 513]).is_err());
}

fn case_row(community_hex: &str) -> Value {
    arr_of(vec![
        t(community_hex),
        t(&hex_of(2)),
        t(&hex_of(3)),
        t(&hex_of(4)),
        t(&hex_of(5)),
        u(2),
        t("spam"),
        t("remove"),
        t("buy now"),
        u(1_800_000_000),
    ])
}

#[test]
fn automatic_cases_are_a_page_of_rows() {
    let data = arr_of(vec![
        u(1),
        arr_of(vec![case_row(&hex_of(0x11))]),
        Value::Null,
    ])
    .encode();
    let (cases, cursor) = decode_moderation_page(MODERATION_CASES, &data, &community()).unwrap();
    assert_eq!(cursor, None);
    assert_eq!(
        cases,
        vec![ModerationCase {
            key: format!("{}:2", hex_of(5)),
            channel_id: hex_of(3),
            actor_id: hex_of(4),
            message_id: hex_of(5),
            revision: "2".to_owned(),
            reason: "spam".to_owned(),
            action: "remove".to_owned(),
            excerpt: "buy now".to_owned(),
            received_at: "1800000000".to_owned(),
        }]
    );
}

#[test]
fn manual_evidence_carries_its_cursor_as_hex() {
    let data = arr_of(vec![
        u(1),
        arr_of(vec![arr_of(vec![
            b(7),
            t("warn"),
            t("rude"),
            t(""),
            u(1_800_000_001),
        ])]),
        Value::Bytes(vec![0xab, 0xcd]),
    ])
    .encode();
    let (cases, cursor) = decode_moderation_page(MODERATION_EVIDENCE, &data, &community()).unwrap();
    assert_eq!(cursor.as_deref(), Some("abcd"));
    assert_eq!(cases[0].key, format!("manual:{}", "07".repeat(16)));
    assert_eq!(
        (cases[0].action.as_str(), cases[0].reason.as_str()),
        ("warn", "rude")
    );
}

#[test]
fn a_case_from_another_community_and_a_malformed_page_are_refused() {
    let other = arr_of(vec![
        u(1),
        arr_of(vec![case_row(&hex_of(0x22))]),
        Value::Null,
    ])
    .encode();
    assert!(decode_moderation_page(MODERATION_CASES, &other, &community()).is_err());
    let wrong_version = arr_of(vec![u(2), arr_of(vec![]), Value::Null]).encode();
    assert!(decode_moderation_page(MODERATION_EVIDENCE, &wrong_version, &community()).is_err());
    let nine: Vec<Value> = (0..9).map(|_| case_row(&hex_of(0x11))).collect();
    let too_many = arr_of(vec![u(1), arr_of(nine), Value::Null]).encode();
    assert!(decode_moderation_page(MODERATION_CASES, &too_many, &community()).is_err());
}

fn track_row(item: &str, video: &str, requester: Value) -> Value {
    arr_of(vec![
        t(item),
        t("youtube"),
        t(video),
        t("Track"),
        t("Artist"),
        t(""),
        u(120_000),
        requester,
    ])
}

fn snapshot(revision: u64, paused: u64, can_control: u64, tail: Vec<Value>) -> Vec<u8> {
    let mut row = vec![
        u(1),
        u(revision),
        track_row(&hex_of(0xef), "abcdefghijk", b(1)),
        arr_of(vec![]),
        u(0),
        u(paused),
        u(75),
        u(0),
        u(0),
        u(can_control),
    ];
    row.extend(tail);
    arr_of(row).encode()
}

#[test]
fn a_snapshot_decodes_with_the_requester_the_bot_recorded() {
    let s = decode_music_snapshot(&snapshot(1, 0, 1, vec![]), 5).unwrap();
    assert_eq!(s.revision, 1);
    assert_eq!(s.track.as_ref().map(|t| t.title.as_str()), Some("Track"));
    // A byte string in slot 7 is no requester.
    assert_eq!(s.track.as_ref().unwrap().requester, None);
    assert_eq!(
        (s.volume, s.paused, s.shuffle, s.repeat, s.can_control),
        (75, false, false, Repeat::Off, true)
    );
    assert_eq!(s.clock, None);
    let queued = arr_of(vec![
        u(1),
        u(2),
        Value::Null,
        arr_of(vec![track_row(
            &hex_of(0x01),
            "ABCDEFGHIJK",
            t(&hex_of(0x44)),
        )]),
        u(0),
        u(0),
        u(50),
        u(1),
        u(2),
        u(0),
    ])
    .encode();
    let s = decode_music_snapshot(&queued, 5).unwrap();
    assert_eq!(s.queue[0].requester, Some(UserId([0x44; 16])));
    assert_eq!(
        (s.shuffle, s.repeat, s.can_control),
        (true, Repeat::All, false)
    );
}

#[test]
fn a_snapshot_tolerates_a_newer_bot_and_refuses_a_malformed_one() {
    // 12 slots: the stamp; 13: a bounded queue's total.
    let stamped = snapshot(3, 0, 1, vec![u(1_000), u(1_600)]);
    let s = decode_music_snapshot(&stamped, 9_000).unwrap();
    assert_eq!(
        s.clock,
        Some(PlayheadClock {
            elapsed_ms: 600,
            received_at_ms: 9_000
        })
    );
    let bounded = snapshot(3, 0, 1, vec![u(0), u(0), u(7)]);
    assert_eq!(
        decode_music_snapshot(&bounded, 0).unwrap().queue_total,
        Some(7)
    );
    let under = snapshot(3, 0, 1, vec![u(0), u(0), u(0)]);
    assert_eq!(decode_music_snapshot(&under, 0).unwrap().queue_total, None);
    assert!(decode_music_snapshot(&arr_of(vec![u(1)]).encode(), 0).is_err());
    assert!(decode_music_snapshot(&snapshot(1, 2, 1, vec![]), 0).is_err());
    let bad_provider = arr_of(vec![
        u(1),
        u(1),
        arr_of(vec![
            t(&hex_of(1)),
            t("spotify"),
            t("abcdefghijk"),
            t("T"),
            t("A"),
            t(""),
            u(1),
            u(0),
        ]),
        arr_of(vec![]),
        u(0),
        u(0),
        u(1),
        u(0),
        u(0),
        u(0),
    ])
    .encode();
    assert!(decode_music_snapshot(&bad_provider, 0).is_err());
}

#[test]
fn the_playhead_runs_on_only_from_the_bots_stamp() {
    let mut s =
        decode_music_snapshot(&snapshot(1, 0, 1, vec![u(1_000), u(1_600)]), 10_000).unwrap();
    s.position_ms = 5_000;
    // 5 s + the Bot's 600 ms + 2 s of local time since the snapshot arrived.
    assert_eq!(playhead_ms(&s, 12_000), 7_600);
    // Clamped to the track.
    assert_eq!(playhead_ms(&s, 10_000_000), 120_000);
    // Paused: the position as it is.
    s.paused = true;
    assert_eq!(playhead_ms(&s, 12_000), 5_000);
    // An older Bot (no stamp): static.
    s.paused = false;
    s.clock = None;
    assert_eq!(playhead_ms(&s, 12_000), 5_000);
}

#[test]
fn search_results_have_stable_ids_and_a_watch_address() {
    let row = |id: &str| {
        arr_of(vec![
            t(id),
            t("youtube"),
            t("abcdefghijk"),
            t("Song"),
            t("Band"),
            t(""),
            u(61_000),
            t(""),
        ])
    };
    let data = arr_of(vec![u(1), arr_of(vec![row("yt:abcdefghijk")])]).encode();
    let r = decode_music_search(&data).unwrap();
    assert_eq!(r[0].track.id, "yt:abcdefghijk");
    assert_eq!(r[0].input, "https://www.youtube.com/watch?v=abcdefghijk");
    // An id that does not name its video is refused.
    let forged = arr_of(vec![u(1), arr_of(vec![row("yt:zzzzzzzzzzz")])]).encode();
    assert!(decode_music_search(&forged).is_err());
}

#[test]
fn the_player_time_is_m_ss() {
    assert_eq!(player_time(0.0), "0:00");
    assert_eq!(player_time(37.9), "0:37");
    assert_eq!(player_time(214.0), "3:34");
    assert_eq!(player_time(-5.0), "0:00");
}

#[test]
fn the_health_line_names_the_state_and_its_time() {
    let status = |state, since| BotCatalog {
        state,
        since,
        fingerprint: None,
        bot_channels: None,
    };
    assert_eq!(
        health_line(&status(CatalogState::Ready, Some(5))),
        Some(("bot.health.ok", None))
    );
    assert_eq!(
        health_line(&status(CatalogState::Degraded, None)),
        Some(("bot.health.failingNow", None))
    );
    assert_eq!(
        health_line(&status(CatalogState::Offline, Some(9))),
        Some(("bot.health.offline", Some(9)))
    );
    assert_eq!(health_line(&status(CatalogState::Other, None)), None);
}

#[test]
fn an_installation_row_is_scoped_to_its_community() {
    let row = |community: u8| {
        arr_of(vec![
            b(1),
            b(community),
            u(3),
            u(2),
            Value::Null,
            Value::Null,
            u(0b11),
            u(7),
            u(3),
            u(2),
            t("1.4.0"),
            u(75),
            u(30),
        ])
    };
    let ok = decode_installation(&row(0x11), &community()).unwrap();
    assert_eq!(
        (ok.state, ok.music_volume, ok.music_idle_minutes),
        (3, 75, 30)
    );
    assert_eq!((ok.restart_requested, ok.restart_handled), (3, 2));
    assert!(decode_installation(&row(0x22), &community()).is_err());
    let bad_volume = arr_of(vec![
        b(1),
        b(0x11),
        u(3),
        u(2),
        Value::Null,
        Value::Null,
        u(1),
        u(7),
        u(0),
        u(0),
        Value::Null,
        u(60),
        u(30),
    ]);
    assert!(decode_installation(&bad_volume, &community()).is_err());
}
