# Dilla Bot backup and restore acceptance

This is a staging release gate for a coordinated, cold Dilla + Bot snapshot. Run it on a disposable
instance with no real community members or provider credentials. The server archive contains
instance keys and server-readable content; the Bot archive contains the runtime credential and the
key that decrypts its local database. Store both as secrets.

Since Bot v2 the Bot is a child of `dilla.service`: there is one service to stop and start, no
`dillabot.service` and no `/etc/dillabot`. The Bot's files are `/var/lib/dillabot`, and its settings are `[bot]`
in `dilla.toml`, which the server archive carries.

Local evidence is partial. These commands check the server restore lifecycle, Bot local state and
paused queue startup, and the server's stale-generation send refusal. They do not restore the server
and a Bot archive from one installation together.

- `go test ./cmd/dillad -run 'TestRestoreBumpsTheGenerationAndArmsTheHeal|TestServeAdmitsABackupRefusesARestoreAndFinishesOneAtStart' -count=1` checks the server restore generation and heal window.
- `go test ./cmd/dillabot -run 'TestColdBotArchiveRoundTripKeepsKeyAndSQLiteSidecarsDoctorReady|TestNewMusicSupervisorRestoresPersistedStateWithoutStartingAudio|TestRestoreMusicSupervisorsCreatesPausedVoiceQueuesOnly' -count=1` checks Bot archive files and paused queue startup.
- `go test ./internal/ds -run 'TestManagedBotSendAcceptReplayAndResolveAreDurable/stale_MLS_snapshot_send_is_refused_before_group_heal' -count=1` directly invokes the delivery service's restore hook and checks that stale-epoch ciphertext is refused without adding an application message.

The real local testhost path is `restore_heal_from_tail.scn`, run with `dilla-testhost` and
`dilla-testkit` on disposable data. It writes and restores the server archive through the host's
production backup/restore operations, then heals the group with an active member and verifies
messages flow in both directions. That scenario uses human test clients, not a managed Bot.

Run record, 2026-10-23: the testhost scenario passed on unique loopback ports `60527` and `51867`
with data under a disk-backed `TMPDIR`. The focused Go commands above passed (`cmd/dillabot`,
`internal/ds`, and `cmd/dillad`); `cargo test -p dilla-testkit --lib --locked` passed 95 tests.
The generated testhost invite stayed in the local process environment and was not included in the
record. The first Go attempt used `/tmp`, which was a full tmpfs; rerunning with disk-backed
`TMPDIR` succeeded.

The existing offline checks establish:

- The cold archive fixture round trip preserves `database.key`, `state.db`, and SQLite `-wal`, `-shm`, and
  `-journal` files byte for byte with mode `0600`; the restored state directory remains `0700` and
  `dillabot doctor` (the v1 doctor) accepts it. That doctor reads a v1 `bot.toml` and is not part of the
  procedure below; there the check is that every file belongs to `dillabot` and `dillad doctor`'s `bot` leg is green.
- Queue tests retain the current track as paused and do not resolve or start a provider; they use an in-memory native-state seam.
- The stale-send test refuses a Bot message request while the group is `epoch_unknown` and verifies no additional application message was stored. It calls `DS.OnRestore` directly; it does not submit from a restored Bot process.
- `TestCompiledHelperNativeSendIntentIntegration` (Linux, with `DILLA_BOT_HELPER_INTEGRATION=1` and `DILLA_BOT_HELPER_BIN`) now copies the compiled helper's real encrypted SQLite state into a fresh restore directory and confirms that the restored native client replays the exact durable send intent. This is a reusable native-state restore leg, but still does not pair with a restored server or start `dillabot` itself.

The cold archive test uses opaque fixture bytes, not a production encrypted database or real
service account. The doctor checks local files and permissions; it does not contact dillad or prove
the installation is reconciled. The queue fixture does not create a queue in that archive.

## Staging procedure

Use an isolated staging host with `dilla` installed and the Bot running under it. Set `ARCHIVE_DIR` to a
directory on separate backup storage. The test must stop the writers before taking either archive so the two files
describe one coordinated point in time. Stopping `dilla` stops the Bot and `dilla-media` with it.

```sh
ARCHIVE_DIR=/srv/backups/dilla-staging
sudo install -d -m 0700 -o dillad -g dillad "$ARCHIVE_DIR"
sudo systemctl stop dilla

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
server="$ARCHIVE_DIR/dilla-$stamp.tar.gz"
bot="$ARCHIVE_DIR/dillabot-$stamp.tar.gz"

sudo -u dillad dillad backup --config=/etc/dilla/dilla.toml --include-blobs --out="$server"
sudo dillad backup verify --from="$server"

# Check every installation's state files and key before archiving. This must
# print 0600 for database.key and each present state.db sidecar.
sudo find /var/lib/dillabot -mindepth 2 -maxdepth 2 -type f \
  \( -name database.key -o -name 'state.db*' \) -printf '%m %U %p\n' | sort
sudo ls -ln /var/lib/dillabot/runtime.token   # 0600, owned by the dillabot user
sudo sh -c 'umask 077; tar -C / -czf "$1" var/lib/dillabot' sh "$bot"
sudo chmod 0600 "$server" "$bot"
sudo tar -tzf "$bot" | sort

# Start the baseline only after both snapshots have completed.
sudo systemctl start dilla
```

Record the file listings and hashes (`sha256sum "$server" "$bot"`) in the staging change record.
Confirm the Bot listing includes each active installation's `database.key`, `state.db`, and any
present `state.db-wal`, `state.db-shm`, or `state.db-journal`, along with `runtime.token`. An absent sidecar is
expected when SQLite has none; never synthesize one.

For the restore leg, stop `dilla` again, preserve the live staging directories as rollback
copies, then restore the server archive first while it stays stopped:

```sh
ARCHIVE_DIR=/srv/backups/dilla-staging
server="$(ls -1t "$ARCHIVE_DIR"/dilla-*.tar.gz | head -n 1)"
stamp="${server##*/dilla-}"
stamp="${stamp%.tar.gz}"
bot="$ARCHIVE_DIR/dillabot-$stamp.tar.gz"
test -s "$server" && test -s "$bot"

sudo systemctl stop dilla
sudo -u dillad dillad restore --config=/etc/dilla/dilla.toml --from="$server" --dry-run
sudo -u dillad dillad restore --config=/etc/dilla/dilla.toml --from="$server"
sudo tar --same-owner -C / -xzf "$bot"

# Check the restored DB key and state files before allowing the Bot to start.
sudo find /var/lib/dillabot -mindepth 2 -maxdepth 2 -type f \
  \( -name database.key -o -name 'state.db*' \) -printf '%m %U %p\n' | sort
sudo ls -ln /var/lib/dillabot/runtime.token
sudo -u dillad /usr/local/bin/dillad doctor --config=/etc/dilla/dilla.toml
```

Every restored file must belong to the `dillabot` user. A restore done as root without `--same-owner` leaves
`runtime.token` root-owned, and the Bot refuses it; fix it with
`sudo chown -R dillabot:dillabot /var/lib/dillabot` before going on. `dillad doctor`'s `bot` leg must be green.

Start `dilla`, check that the server is healthy and that its instance generation is newer than the one recorded
before the restore, and read the journal. The Bot starts with `dilla` and logs there:

```sh
since="$(date --iso-8601=seconds)"
sudo systemctl start dilla
sudo journalctl -u dilla --since "$since" --no-pager
```

Acceptance requires the restored installation to report `bot installation identity and membership
reconciled` with `ready=true` before operators permit Bot-triggered sends or playback. A
`ready=false`, degraded reconciliation, missing key, or a red `bot` leg is a stop condition: stop
`dilla`, keep the queue paused, and investigate. Do not manually resume a restored music queue
or provoke a Bot action while it is still reconciling. Also check that `dillad doctor` shows no
`commands failing since` line for the community, which is the health probe of the whole command path. Keep at
least one staging MLS group with active members online so its restored epoch can heal; verify clients resync and
the group becomes usable before testing an outbound Bot action. No outbound action may be observed before that
point.

A Bot channel's group is in the same position as any text group after a restore: the Bot rejoins it with the
members' heal. If a Bot channel still says **The Bot needs adding to this channel again** on **Server settings →
Bot** after the heal, press **Add the Bot again**.

## Evidence boundary

The paired acceptance test is still open. The local harness has no disposable way to start a
Bot process from an extracted Bot archive and trigger one of its real outbound actions against
the restored `dillad` instance. In particular:

- `TestManagedBotSendAcceptReplayAndResolveAreDurable` calls `DS.OnRestore` and then invokes
  `AcceptBotMessage` with ciphertext prepared before the restore. It proves the delivery service
  refuses that stale epoch, but it does not exercise a send initiated by a restored Bot process.
- `TestColdBotArchiveRoundTripKeepsKeyAndSQLiteSidecarsDoctorReady` archives fixture strings as
  opaque file contents. Its doctor seam skips WASM validation, so the archive is not an encrypted
  production Bot state database and cannot be used to trigger an MLS send.
- `restore_heal_from_tail.scn` performs a real server backup, restore, and member-driven heal, but
  its actors are human test clients. It neither restores a Bot archive alongside the server archive
  nor starts a Bot action before or after healing.
- Paused music queue restoration is tested independently with an in-memory native-state seam. The
  test does not carry that queue through the paired restore or a post-heal Bot action.

Closing this gate needs a disposable integration seam that creates a genuine Bot MLS snapshot,
archives that state together with the server snapshot, restores both, invokes a real Bot outbound
action while the restored group is still epoch-unknown, observes a refusal with no application
message, heals the group with an active member, and then verifies the Bot can send while restored
music queues remain paused. The operator procedure above is not a substitute for that automated
test, and has not been run; keep this as an open release acceptance gate.
