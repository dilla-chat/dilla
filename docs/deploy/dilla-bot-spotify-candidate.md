# Dilla Bot Spotify candidate handoff

This runbook qualifies one same-host Bot candidate in a separate homelab session. It does not publish a release. The Bot candidate cannot work with the currently released `dillad`: Bot requires the matching Bot-capable server build, its embedded web client, and the WASI core from the same reviewed checkout. Installing only `dillabot` beside the released server is not a valid candidate.

The server upgrade can migrate the live database and restart API/media services. Treat this as a staging or production change with a scheduled maintenance window, operator present, verified restorable backup, and an explicit go/no-go gate. Start with staging; do not run against production unless the operator separately approves that risk. The instructions below build and prepare assets only; they do not deploy them.

## Before the host session

- Confirm the reviewed checkout is clean and record its full Git commit. Do not pin this handoff to a moving or stale hash.
- Confirm host OS and architecture (`x86_64` or `aarch64`), that the current `dillad` is healthy, and that the public origin is reachable over HTTPS.
- Confirm there is no existing `dillabot` service, account, config, or state for first install. `install-bot.sh` refuses to overwrite them. Existing Bot installs require the upgrade procedure below and a separate review.
- For an existing server, verify a recent backup by checking its nonzero size, manifest, database integrity, and expected row counts; establish that the operator can restore it. A backup that has not been restore-checked is not a maintenance gate.
- Record the currently installed dillad version, binary/core checksums, database schema version, and service state. Check active calls and schedule downtime: `install.sh --upgrade` can restart dillad and media services, ending calls.
- Review the new dillad migration set against the current schema. Identify the previous binary/core pair and the recovery path before starting. The installer attempts migration-aware rollback, but a migration or restore failure can leave service stopped and require operator recovery.
- Confirm an explicitly approved Spotify Premium test account is available. If not, run readiness checks only and record live Spotify qualification as not run.
- Keep OAuth tokens, authorization codes, refresh tokens, runtime credential contents, account login details, and database/state backup contents out of chat, command arguments, terminal transcripts, screenshots, and logs. No client secret is used. Do not put credentials in CLI arguments.

## Build and inspect a local candidate

Run in the reviewed checkout with Bash. These commands create local assets only; they do not create a tag, GitHub release, or deployment. Build for the server host's native architecture. The web build must be embedded into the `dillad` binary, and all server and Bot artifacts must come from this same checkout.

```sh
set -eu
git status --short --branch
test -z "$(git status --porcelain)"
commit="$(git rev-parse HEAD)"
printf 'candidate commit: %s\n' "$commit"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "unsupported host architecture" >&2; exit 1 ;;
esac

umask 077
candidate_dir="$HOME/.cache/dilla-candidate-${commit:0:12}-$(date -u +%Y%m%dT%H%M%SZ)"
install -d -m 0700 "$candidate_dir"

npm ci
bash scripts/install-ci-tool.sh wasm-bindgen
bash scripts/install-ci-tool.sh wasm-pack
bash scripts/install-ci-tool.sh binaryen
wasm-pack build core/dilla-core-wasm --target web --profile wasm-release \
  --mode no-install --out-dir ../../packages/core-wasm/pkg
node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
npm run typecheck -w @dilla/core-wasm
npm run typecheck -w @dilla/client-core
npm run typecheck -w @dilla/web
DILLA_NOISE_MODE=beta npm run build -w @dilla/web
test -s packages/web/dist/index.html
! grep -q 'content="placeholder"' packages/web/dist/index.html
rm -rf internal/web/dist
install -d -m 0755 internal/web/dist
cp -a packages/web/dist/. internal/web/dist/
DILLA_WEB_DIST="$PWD/internal/web/dist" go test -tags webdist ./internal/web/...

rustup target add wasm32-wasip1
cargo build --locked --release -p dilla-core-wasi --target wasm32-wasip1
cargo build --locked --release -p dilla-bot-core
cargo build --locked --release --manifest-path crates/dilla-spotify-helper/Cargo.toml

CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
  -ldflags="-s -w -X main.Version=0.1.0-candidate+${commit:0:12}" \
  -o "$candidate_dir/dillad-linux-$arch" ./cmd/dillad
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
  -ldflags="-s -w -X main.Version=0.1.0-candidate+${commit:0:12}" \
  -o "$candidate_dir/dillabot-linux-$arch" ./cmd/dillabot
install -m 0755 target/release/dilla-bot-core "$candidate_dir/dilla-bot-core-linux-$arch"
install -m 0755 target/release/dilla-spotify-helper "$candidate_dir/dilla-spotify-helper-linux-$arch"
install -m 0644 target/wasm32-wasip1/release/dilla_core_wasi.wasm "$candidate_dir/dilla_core_wasi.wasm"
install -m 0755 deploy/install.sh deploy/install-bot.sh "$candidate_dir/"

"$candidate_dir/dillad-linux-$arch" version
"$candidate_dir/dillabot-linux-$arch" version
(cd "$candidate_dir" && sha256sum \
  "dillad-linux-$arch" "dillabot-linux-$arch" \
  "dilla-bot-core-linux-$arch" "dilla-spotify-helper-linux-$arch" \
  dilla_core_wasi.wasm install.sh install-bot.sh > SHA256SUMS && \
  sha256sum -c --strict SHA256SUMS)
printf 'candidate assets: %s\n' "$candidate_dir"
```

Inspect the candidate commit, version output, file list, and checksums. Transfer the whole directory over the approved administrator path to a root-only directory on the target host; preserve the directory and filenames because the installers match checksums by exact asset name. Verify `sha256sum -c --strict SHA256SUMS` again on the target host. Keep the original local copy until qualification and rollback are complete.

The `install.sh` local upgrade requires `--binary`, `--core`, and `--sums`; it accepts `--spotify-helper` when that asset is present. It verifies each supplied asset against `SHA256SUMS`. The Bot installer requires `--binary`, `--helper`, `--module`, and `--sums`, and verifies all three Bot assets. Both installers must be from this same candidate checkout. The release workflow packages these same named assets and checksums after building the web client; a local candidate is not a signed or published release.

## Maintenance gate and matching server upgrade

Do not run this section without the operator's scheduled maintenance window, verified backup, restore plan, migration review, and documented pre-change state. These actions are intentionally not part of the local build handoff. On the target host, set `candidate_dir` to the root-only transferred directory and `arch` to `amd64` or `arm64`. First validate arguments without changing files:

```sh
candidate_dir=/root/dilla-candidate-COMMIT-TIMESTAMP
arch=amd64 # use arm64 on aarch64
sudo sha256sum -c --strict "$candidate_dir/SHA256SUMS"
sudo sh "$candidate_dir/install.sh" --upgrade \
  --binary "$candidate_dir/dillad-linux-$arch" \
  --core "$candidate_dir/dilla_core_wasi.wasm" \
  --spotify-helper "$candidate_dir/dilla-spotify-helper-linux-$arch" \
  --sums "$candidate_dir/SHA256SUMS" --check-args
```

After the operator confirms the gate, rerun the same command without `--check-args`. Do not use `--yes` unless the operator explicitly accepts ending active calls. The server installer keeps the previous binary/core/config alongside as `.prev`, takes a pre-migration SQLite copy, checks schema before and after, and attempts to migrate down or restore the previous pair on failure. A failure can still require manual recovery; follow the installer output and inspect the live schema before restoring any old binary. Never start a previous dillad against a newer schema. Preserve the pre-change backup and installer-created recovery files until the candidate is accepted.

Confirm the matching server build is active and healthy before installing Bot:

```sh
sudo /usr/local/bin/dillad version
sudo runuser -u dillad -- /usr/local/bin/dillad doctor --config=/etc/dilla/dilla.toml
sudo systemctl is-active --quiet dilla
```

If any check fails, stop and use the reviewed server recovery plan. Do not proceed to Bot installation against a mixed server/client/core version.

## First Bot install beside the candidate server

On the Dilla host, set the public HTTPS origin and public `instance_id` from `GET https://<dilla-host>/v1/instance`. These are not credentials. `install-bot.sh` creates a runtime credential and private state, so this is a host mutation and must remain within the approved maintenance session. First validate arguments:

```sh
candidate_dir=/root/dilla-candidate-COMMIT-TIMESTAMP
arch=amd64 # use arm64 on aarch64
base_url=https://dilla.example.org
instance_id=REPLACE_WITH_32_LOWERCASE_HEX_INSTANCE_ID
sudo sh "$candidate_dir/install-bot.sh" \
  --base-url "$base_url" --instance-id "$instance_id" \
  --dillad-config /etc/dilla/dilla.toml \
  --binary "$candidate_dir/dillabot-linux-$arch" \
  --helper "$candidate_dir/dilla-bot-core-linux-$arch" \
  --module "$candidate_dir/dilla_core_wasi.wasm" \
  --sums "$candidate_dir/SHA256SUMS" --check-args
```

After the go/no-go gate, run the same command without `--check-args`. The installer checksum-verifies Bot assets, creates the service account and private runtime credential, runs `dillabot doctor`, then enables `dillabot.service`. It reports a runtime record ID, never the credential. Record the ID locally for rollback.

Check readiness and service state:

```sh
sudo -u dillabot /usr/local/bin/dillabot version
sudo -u dillabot /usr/local/bin/dillabot doctor --config=/etc/dillabot/bot.toml
sudo systemctl --no-pager --full status dillabot
sudo journalctl -u dillabot --since=-10m --no-pager
```

Doctor checks local files; it does not prove server reconciliation or Spotify playback. Review logs locally and redact identifiers/content before retaining evidence.

## Enable and qualify Spotify

In Dilla, open the community Bot settings, enter the public Spotify application client ID, and select **Link Spotify**. The screen shows the exact HTTPS callback URL to register in the Spotify application dashboard. Dilla derives that URL from the instance domain and passes the saved client ID to the assigned Bot runtime; no OAuth values need to be duplicated in server or Bot config files. Do not configure a client secret. Legacy `bot_spotify` and `spotify_client_id`/`spotify_redirect_url` config values remain as fallbacks for existing installations.

The candidate `install.sh --upgrade` installs the architecture-matched `dilla-spotify-helper` at `/usr/local/libexec/dilla-spotify-helper`. Confirm it exists and matches the candidate checksum. The helper uses OpenSSL 3 for native TLS. On Debian 13, install `libssl3t64` and confirm `ldd /usr/local/libexec/dilla-spotify-helper` reports no missing libraries. Set `spotify_helper_executable = "/usr/local/libexec/dilla-spotify-helper"` in `/etc/dillabot/bot.toml`, preserving installer-generated values. Restart only Bot after the UI setting is saved:

```sh
sudo systemctl restart dillabot
sudo -u dillabot /usr/local/bin/dillabot doctor --config=/etc/dillabot/bot.toml
sudo systemctl is-active --quiet dillabot
```

Use a disposable test community's **Server Settings > Bot > Music** flow. Complete step-up authentication, authorize the approved Spotify account, and confirm the callback returns to Dilla settings. Test one short known track in one test voice channel. Capture locally: full commit and binary versions, host architecture, candidate SHA-256 values, doctor output, active service state, successful account-link UI state without account identifiers, expected queue entry, and a listener's browser evidence that audio arrived and was audible. Verify Pause, Resume, and Stop; retry after token refresh if the session lasts long enough. Confirm a second channel reports busy while the first is active, and can play after Stop.

Record separate outcomes for OAuth linking, catalog/track resolution, helper PCM startup, encrypted browser audio, playback controls, and cross-channel lease behavior. Mark every unobserved leg **not run**. Account connectability alone does not prove Premium eligibility. A synthetic helper test, successful callback, metadata result, or doctor success is not live playback qualification. Do not capture token files, browser storage, callback query strings, or unfiltered service logs.

## Roll back

For a failed Bot first install, disable Bot for the test community, stop Bot, and revoke its runtime record using the locally recorded ID:

```sh
sudo systemctl disable --now dillabot
sudo /usr/local/bin/dillad admin bot-runtime revoke \
  --id=RUNTIME_ID_FROM_INSTALL_OUTPUT --config=/etc/dilla/dilla.toml
```

Verify that exact runtime row is revoked. Keep Bot state and its encryption key until the operator has reviewed evidence and confirmed no recovery is needed. For a fully approved disposable teardown only, remove Bot files and account after that review. Remove the Spotify helper only if this candidate session installed it and no other Dilla service uses it. Remove OAuth config entries only if they were added solely for this trial, then validate and restart dillad in the coordinated session.

For a failed server candidate, use the `install.sh` migration-aware recovery instructions and verify the database schema before selecting an old binary. The server upgrade may have changed the database; the pre-migration copy and the verified host backup are the recovery sources. Database restore can lose writes made since the backup, so keep the service in maintenance until the operator chooses and verifies the recovery point. Do not remove `.prev` files, the pre-migration copy, Bot backup, or host backup until the candidate is accepted.

## Existing Bot upgrade

Do not run first-install steps if Bot files or state already exist. `install-bot.sh --upgrade` preserves config, runtime credential, and state, but first runs the existing Bot doctor, stops an active Bot, stores a root-only backup of binaries, unit, and Bot state under `/var/backups/dillabot`, then installs the matching Bot assets and checks doctor/service state. On failure it restores files and state. The backup includes the database encryption key; preserve it as sensitive data. The Bot database migration path has not been qualified by this handoff, so review that risk and test against staging before considering production Bot state.
