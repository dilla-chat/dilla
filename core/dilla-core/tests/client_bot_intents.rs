#![cfg(not(target_arch = "wasm32"))]

use dilla_core::bot::interaction_admission::Expected;
use dilla_core::cbor::decode_strict;
use dilla_core::client::ClientCore;
use dilla_core::ids::MsgId;
use dilla_core::mls::ConnHandle;
use std::path::PathBuf;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicU64, Ordering},
};

#[path = "client_support/mod.rs"]
mod support;

const INSTANCE: [u8; 16] = [0x11; 16];
const COMMUNITY: [u8; 16] = [0x22; 16];
const INSTALLATION: [u8; 16] = [0x33; 16];
const USER: [u8; 16] = [0x44; 16];
const OPERATION: [u8; 16] = [0x55; 16];
const OP_COMMIT: [u8; 16] = [0x56; 16];
const OP_JOIN: [u8; 16] = [0x57; 16];
const GROUP: [u8; 16] = [0x66; 16];
const GROUP_TWO: [u8; 16] = [0x68; 16];
const GROUP_THREE: [u8; 16] = [0x69; 16];
const CHANNEL: [u8; 16] = [0x77; 16];
const FOREIGN_COMMUNITY: [u8; 16] = [0x23; 16];
static FILE_ID: AtomicU64 = AtomicU64::new(0);
type IntentResult = Option<(u64, [u8; 16], u64, Option<Vec<u8>>)>;

fn conn() -> ConnHandle {
    Arc::new(Mutex::new(
        rusqlite::Connection::open_in_memory().expect("sqlite"),
    ))
}

fn openmls_row_count(connection: &rusqlite::Connection) -> i64 {
    let mut statement = connection
        .prepare("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'openmls_%'")
        .expect("OpenMLS table query");
    let tables = statement
        .query_map([], |row| row.get::<_, String>(0))
        .expect("OpenMLS tables")
        .collect::<Result<Vec<_>, _>>()
        .expect("OpenMLS table names");
    tables
        .into_iter()
        .try_fold(0_i64, |total, table| {
            let quoted = table.replace('"', "\"\"");
            let count =
                connection.query_row(&format!("SELECT count(*) FROM \"{quoted}\""), [], |row| {
                    row.get::<_, i64>(0)
                })?;
            Ok::<_, rusqlite::Error>(total + count)
        })
        .expect("OpenMLS state count")
}

fn file_conn() -> (PathBuf, ConnHandle) {
    let id = FILE_ID.fetch_add(1, Ordering::Relaxed);
    let path =
        std::env::temp_dir().join(format!("dilla-bot-intents-{}-{id}.db", std::process::id()));
    let connection = Arc::new(Mutex::new(
        rusqlite::Connection::open(&path).expect("sqlite file"),
    ));
    (path, connection)
}

fn reopen_file(path: &PathBuf) -> ConnHandle {
    Arc::new(Mutex::new(
        rusqlite::Connection::open(path).expect("reopen sqlite file"),
    ))
}

fn ready_bot(conn: ConnHandle) -> ClientCore {
    let mut core = ClientCore::open(conn).expect("open");
    core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
        .expect("bootstrap begin");
    core.bot_bootstrap_complete(&USER, "intent bot", 1_800_000_000)
        .expect("bootstrap complete");
    core
}

fn ready_bot_support(conn: ConnHandle) -> support::Core {
    let probe = conn.clone();
    let core = ready_bot(conn);
    let device = decode_strict(&core.identity().expect("identity"), |d| {
        d.array(6)?;
        d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        d.text()?;
        d.uint()?;
        Ok(device)
    })
    .expect("identity shape")
    .expect("device id");
    support::Core {
        core,
        probe,
        user: USER,
        device,
    }
}

fn ready_candidate(user: [u8; 16], username: &str) -> support::Core {
    let conn = support::memory();
    let probe = conn.clone();
    let mut core = ClientCore::open(conn).expect("open candidate");
    core.signup_begin(&INSTANCE)
        .expect("candidate signup begin");
    core.signup_complete(
        &user,
        username,
        1_800_000_000,
        &support::INSTANCE_SIGNER_PUB,
    )
    .expect("candidate signup complete");
    let device = decode_strict(&core.identity().expect("candidate identity"), |d| {
        d.array(6)?;
        d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        d.text()?;
        d.uint()?;
        Ok(device)
    })
    .expect("candidate identity shape")
    .expect("candidate device");
    support::Core {
        core,
        probe,
        user,
        device,
    }
}

fn result(core: &ClientCore, operation: &[u8; 16]) -> IntentResult {
    decode_strict(
        &core.bot_intent_result(operation).expect("intent result"),
        |d| {
            let len = d.array_len()?;
            if len == 1 {
                d.null()?;
                return Ok(None);
            }
            if len != 4 {
                return Err(dilla_core::cbor::CborError::TypeMismatch {
                    expected: "intent tuple",
                    offset: 0,
                });
            }
            Ok(Some((
                d.uint()?,
                d.bytes_exact()?,
                d.uint()?,
                d.opt_bytes()?.map(ToOwned::to_owned),
            )))
        },
    )
    .expect("result shape")
}

#[test]
fn create_intent_retries_exact_bytes_after_reopen_and_refuses_changed_input() {
    let (path, connection) = file_conn();
    let mut bot = ready_bot(connection.clone());
    let first = bot
        .bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
        .expect("create");
    drop(bot);
    drop(connection);

    let connection = reopen_file(&path);
    let mut bot = ClientCore::open(connection).expect("reopen");
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
            .expect("same retry"),
        first
    );
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 2, &[0x88; 32])
            .expect_err("changed kind conflicts")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &[0x78; 16], 0, 1, &[0x88; 32])
            .expect_err("changed channel conflicts")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(
        bot.bot_commit_build(&OPERATION, &GROUP, &[0x80])
            .expect_err("operation id cannot change intent kind")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(result(&bot, &[0x58; 16]), None);
    assert_eq!(result(&bot, &OPERATION), Some((2, GROUP, 0, Some(first))));
    drop(bot);
    std::fs::remove_file(path).expect("remove sqlite file");
}

#[test]
fn commit_intent_recovers_after_reopen_and_abort_resolution_is_idempotent() {
    let (path, connection) = file_conn();
    let mut bot = ready_bot(connection.clone());
    bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
        .expect("create");
    bot.bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register");
    let commit = bot
        .bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
        .expect("build commit");
    drop(bot);
    drop(connection);

    let connection = reopen_file(&path);
    let mut bot = ClientCore::open(connection.clone()).expect("reopen pending commit");
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
            .expect("repeat pending commit"),
        commit
    );
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80, 0])
            .expect_err("changed proposal input conflicts")
            .code,
        "E_CORE_STATE"
    );
    bot.bot_commit_abort(&OP_COMMIT, &GROUP)
        .expect("abort commit");
    bot.bot_commit_abort(&OP_COMMIT, &GROUP)
        .expect("same abort is idempotent");
    assert_eq!(
        bot.bot_commit_confirm(&OP_COMMIT, &GROUP)
            .expect_err("opposite outcome conflicts")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(result(&bot, &OP_COMMIT), Some((9, GROUP, 2, None)));
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
            .expect_err("resolved crypto operation cannot run again")
            .code,
        "E_CORE_STATE"
    );
    drop(bot);
    drop(connection);
    std::fs::remove_file(path).expect("remove sqlite file");
}

#[test]
fn failed_intent_insert_rolls_back_group_crypto_and_metadata() {
    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    {
        let guard = connection.lock().expect("connection lock");
        guard.execute_batch(
            "CREATE TRIGGER fail_group_intent BEFORE INSERT ON app_bot_intents BEGIN SELECT RAISE(ABORT,'intent fault'); END",
        ).expect("install fault");
    }
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
            .expect_err("intent receipt failure rolls back")
            .code,
        "E_CORE_STORAGE"
    );
    let guard = connection.lock().expect("connection lock");
    let rows: i64 = guard
        .query_row(
            "SELECT count(*) FROM app_groups WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("group row count");
    let intents: i64 = guard
        .query_row(
            "SELECT count(*) FROM app_bot_intents WHERE operation_id=?1",
            [OPERATION.as_slice()],
            |r| r.get(0),
        )
        .expect("intent row count");
    assert_eq!((rows, intents), (0, 0));
}

#[test]
fn commit_and_finalizer_receipt_failures_roll_back_mls_and_intent_state() {
    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
        .expect("create");
    bot.bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register");
    {
        let guard = connection.lock().expect("connection lock");
        guard.execute_batch(
            "CREATE TRIGGER fail_commit_intent BEFORE INSERT ON app_bot_intents WHEN NEW.kind=9 BEGIN SELECT RAISE(ABORT,'intent fault'); END",
        ).expect("install insert fault");
    }
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
            .expect_err("commit receipt failure rolls back")
            .code,
        "E_CORE_STORAGE"
    );
    let (pending, intents): (i64, i64) = connection
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT (SELECT pending_commit FROM app_groups WHERE group_id=?1),(SELECT count(*) FROM app_bot_intents WHERE operation_id=?2)",
            rusqlite::params![GROUP.as_slice(), OP_COMMIT.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("rollback state");
    assert_eq!((pending, intents), (0, 0));
    connection
        .lock()
        .expect("connection lock")
        .execute_batch("DROP TRIGGER fail_commit_intent")
        .expect("remove insert fault");

    bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
        .expect("build after rollback");
    connection
        .lock()
        .expect("connection lock")
        .execute_batch(
            "CREATE TRIGGER fail_commit_resolution BEFORE UPDATE ON app_bot_intents WHEN NEW.state<>0 BEGIN SELECT RAISE(ABORT,'resolution fault'); END",
        )
        .expect("install finalizer fault");
    assert_eq!(
        bot.bot_commit_confirm(&OP_COMMIT, &GROUP)
            .expect_err("finalizer receipt failure rolls back")
            .code,
        "E_CORE_STORAGE"
    );
    let (pending, state): (i64, i64) = connection
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT (SELECT pending_commit FROM app_groups WHERE group_id=?1),(SELECT state FROM app_bot_intents WHERE operation_id=?2)",
            rusqlite::params![GROUP.as_slice(), OP_COMMIT.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("finalizer rollback state");
    assert_eq!((pending, state), (1, 0));
    connection
        .lock()
        .expect("connection lock")
        .execute_batch("DROP TRIGGER fail_commit_resolution")
        .expect("remove finalizer fault");
    bot.bot_commit_confirm(&OP_COMMIT, &GROUP)
        .expect("confirm after rollback");
}

#[test]
fn external_join_intent_recovers_exact_commit_and_join_finalization_after_reopen() {
    let mut inviter = ready_bot(conn());
    let upload = inviter
        .bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
        .expect("create source group");
    let mut relay = support::Relay::new(GROUP);
    relay.register(&upload).expect("register source group");
    let connection = conn();
    let mut joiner = ready_bot(connection.clone());
    let joined_commit = joiner
        .bot_group_external_join(
            &OP_JOIN,
            &GROUP,
            &CHANNEL,
            0,
            1,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect("external join");
    drop(joiner);

    let mut joiner = ClientCore::open(connection).expect("reopen joining bot");
    assert_eq!(
        joiner
            .bot_group_external_join(
                &OP_JOIN,
                &GROUP,
                &CHANNEL,
                0,
                1,
                &relay.info_body(),
                &relay.tree_body(),
            )
            .expect("retry join bytes"),
        joined_commit
    );
    joiner
        .bot_group_joined(&OP_JOIN, &GROUP, 1)
        .expect("accepted join");
    joiner
        .bot_group_joined(&OP_JOIN, &GROUP, 1)
        .expect("same accepted sequence is idempotent");
    assert_eq!(
        joiner
            .bot_group_joined(&OP_JOIN, &GROUP, 2)
            .expect_err("changed accepted sequence conflicts")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(result(&joiner, &OP_JOIN), Some((5, GROUP, 1, None)));
}

#[test]
fn commit_confirmation_resolves_and_repeats_without_generating_another_epoch() {
    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
        .expect("create");
    bot.bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register");
    let before = bot
        .bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
        .expect("build commit");
    bot.bot_commit_confirm(&OP_COMMIT, &GROUP)
        .expect("confirm commit");
    bot.bot_commit_confirm(&OP_COMMIT, &GROUP)
        .expect("repeat confirmation");
    assert_eq!(result(&bot, &OP_COMMIT), Some((9, GROUP, 1, None)));
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
            .expect_err("resolved commit cannot be rebuilt")
            .code,
        "E_CORE_STATE"
    );
    let epoch: i64 = connection
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT epoch FROM app_groups WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("stored epoch");
    assert_eq!(epoch, 1);
    assert!(!before.is_empty());
}

#[test]
fn one_pending_intent_per_group_survives_a_competing_commit_and_stale_finalizers() {
    let instance = support::Instance::generate();
    let mut bot = ready_bot_support(conn());
    let mut relay = support::Relay::new(GROUP);
    let group = bot
        .core
        .bot_group_create(
            &OPERATION,
            &GROUP,
            &support::CHANNEL,
            0,
            1,
            &instance.public(),
        )
        .expect("create");
    relay.register(&group).expect("register relay");
    bot.core
        .bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register Bot");

    let mut peer = ready_candidate([0x93; 16], "competing peer");
    peer.join_external_in(&mut relay, Some(&COMMUNITY), &support::CHANNEL);
    bot.sync(&relay);

    let op_a = [0x59; 16];
    let op_b = [0x5a; 16];
    bot.core
        .bot_commit_build(&op_a, &GROUP, &[0x80])
        .expect("build first commit");
    peer.commit(&mut relay);
    bot.sync(&relay);

    assert_eq!(
        bot.core
            .bot_commit_build(&op_b, &GROUP, &[0x80])
            .expect_err("unresolved intent A owns this group")
            .code,
        "E_CORE_STATE"
    );
    bot.core
        .bot_commit_abort(&op_a, &GROUP)
        .expect("authoritatively refuse A after the competing commit");
    let commit_b = bot
        .core
        .bot_commit_build(&op_b, &GROUP, &[0x80])
        .expect("build B after resolving A");
    bot.core
        .bot_commit_abort(&op_a, &GROUP)
        .expect("stale abort A remains idempotent");
    assert_eq!(
        bot.core
            .bot_commit_confirm(&op_a, &GROUP)
            .expect_err("stale opposite finalizer cannot resolve A")
            .code,
        "E_CORE_STATE"
    );
    assert_eq!(
        bot.core
            .bot_commit_build(&op_b, &GROUP, &[0x80])
            .expect("stale A finalizer did not consume B"),
        commit_b
    );
    assert_eq!(
        result(&bot.core, &op_b),
        Some((9, GROUP, 0, Some(commit_b)))
    );
}

#[test]
fn delayed_first_confirm_preserves_proposals_staged_after_applied_own_handshake() {
    let instance = support::Instance::generate();
    let mut bot = ready_bot_support(conn());
    let mut relay = support::Relay::new(GROUP);
    let group = bot
        .core
        .bot_group_create(
            &OPERATION,
            &GROUP,
            &support::CHANNEL,
            0,
            1,
            &instance.public(),
        )
        .expect("create");
    relay.register(&group).expect("register relay");
    bot.core
        .bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register Bot");
    let mut peer = ready_candidate([0x94; 16], "confirm peer");
    peer.join_external_in(&mut relay, Some(&COMMUNITY), &support::CHANNEL);
    bot.sync(&relay);

    let accepted_operation = [0x5b; 16];
    let commit = bot
        .core
        .bot_commit_build(&accepted_operation, &GROUP, &[0x80])
        .expect("build commit");
    relay.commit(&commit).expect("accept bot commit");
    bot.sync(&relay);
    peer.sync(&relay);

    instance.propose_remove(&mut relay, peer.device);
    bot.sync(&relay);
    let proposals_before: i64 = bot
        .probe
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT count(*) FROM app_proposals WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("staged proposal count");
    assert_eq!(proposals_before, 1);

    bot.core
        .bot_commit_confirm(&accepted_operation, &GROUP)
        .expect("delayed first confirmation");
    let proposals_after: i64 = bot
        .probe
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT count(*) FROM app_proposals WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("proposal count after delayed finalizer");
    assert_eq!(proposals_after, proposals_before);
}

#[test]
fn human_incomplete_and_interaction_group_upload_intents_are_refused() {
    let mut human = support::ready_core(0x91, "human");
    assert_eq!(
        human
            .core
            .bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
            .expect_err("human identity refused")
            .code,
        "E_CORE_STATE"
    );

    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 2, 1, &[0x88; 32])
            .expect_err("interaction groups require typed participant bindings")
            .code,
        "E_CORE_INPUT"
    );
    assert_eq!(
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 3, 1, &[0x88; 32])
            .expect_err("interaction groups require typed participant bindings")
            .code,
        "E_CORE_INPUT"
    );
    for kind in [2, 3] {
        assert_eq!(
            bot.bot_group_external_join(&OP_JOIN, &GROUP, &CHANNEL, kind, 1, &[0x80], &[0x80])
                .expect_err("interaction and pairing groups cannot use external join")
                .code,
            "E_CORE_INPUT"
        );
        assert_eq!(
            bot.bot_welcomes_apply(&[0x80], &[0x80], kind)
                .expect_err("interaction and pairing Welcome admission require typed bindings")
                .code,
            "E_CORE_INPUT"
        );
    }
    assert_eq!(
        result(&bot, &OP_JOIN),
        None,
        "refused joins create no intent"
    );
    assert_eq!(
        result(&bot, &OPERATION),
        None,
        "refused creates create no intent"
    );
    let persisted_groups: i64 = connection
        .lock()
        .expect("connection lock")
        .query_row(
            "SELECT count(*) FROM app_groups WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("group count");
    assert_eq!(
        persisted_groups, 0,
        "refused interaction admission must not persist an MLS group"
    );

    let mut incomplete = ClientCore::open(conn()).expect("open incomplete Bot");
    incomplete
        .bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
        .expect("bootstrap begin");
    assert_eq!(
        incomplete
            .bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &[0x88; 32])
            .expect_err("unfinished Bot identity refused")
            .code,
        "E_CORE_NO_IDENTITY"
    );

    bot.group_create(&GROUP, Some(&FOREIGN_COMMUNITY), &CHANNEL, 1, &[0x88; 32])
        .expect("create foreign-community group through human API");
    assert_eq!(
        bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
            .expect_err("foreign community refused before crypto")
            .code,
        "E_CORE_STATE"
    );
    bot.call_group_create(&GROUP_TWO, Some(&COMMUNITY), &[0x79; 16], 1, &[0x88; 32])
        .expect("create call group through human API");
    bot.group_registered(&GROUP_TWO, 1)
        .expect("activate call group");
    assert!(
        !bot.bot_commit_build(&OP_COMMIT, &GROUP_TWO, &[0x80])
            .expect("call groups are valid Bot commit groups")
            .is_empty()
    );
    bot.bot_commit_abort(&OP_COMMIT, &GROUP_TWO)
        .expect("clear accepted call-group test commit");

    bot.group_create(&GROUP_THREE, Some(&COMMUNITY), &[0x7a; 16], 1, &[0x88; 32])
        .expect("create active text group");
    bot.group_registered(&GROUP_THREE, 1)
        .expect("activate text group");
    connection
        .lock()
        .expect("connection lock")
        .execute(
            "UPDATE app_groups SET kind=2 WHERE group_id=?1",
            [GROUP_THREE.as_slice()],
        )
        .expect("simulate an unsupported stored group kind");
    assert_eq!(
        bot.bot_commit_build(&[0x5c; 16], &GROUP_THREE, &[0x80])
            .expect_err("wrong stored group kind refused before crypto")
            .code,
        "E_CORE_STATE"
    );
}

#[test]
fn rejected_interaction_snapshot_leaves_no_group_or_create_intent() {
    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    let openmls_before = openmls_row_count(&connection.lock().expect("connection lock"));
    let bot_device = decode_strict(&bot.identity().expect("Bot identity"), |d| {
        d.array(6)?;
        d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        d.text()?;
        d.uint()?;
        Ok(device)
    })
    .expect("Bot identity shape")
    .expect("Bot device");
    let expected = Expected {
        instance: MsgId::from_bytes(INSTANCE),
        installation: MsgId::from_bytes(INSTALLATION),
        community: MsgId::from_bytes(COMMUNITY),
        bot_user: MsgId::from_bytes(USER),
        bot_device: MsgId::from_bytes(bot_device),
        participant_user: MsgId::from_bytes([0x90; 16]),
        participant_device: MsgId::from_bytes([0x91; 16]),
        group: MsgId::from_bytes([0x92; 16]),
        minimum_eligibility_revision: 1,
        server_now: 1_800_000_000,
        policy_version: 1,
    };
    assert!(
        bot.bot_interaction_group_create(
            &OPERATION,
            &[0x92; 16],
            b"invalid snapshot",
            &expected,
            b""
        )
        .is_err()
    );
    let connection = connection.lock().expect("connection lock");
    let app_counts: (i64, i64) = connection
        .query_row(
            "SELECT (SELECT count(*) FROM app_groups),(SELECT count(*) FROM app_bot_intents)",
            [],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .expect("app state counts");
    let openmls_after = openmls_row_count(&connection);
    assert_eq!(app_counts, (0, 0));
    assert_eq!(openmls_after, openmls_before);
}

#[test]
fn a_commit_intent_retains_all_256_real_welcome_bodies() {
    let instance = support::Instance::generate();
    let external_sender = instance.public();
    let mut bot = ready_bot_support(conn());
    let mut relay = support::Relay::new(GROUP);
    let group = bot
        .core
        .bot_group_create(&OPERATION, &GROUP, &CHANNEL, 0, 1, &external_sender)
        .expect("create group");
    relay.register(&group).expect("register group");
    bot.core
        .bot_group_registered(&OPERATION, &GROUP, 1)
        .expect("register bot group");

    let mut candidates = (0..256u32)
        .map(|index| {
            let mut user = [0u8; 16];
            user[0] = 0x92;
            user[12..].copy_from_slice(&index.to_be_bytes());
            ready_candidate(user, &format!("welcome-candidate-{index}"))
        })
        .collect::<Vec<_>>();
    eprintln!(
        "prepared {} distinct signed candidate identities",
        candidates.len()
    );
    let packages = candidates
        .iter_mut()
        .map(support::Core::first_key_package)
        .collect::<Vec<_>>();
    for package in &packages {
        instance.propose_add(&mut relay, package);
    }
    eprintln!(
        "queued {} authentic external Add proposals",
        relay.proposals.len()
    );
    bot.sync(&relay);
    for (proposal_index, candidate) in candidates.iter().enumerate() {
        support::ingest_add_candidate_proposal(&mut bot, &relay, proposal_index, candidate, &GROUP);
    }
    eprintln!(
        "authenticated all {} candidate device lists",
        candidates.len()
    );

    let commit = bot
        .core
        .bot_commit_build(&OP_COMMIT, &GROUP, &relay.proposals_body())
        .expect("build 256-Welcome commit");
    let (welcome_count, encoded_len) = decode_strict(&commit, |d| {
        d.array(5)?;
        d.uint()?;
        d.bytes()?;
        d.bytes()?;
        let n = d.array_len()?;
        for _ in 0..n {
            d.array(2)?;
            d.bytes()?;
            d.bytes()?;
        }
        d.null()?;
        Ok(n)
    })
    .map(|n| (n, commit.len()))
    .expect("commit upload shape");
    eprintln!("built {welcome_count} real Welcome bodies in {encoded_len} bytes");
    assert_eq!(welcome_count, 256);
    assert!(
        encoded_len > 1_048_576,
        "the valid upload exceeds the old frame cap"
    );
    drop(bot);
}

#[test]
fn bot_external_join_after_call_group_forget_is_not_wedged() {
    let mut inviter = ready_candidate([0x95; 16], "call creator");
    let upload = inviter
        .core
        .call_group_create(&GROUP, Some(&COMMUNITY), &CHANNEL, 1, &[0x88; 32])
        .expect("source call");
    let mut relay = support::Relay::new(GROUP);
    relay.register(&upload).expect("register source");
    inviter
        .core
        .group_registered(&GROUP, 1)
        .expect("source registered");
    let mut bot = ready_bot(conn());
    bot.bot_group_external_join(
        &OP_JOIN,
        &GROUP,
        &CHANNEL,
        1,
        1,
        &relay.info_body(),
        &relay.tree_body(),
    )
    .expect("stage E");
    bot.call_group_forget(&GROUP).expect("forget staged join");
    assert_eq!(result(&bot, &OP_JOIN), Some((5, GROUP, 2, None)));
    let rotation = inviter
        .core
        .call_rotate(&GROUP)
        .expect("advance source epoch");
    relay.commit(&rotation).expect("accept rotation");
    inviter
        .core
        .commit_confirm(&GROUP)
        .expect("confirm rotation");
    bot.bot_group_external_join(
        &[0x5e; 16],
        &GROUP,
        &CHANNEL,
        1,
        1,
        &relay.info_body(),
        &relay.tree_body(),
    )
    .expect("rejoin E+1");
}

#[test]
fn bot_external_join_backstop_preserves_live_joining_record() {
    let mut inviter = ready_candidate([0x95; 16], "call creator");
    let upload = inviter
        .core
        .call_group_create(&GROUP, Some(&COMMUNITY), &CHANNEL, 1, &[0x88; 32])
        .expect("source call");
    let mut relay = support::Relay::new(GROUP);
    relay.register(&upload).expect("register source");
    let mut bot = ready_bot(conn());
    let staged = bot
        .bot_group_external_join(
            &OP_JOIN,
            &GROUP,
            &CHANNEL,
            1,
            1,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect("live join");
    assert_eq!(
        bot.bot_group_external_join(
            &[0x5e; 16],
            &GROUP,
            &CHANNEL,
            1,
            1,
            &relay.info_body(),
            &relay.tree_body()
        )
        .expect_err("live join must conflict")
        .code,
        "E_CORE_STATE"
    );
    assert_eq!(result(&bot, &OP_JOIN), Some((5, GROUP, 0, Some(staged))));
}

#[test]
fn bot_external_join_backstop_repairs_only_gone_or_missing_same_group() {
    for state in [None, Some(0), Some(2), Some(3), Some(4)] {
        let mut inviter = ready_candidate([0x95; 16], "call creator");
        let upload = inviter
            .core
            .call_group_create(&GROUP, Some(&COMMUNITY), &CHANNEL, 1, &[0x88; 32])
            .expect("source call");
        let mut relay = support::Relay::new(GROUP);
        relay.register(&upload).expect("register source");
        inviter
            .core
            .group_registered(&GROUP, 1)
            .expect("source registered");
        let connection = conn();
        let mut bot = ready_bot(connection.clone());
        let staged = bot
            .bot_group_external_join(
                &OP_JOIN,
                &GROUP,
                &CHANNEL,
                1,
                1,
                &relay.info_body(),
                &relay.tree_body(),
            )
            .expect("stage join");
        // An unrelated group's pending upload must survive both cleanup paths.
        let other_upload = inviter
            .core
            .call_group_create(&GROUP_TWO, Some(&COMMUNITY), &[0x79; 16], 1, &[0x88; 32])
            .expect("other source call");
        let mut other_relay = support::Relay::new(GROUP_TWO);
        other_relay
            .register(&other_upload)
            .expect("register other source");
        bot.bot_group_external_join(
            &OPERATION,
            &GROUP_TWO,
            &[0x79; 16],
            1,
            1,
            &other_relay.info_body(),
            &other_relay.tree_body(),
        )
        .expect("other pending join");
        let other = result(&bot, &OPERATION);
        // Reproduce historical storage left by the old forget path, independently of the new forget.
        {
            let c = connection.lock().unwrap();
            match state {
                Some(state) => {
                    c.execute(
                        "UPDATE app_groups SET state=?2 WHERE group_id=?1",
                        rusqlite::params![GROUP.as_slice(), state],
                    )
                    .unwrap();
                }
                None => {
                    c.execute(
                        "DELETE FROM app_groups WHERE group_id=?1",
                        [GROUP.as_slice()],
                    )
                    .unwrap();
                }
            }
        }
        let rotation = inviter.core.call_rotate(&GROUP).expect("advance epoch");
        relay.commit(&rotation).expect("accept rotation");
        inviter
            .core
            .commit_confirm(&GROUP)
            .expect("confirm rotation");
        drop(bot);
        let mut bot = ClientCore::open(connection).expect("reopen historical orphan");
        let rejoin = bot.bot_group_external_join(
            &[0x5e; 16],
            &GROUP,
            &CHANNEL,
            1,
            1,
            &relay.info_body(),
            &relay.tree_body(),
        );
        if state.is_none() || state == Some(4) {
            rejoin.expect("orphan repaired");
            assert_eq!(result(&bot, &OP_JOIN), Some((5, GROUP, 2, None)));
        } else {
            assert_eq!(
                rejoin.expect_err("live row still owns pending upload").code,
                "E_CORE_STATE"
            );
            assert_eq!(result(&bot, &OP_JOIN), Some((5, GROUP, 0, Some(staged))));
        }
        assert_eq!(result(&bot, &OPERATION), other, "other group untouched");
    }
}

#[test]
fn bot_call_group_forget_rolls_back_intent_and_mls_together() {
    let mut inviter = ready_candidate([0x95; 16], "call creator");
    let upload = inviter
        .core
        .call_group_create(&GROUP, Some(&COMMUNITY), &CHANNEL, 1, &[0x88; 32])
        .expect("source call");
    let mut relay = support::Relay::new(GROUP);
    relay.register(&upload).expect("register source");
    let connection = conn();
    let mut bot = ready_bot(connection.clone());
    bot.bot_group_external_join(
        &OP_JOIN,
        &GROUP,
        &CHANNEL,
        1,
        1,
        &relay.info_body(),
        &relay.tree_body(),
    )
    .expect("stage join");
    let before = result(&bot, &OP_JOIN);
    let mls_before = openmls_row_count(&connection.lock().unwrap());
    connection.lock().unwrap().execute_batch("CREATE TRIGGER fail_forget BEFORE UPDATE OF state ON app_groups BEGIN SELECT RAISE(ABORT,'injected forget failure'); END").unwrap();
    bot.call_group_forget(&GROUP).expect_err("injected failure");
    assert_eq!(result(&bot, &OP_JOIN), before);
    let c = connection.lock().unwrap();
    assert_eq!(openmls_row_count(&c), mls_before);
    assert_eq!(
        c.query_row(
            "SELECT state FROM app_groups WHERE group_id=?1",
            [GROUP.as_slice()],
            |r| r.get::<_, i64>(0)
        )
        .unwrap(),
        1
    );
    c.execute_batch("DROP TRIGGER fail_forget").unwrap();
    drop(c);
    bot.call_group_forget(&GROUP).expect("retry forget");
    assert_eq!(result(&bot, &OP_JOIN), Some((5, GROUP, 2, None)));
}

#[test]
fn bot_call_group_forget_discards_create_and_commit_only_for_forgotten_group() {
    for commit in [false, true] {
        let connection = conn();
        let mut bot = ready_bot(connection.clone());
        bot.bot_group_create(&OPERATION, &GROUP, &CHANNEL, 1, 1, &[0x88; 32])
            .expect("create call");
        let op = if commit {
            bot.bot_group_registered(&OPERATION, &GROUP, 1)
                .expect("register call");
            bot.bot_commit_build(&OP_COMMIT, &GROUP, &[0x80])
                .expect("stage commit");
            OP_COMMIT
        } else {
            OPERATION
        };
        bot.bot_group_create(&OP_JOIN, &GROUP_TWO, &[0x79; 16], 1, 1, &[0x88; 32])
            .expect("other create");
        bot.bot_group_registered(&OP_JOIN, &GROUP_TWO, 1)
            .expect("other active group");
        bot.bot_commit_build(&[0x5e; 16], &GROUP_TWO, &[0x80])
            .expect("other live commit");
        let other = result(&bot, &[0x5e; 16]);
        let before = result(&bot, &op);
        connection.lock().unwrap().execute_batch("CREATE TRIGGER fail_forget BEFORE UPDATE OF state ON app_groups BEGIN SELECT RAISE(ABORT,'injected forget failure'); END").unwrap();
        bot.call_group_forget(&GROUP)
            .expect_err("forget rolls back");
        assert_eq!(result(&bot, &op), before);
        connection
            .lock()
            .unwrap()
            .execute_batch("DROP TRIGGER fail_forget")
            .unwrap();
        bot.call_group_forget(&GROUP).expect("forget");
        assert_eq!(
            result(&bot, &op),
            Some((if commit { 9 } else { 2 }, GROUP, 2, None))
        );
        assert_eq!(
            result(&bot, &[0x5e; 16]),
            other,
            "other active group's live record untouched"
        );
        // A late discard/abort is idempotent with the same resolution hash.
        if commit {
            bot.bot_commit_abort(&op, &GROUP).expect("late abort");
        } else {
            bot.bot_group_discard(&op, &GROUP).expect("late discard");
        }
        bot.call_group_forget(&GROUP)
            .expect("gone forget remains idempotent");
    }
}
