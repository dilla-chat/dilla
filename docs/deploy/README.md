# Deploying dillad

dillad is one static binary (`dillad`) plus one file beside it, `dilla_core_wasi.wasm`, the MLS
core it runs inside wazero. Set `livekit.mode = 'sidecar'` and run `dillad media` as a second
process: the API can restart while the SFU keeps existing rooms. The API owns the `/rtc` join
gate, webhook receiver, TLS and TURN relay. Replacing the media process ends its rooms;
the updated client reopens community voice during the upgrade window. This page covers the install
script (`deploy/install.sh`, the one supported install: a native systemd install), the systemd units it
writes, and the Proxmox LXC helper. There is no container image.

### Open tabs during an upgrade

Music player (task 8b): upgrade `dillad` with its web client first and reload open tabs, then `dillabot`. An older
web client cannot read the newer Bot's music snapshot; an older Bot simply does not post the "started music" message.

Server settings (task 10c): upgrade `dillad` and `dillabot` together and reload open tabs. Until then:

- A tab opened before the upgrade still shows the old DJ roles card on the Music page. The upgrade removes that setting:
  every DJ role becomes a role with the **Control music (DJ)** permission, so that card can no longer be saved. Reload
  the tab.
- A tab opened before the upgrade can still save the Bot's capabilities. Its request does not mention Polls and
  Reminders, so the server keeps them as they are.
- A tab opened before the upgrade drops **Control music** (bit 25) from any role or channel permissions it saves, because its permission mask is the old one. Reload before editing roles or channel permissions.
- An older `dillabot` reports no version, so Glenn's card shows none. A Restart stays "not picked up yet" until the newer
  `dillabot` runs. The Music page's default volume and idle-leave time take effect only with the newer `dillabot`, and
  the older one cannot read the new permission bit, so it refuses commands from owners, administrators and DJs.
  Upgrade both together.

Wire 5 adds ephemeral build, presence and coarse-call telemetry frames on the existing gateway. See
[transport inventory and rollback](../realtime-transport.md) for transport behavior, authorization and native-state
requirements. API and Bot artifacts must come from the same complete combined release.

Account kind (task S1, wire 4): a tab opened before the upgrade keeps talking wire 3 and works as before; after a reload it
negotiates wire 4 and starts checking each person's signed account kind. `dillabot` stays on wire 3 next to wire-4
people, so the order above still holds. Once browsers run wire 4, do not roll `dillad` back below it (see
[Client data version per release](#client-data-version-per-release)).

For upgrades already running the media sidecar, the normal path is:

```mermaid
sequenceDiagram
    actor Operator
    participant Rollout as Upgrade process
    participant API as dillad API and web
    participant Media as dillad media
    participant Tab as Open browser tab
    participant Store as Browser IndexedDB

    Operator->>Rollout: Upgrade to a new release
    Rollout->>API: Replace binary and web assets, then restart
    Note over Media: Existing rooms survive the API restart
    Rollout->>Media: Restart after API checks
    Note over Media: A media restart closes its rooms
    API-->>Tab: Verified build entry on gateway ready/resume
    loop On load, visibility/network return, reconnect or a new build hint
        Tab->>API: GET /app-version.json (no-store)
        API-->>Tab: Current JavaScript entry path
    end
    alt Entry differs from the tab's loaded entry
        Note over Tab: Wait for modal forms and live/recovering calls to finish
        Tab->>Store: Save encrypted view, drafts, files, and call choice
        Tab->>API: Reload the page
        API-->>Tab: Serve the new web build
        Tab->>Store: Read checkpoint
        Store-->>Tab: Return encrypted checkpoint
        Tab->>Tab: Decrypt and restore view, drafts, files, and call choice
        Tab->>Store: Delete checkpoint
        opt Previously in community voice
            Tab->>API: Reconnect and request call admission
            API-->>Tab: Allow the call join
            Tab->>Media: Establish a new media connection
        end
    else Entry is unchanged
        Note over Tab: Keep the current page
    end
```

The current browser verifies `/app-version.json` on startup, foreground, network return and
gateway reconnect, and when wire 5 supplies a new build entry. It never polls versions periodically.
An unresolved new hint retries with bounded backoff; verification stops those retries. A changed
entry waits for open modal forms and live/recovering calls, then checkpoints the view, drafts and
selected attachments before reloading and restoring them. Pre-login pages and browsers without
working checkpoint storage offer a manual reload. Screen sharing needs another click after
rejoining because the browser requires a user gesture. An older tab retains its older updater
until it reloads; reload it once to receive the current WSS-only activity and coarse-stats producers.

The media sidecar keeps the SFU room alive during an API restart, but the API's signalling and TURN
connections reset. The browser attempts to rejoin voice after a lost connection. Restarting the
media process closes its rooms. During the two minutes after an app update is detected or restored,
the browser also reopens a community voice room that closes with `E_CALL_ENDED`. A DM call that
ends stays ended. Outside that window, and on tabs from before this feature, a person must choose
Join again. A network or browser permission refusal can also require a manual join.

`dillad doctor --config=/etc/dilla/dilla.toml` checks the configuration, the database, the data
directory's mode and ownership, the wasi core, the clock, the certificate, the TURN relay, the
blob tree and the passkey settings (`webauthn`), and prints one line per check. By default its `udp` leg reads WARN "not probed": dillad
runs no reflector, so the leg never proves the UDP forward, which can only be checked from outside.
With `doctor.udp_probe = false` the leg reads OK "skipped", and with `livekit.enabled = false` OK
with no media port to reach. Run it after every change.

## 1. The TLS modes

`tls.mode` in `dilla.toml` is the one switch. `dillad init` writes `acme_tls_alpn`.

| Mode | Use it when | What it needs |
|---|---|---|
| `acme_tls_alpn` (default) | dillad is reachable from the internet on 443/tcp at `instance.domain`, and nothing else on the host owns 443. | `instance.domain` resolving to the host, `instance.public_ip`, `tls.agreed = true`. Issuance uses the TLS-ALPN-01 challenge on 443 itself, so no port 80 is needed. |
| `acme_dns` | 443 is not reachable from the internet while the certificate is issued (a home connection, a split-horizon network), or you want a wildcard. | The same, plus `tls.dns.provider` and `tls.dns.credentials_file`. Issuance uses the DNS-01 challenge, which needs no inbound connection. |
| `acme_ip` | The instance has no domain of its own and clients connect to an address. | A public IP the certificate authority can reach on 443/tcp (validation is TLS-ALPN-01). dillad forces the `shortlived` profile, the only one that permits IP names, so certificates are renewed far more often; `tls.renewal_window_ratio` sets when. |
| `behind_proxy` | A reverse proxy you already run terminates TLS. | dillad listens on `server.plain_listen` (`127.0.0.1:8080` by default) and trusts `X-Forwarded-For` only from `server.trusted_proxy_cidrs`, which must be non-empty. `instance.public_ip` and `tls.agreed` are not required. |

**Probing health.** `/healthz` (liveness) and `/readyz` (readiness) are served on `server.plain_listen` only. dillad does not also listen on the loopback address, so probe the address configured there, for example `curl -fsS http://10.0.0.5:8080/readyz` for `plain_listen = '10.0.0.5:8080'`. A `curl` to `127.0.0.1` prints `000` unless `plain_listen` is a loopback address. The installer's `--listen` sets it.

In every mode except `behind_proxy`, TURN shares 443 with the web server: a demultiplexer in front
of the TLS listener hands STUN to the relay and everything else to HTTPS. Only 443/tcp and
7882/udp (LiveKit's media port) need to be open.

### What the TURN relay can reach

The relay is restricted to the host's own SFU, by address and by port. It refuses a
`CreatePermission` or `ChannelBind` for any peer address that is not `livekit.node_ip` (plus, with
`livekit.advertise_internal_ip`, the host's interface addresses LiveKit also listens on, minus
`livekit.ips_excludes`), and it refuses all of them when LiveKit is off. On an admitted address the
relay socket then exchanges datagrams only with LiveKit's media port, `livekit.udp_port` (7882; with
`udp_port = 0`, LiveKit's 50000-60000 range), and never with another relay allocation: anything a
client sends elsewhere, and anything that arrives from elsewhere, is dropped and counted in
`dilla_turn_peer_drops_total`. Other UDP services on the admitted addresses, loopback included when
`livekit.node_ip` is unset, are not reachable through the relay, and one member cannot push traffic
into another member's relayed call. With `udp_port = 0` every port of that range on the admitted
addresses is reachable, so keep other services out of 50000-60000 there, or set a fixed `udp_port`.

The relay also bounds what one credential, and the whole instance, can hold: a TURN connection that
has not created an allocation within 30 seconds of connecting is closed, and so is one that has gone
61 minutes without an authenticated request on its allocation (longer than any allocation lives
without a refresh). Unauthenticated bytes, an `Allocate` refused at a quota, and the requests of a
revoked device or an expired credential do not keep a connection open. An `Allocate`
with `EVEN-PORT` or `RESERVATION-TOKEN` is refused with 508 (browsers never send either). At most
8192 relay allocations are live on the instance at once, whatever the devices; past that, an
`Allocate` is refused with 486, counted in `dilla_turn_capacity_refusals_total` and logged at WARN.
This is a fixed backstop until device enrolment per user is capped.

The relay offers UDP relays only. It refuses an RFC 6062 TCP allocation (`REQUESTED-TRANSPORT` TCP)
with STUN error 508, so no `Connect` can open a TCP connection to an admitted address. Browsers never
ask for one. Regression note: before this was fixed, pion's default relay generator served TCP
allocations, and any holder of a call credential could reach every TCP service on the admitted
addresses, loopback included. If you ran an instance built before that fix, check your TCP services
on those addresses.

### The `behind_proxy` TURN consequence

A TLS-terminating proxy cannot forward the TURN traffic that shares 443, so in `behind_proxy` mode
the relay has its own listener, `turn.listen`, and it is required while `turn.enabled` is true.
Either the operator publishes a second TCP port straight to dillad for TURN, or the UI shows the
explicit relay-unavailable dialog and the call is UDP-or-nothing. Decide this when you pick the
mode: a proxy in front of dillad is a convenient way to share 443 with other sites, and the price
is one more port to open, or calls that fail for people behind a UDP-blocking network.

dillad does not terminate TLS on `turn.listen`: the relay there is plain TURN over TCP (a recorded
deviation from the design, which puts TURN/TLS on the relay itself before the first public
release). Media stays DTLS-SRTP with SFrame end to end, but a relay credential's username
(`<expiry>:<device_id>:<issued>`), the allocation requests and the call timing cross the network in the
clear unless you put TLS in front of it: a TCP stream proxy that terminates TLS and forwards to
`turn.listen` (with `turn.proxy_protocol` if it sends a PROXY header). Tell clients where it is with
`turn.public_url`, which they are handed verbatim instead of `turn:<domain>:<turn.listen port>`:

```toml
[turn]
enabled = true
listen = "127.0.0.1:13478"
public_url = "turns:turn.example.org:5349?transport=tcp"
```

Set `turn.public_url` whenever the port the world reaches differs from `turn.listen`'s, TLS or not.

### TURN knobs for long calls

| key | default | what it bounds |
|---|---|---|
| `turn.credential_ttl` | `"1h"` (at least `"1m"`) | how long a relay credential handed out with a call can open a new allocation |
| `turn.max_allocation_age` | `"2h"` | how long one allocation keeps working after its credential was issued; never shorter than `credential_ttl` |
| `turn.allocations_per_device` | `4` (1–16) | live allocations per device; a browser needs about two per network it gathers on |

The credential's expiry is checked when an allocation is made, not on every refresh, so a relayed
call outlives its credential; `turn.max_allocation_age` is measured from the issue time each
credential carries, so changing `turn.credential_ttl` never lengthens an old credential's life, and
after it the browser re-allocates with the servers it re-fetched. A revoked, quarantined or
logged-out device loses its relay allocations at once. A user disabled by the running instance loses
them as soon as the call cut queue handles the user (at once while the SFU runs and the queue has
room), otherwise like a revocation written by `dillad admin`, which reaches a device holding an
allocation within about a minute (62 seconds at worst) while the database answers; the relay's own
lookup of a device on its next authenticated request can be up to 30 seconds late, as a "not barred"
answer is trusted for 30 seconds. The relay holds
its revocations in memory, so it refuses every credential issued before it started: after a restart
every client fetches a new one with its next call start. If the host's wall clock steps backwards, a
cut device's call start answers 429 until the clock passes the cut, and a step back past the moment
`dillad serve` started refuses every start until the clock passes it; keep the clock disciplined
(NTP slewing, not stepping). `dillad doctor` run from another host mints its relay probe on that
host's clock, which must be within 2 seconds of the server's. Watch `dilla_turn_allocations`, `dilla_turn_quota_refusals_total`,
`dilla_turn_relay_bytes_total` and the `turn_relay` leg of the admin diagnostics; raise
`turn.allocations_per_device` when refusals appear. `dilla_turn_cut_overflows_total` counts the
revocations that found the relay's revocation table full (65 536 live entries) and refused every
device's earlier credentials instead, each logged at WARN; any value above 0 means a mass relay
reconnect happened and is worth investigating. If `livekit.node_ip` is a public address the
host does not hold (behind NAT) and `livekit.advertise_internal_ip` is on, the relay does not admit
`node_ip` and `dillad serve` logs a warning saying so: relayed media pairs with the host's own
addresses instead.

### Behind a WireGuard tunnel (Pangolin): MTU and UDP 7882

Pangolin's `gerbil` and `newt` both default the tunnel MTU to **1280** (not the 1420 the design
assumed). WebRTC keeps RTP packets at most 1200 bytes, which fits with 36 bytes to spare over IPv4
and 16 over IPv6; raise `MTU` on both ends only together. The raw UDP forward for LiveKit's media
port is Traefik's UDP router in gerbil's network namespace, and Pangolin's installer defines no UDP
entry point, so add one with a session timeout well above LiveKit's 2-second keepalive (Traefik's
default is 3 seconds, and every expiry re-dials with a new source port):

```yaml
# Traefik static configuration
entryPoints:
  udp-7882:
    address: ":7882/udp"
    udp:
      timeout: 30s
```

and publish `7882:7882/udp` on the `gerbil` service. The 30-second value is pending the founder-path
measurement (follow-up card 6); the instance itself still publishes only 443/tcp and 7882/udp.
A UDP entry point that already exists without a `timeout` also falls back to Traefik's 3-second
default, so give it `timeout: 30s` as well.

- **The `udp` leg of `dillad doctor` never proves the forward.** By default it reads WARN
  "not probed", with or without the forward: dillad runs no reflector, so it cannot see the forward
  from inside. `ss -ulpn` on the Pangolin host should show a socket on `:7882` (the docker-proxy
  socket for gerbil's published port), but what settles it is the SFU's log line during a call:
  `participant active … connectionType: udp`.
- **Give the raw UDP resource exactly one target:** the node the container runs on. With one target
  per site, as HTTP resources have, the stream reaches the SFU from several source addresses and
  ICE re-validates without end.
- **Recreating `traefik` and `gerbil` drops every tunnel.** The public hostnames then answer 404
  (not 502) for about 20 seconds until the tunnels recover. Do it when nobody is in a call.

### LiveKit

dillad renders LiveKit's configuration itself from `[livekit]`; there is no LiveKit YAML to edit.
What it fixes, and the keys that change it:

- **Codecs.** Opus (no RED, no PCMU/PCMA), VP8 and H.264; dilla's clients publish H.264 only as
  Constrained Baseline with `packetization-mode=1`. (LiveKit cannot be limited to that profile in
  its own configuration without refusing H.264 altogether, so the restriction lives in the
  clients.) Every call is end-to-end encrypted frame by frame, so nothing else could be decrypted
  on the other side anyway. `livekit.vp9 = true` is refused at start: VP9 has no frame test vector
  yet and has not been measured through LiveKit. AV1 and H.265 are never offered.
- **Loopback only.** `livekit.bind_address` (default `127.0.0.1`) must be a loopback IP literal
  (`127.0.0.1`, `::1`); a hostname such as `localhost`, `0.0.0.0` or a LAN address is refused at
  start. Clients reach LiveKit only through dillad's `/rtc` proxy, which checks every join. A
  direct LiveKit client on another host reaches signalling through an ssh tunnel to loopback
  (`ssh -L 7880:127.0.0.1:7880 <host>`), with media direct on UDP — never by binding LiveKit to a
  LAN address. The capacity load rig cannot use a production dillad at all: its call routes delete
  every LiveKit room no call opened within 30 s, and `/metrics` needs the scrape token. The rig
  runs against a `dilla-testhost -sfu` on the host instead (`docs/spikes/2026-10-capacity.md`).
- **No TCP fallback.** LiveKit's own TCP fallback is off and `livekit.tcp_port` stays 0: a client
  that cannot reach UDP 7882 relays through TURN/TLS on 443 instead.
- **STUN.** `livekit.stun_servers` defaults to `<instance.domain>:3478` and is never served: it only
  keeps LiveKit from advertising Google's and Twilio's STUN servers to clients, which use the ICE
  servers dillad's calls route hands them.
- **Webhooks.** LiveKit reports joins, leaves and publications to dillad on
  `livekit.webhook_listen` (default `127.0.0.1:7883`), which must be a loopback address; it is
  signed with LiveKit's own API key. A non-default value while `livekit.enabled = false` is
  refused: nothing would listen there.
- **Host limits.** `livekit.limit_num_tracks` and `livekit.limit_bytes_per_sec` (bytes, not bits)
  are LiveKit's node-wide join gates, unset by default. They refuse new joins once reached, so size
  them generously: one 25-person voice call is about 600 forwarded tracks.
- **Publish caps.** `livekit.max_audio_bitrate_kbps` (64) and `livekit.max_share_bitrate_kbps`
  (8000) are handed to clients with every call token. A screen share encodes at the bitrate its
  picker's choice asks for (720p 2.5, 1080p 4.5, 1440p 8, source 12 Mb/s at 30 fps; ×0.6 at 15,
  ×1.6 at 60), never above the share cap, and the picker's "≈ … Mb/s up" shows that capped value.
  The estimate is the top layer's bitrate; a simulcast share also sends a small lower layer on top
  of it. Lower the cap on a host with little upload to spare (each sharer's stream is forwarded to
  every viewer); the range is 100..20000. The cap is advisory: the server cannot enforce a bitrate,
  so a hostile client can send more. `livekit.limit_bytes_per_sec` is the SFU's own hard limit.
- **Participant attributes.** No call token may update its own LiveKit name, metadata or attributes.
  A person's (or the bot's) round-trip time to the SFU reaches the others' tiles as their ping
  through dillad (`POST /v1/calls/{call_id}/rtt`), which writes it as the participant's `dilla.rtt`
  attribute at most every 1.5 s per device. The generated LiveKit config also caps a name at 32
  bytes, metadata at 1 byte and one attribute update at 64 bytes (`limit.*`), as defence in depth.
- **Which addresses LiveKit offers, and `ips_excludes`.** LiveKit opens its media socket
  (`udp_port`) on each address of the host's up interfaces (loopback only when `node_ip` is
  loopback, and never `::1`), skipping those inside `livekit.ips_excludes`, and builds its host
  candidates only from those sockets. `node_ip` is then offered for each of them, beside the local address with
  `advertise_internal_ip = true` (the default) or instead of it with `false`. So `ips_excludes` can
  drop an address but never adds one: if it covers every address the SFU has, LiveKit offers no
  candidate at all and every call fails, and `dillad serve` refuses to start (exit 78) with a message
  saying so. Use it with host networking or systemd to keep addresses such as `docker0`, WireGuard or
  Tailscale out of the candidates, and never on the address `node_ip` is, or the only address left.
- **Logs and metrics.** LiveKit's and pion's logs appear in the media process log (pion's only at
  ERROR) with a fixed set of fields — no SDP, no addresses, ids cut to eight characters — at most
  20 lines in a burst and 5 a second per message, with one "LiveKit log lines suppressed" line
  every 10 seconds counting the rest. In the older `in_process` mode, LiveKit's `livekit_*`
  metrics appear on dillad's `/metrics` beside `dilla_*`, without protocol version, country or
  other client-written values. The sidecar keeps its metrics in its own process; its registry is
  not exposed on the API's `/metrics` endpoint.
- **If LiveKit stops.** In sidecar mode, the API's `livekit` readiness gate turns red until the
  `media` process returns. The API stays up. In the older `in_process` mode, dillad exits with
  status 69 (`EX_UNAVAILABLE`) and its supervisor restarts the whole process.

## 2. Building from source

### Build dillad with the web client from source

The committed `internal/web/dist/index.html` is a placeholder. Build the browser client before
compiling dillad from source, then place the built files in the directory that `go:embed` reads.
These are the build and manifest-check commands used by CI's `web-build` and `go-release` jobs,
run from the repository root:

```sh
wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
node scripts/check-wasm-size.mjs packages/core-wasm/pkg/dilla_core_wasm_bg.wasm
npm ci
npm run typecheck -w @dilla/core-wasm
npm run typecheck -w @dilla/client-core
npm run typecheck -w @dilla/web
npm run build -w @dilla/web
npm run build:harness -w @dilla/client-core
go test -tags webdist ./internal/web/...
rm -rf internal/web/dist
cp -a packages/web/dist internal/web/dist
test -f internal/web/dist/index.html
if grep -q 'content="placeholder"' internal/web/dist/index.html; then echo "internal/web/dist still holds the placeholder client"; exit 1; fi
DILLA_WEB_DIST="$PWD/internal/web/dist" go test -tags webdist ./internal/web/...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/dillad-linux-amd64 ./cmd/dillad
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/dillad-linux-arm64 ./cmd/dillad
```

CI downloads the `dilla-web-dist` artifact into `internal/web/dist` in place of the local `cp`.
The `DILLA_WEB_DIST` test checks those exact bytes before either release binary is compiled.

## 3. systemd

`deploy/dilla.service` is a `Type=notify` unit: dillad sends `READY=1` once it is listening, so
`systemctl start` returns when the instance is up, and `TimeoutStartSec=300s` leaves room for
certificate issuance and a migration. It runs as the `dillad` user with only
`CAP_NET_BIND_SERVICE`, keeps its state in `/var/lib/dillad` (mode 0700) and reads
`/etc/dilla/dilla.toml`.
`deploy/dilla-media.service` runs the SFU as a separate `Type=simple` service. It has `PartOf=dilla.service`, so
stopping or restarting `dilla` stops or restarts the media sidecar with it; run `systemctl daemon-reload` after copying
the unit by hand (`install.sh --upgrade` does). `dillad media --rooms --config=/etc/dilla/dilla.toml` lists the live
rooms and their participants (`no live rooms` when the SFU is empty), and `dillad media --probe` prints the room count.

```sh
sudo install -m 0755 dillad-linux-amd64 /usr/local/bin/dillad
# Beside the binary: dillad looks for the wasi core in the directory its own executable is in.
sudo install -m 0644 dilla_core_wasi.wasm /usr/local/bin/dilla_core_wasi.wasm
sudo install -m 0644 deploy/dilla.service /etc/systemd/system/dilla.service
sudo install -m 0644 deploy/dilla-media.service /etc/systemd/system/dilla-media.service
sudo useradd --system --user-group --no-create-home --home-dir /var/lib/dillad --shell /usr/sbin/nologin dillad

sudo install -d -m 0755 /etc/dilla
sudo /usr/local/bin/dillad init --config=/etc/dilla/dilla.toml --data-dir=/var/lib/dillad \
  --domain=chat.example.org --public-ip=203.0.113.10 --agree-tos
# init wrote /etc/dilla/dilla.toml, which the unit reads (0600, owned by root until the next lines).
sudo chown root:dillad /etc/dilla/dilla.toml
sudo chmod 0640 /etc/dilla/dilla.toml
sudo sed -i "s/mode = 'in_process'/mode = 'sidecar'/" /etc/dilla/dilla.toml
sudo chown -R dillad:dillad /var/lib/dillad

sudo systemctl daemon-reload
sudo systemctl enable --now dilla-media
sudo systemctl enable --now dilla
sudo -u dillad /usr/local/bin/dillad doctor --config=/etc/dilla/dilla.toml
journalctl -o cat -u dilla | jq .
```

`init` writes the config to the path in `--config` (default `/etc/dilla/dilla.toml`) and nowhere else. It refuses an
existing config file unless you pass `--overwrite-config`, which replaces the file and never an instance: it always
refuses a data directory that holds `dilla.db` (or a `dilla.toml` left by an older init). Every file `dillad` creates in
the data directory is readable by its owner only (0600) and the directory is 0700, whatever the caller's umask; `serve`
also tightens the modes of an existing install at its next start.

Two directives are absent on purpose, and each has a comment in the unit:

- `MemoryDenyWriteExecute` is not set. wazero's compiler back end writes and then executes
  generated machine code, so the option would make the MLS path fail at runtime, not at start.
  Do not add it from a generic hardening template.
- `WatchdogSec` is not set. dillad reports readiness and nothing more over the notify socket, so a
  watchdog would make systemd abort a healthy service.

Exit status 78 means `dilla.toml` is wrong; the unit does not restart on it
(`RestartPreventExitStatus=78`), so read `journalctl -u dilla`, fix the file and start it again.

### Install script

`deploy/install.sh` does everything in this section on a Debian 13 machine, LXC container or VM, as
root, with no Docker: it runs the static `dillad` binary as the two systemd services above. Every
release attaches it beside `dillad-linux-amd64`, `dillad-linux-arm64`, the two architecture-matched
`dilla-spotify-helper-linux-*` binaries, `dilla_core_wasi.wasm` and `SHA256SUMS`, with the release's
own tag stamped into it. So the `install.sh` of a release installs
that release, and there is no "latest". Download it from the release you want, read it, then run it
in one of two modes. The examples use the tag `v0.1.0-web3`; take yours from the
[releases page](https://github.com/jonasthim/dilla/releases).

Behind a reverse proxy that terminates TLS (Pangolin, Traefik, Caddy, nginx):

```sh
curl -fsSLO https://github.com/jonasthim/dilla/releases/download/v0.1.0-web3/install.sh
link=$(sh install.sh --behind-proxy --domain chat.example.org \
  --listen 10.0.0.20:8080 --trusted-proxy 10.0.0.5/32 --public-ip 203.0.113.10)
```

With dillad terminating TLS itself on 443/tcp:

```sh
curl -fsSLO https://github.com/jonasthim/dilla/releases/download/v0.1.0-web3/install.sh
link=$(sh install.sh --public --domain chat.example.org --public-ip 203.0.113.10 --agree-tos)
```

What it does, in order:

1. It checks every argument against what dillad accepts. It refuses to run as anyone but root, and
   refuses if `/etc/dilla/dilla.toml` or a non-empty `/var/lib/dillad` already holds an instance.
2. It downloads `dillad-linux-<amd64|arm64>`, the matching `dilla-spotify-helper-linux-<amd64|arm64>`,
   `dilla_core_wasi.wasm` and `SHA256SUMS` from `releases/download/<tag>/`. It uses curl, else wget, and apt installs only
   what is missing of `curl` and `ca-certificates`. Every downloaded file is checked with
   `sha256sum -c` before anything is installed. Any of these stops it: a mismatch, an empty
   `SHA256SUMS`, or one without a line for an asset.
3. It installs `dillad`, the core beside it, the Spotify helper at `/usr/local/libexec/dilla-spotify-helper`,
   the unit, and creates the `dillad` user.
4. It runs `dillad init --config=/etc/dilla/dilla.toml` and edits that file for the mode. In both modes it sets
   `livekit.udp_port` (`--udp-port`, 7882) and `livekit.node_ip` (`--node-ip`, when given).
   - `--behind-proxy` sets:
     - `tls.mode = "behind_proxy"` and `tls.agreed = false`;
     - `server.plain_listen` from `--listen`;
     - `server.trusted_proxy_cidrs` from every `--trusted-proxy` (the flag is repeatable);
     - `turn.enabled = false`.

     It also needs `--public-ip` or `--node-ip`: the address clients send UDP 7882 to (with
     Pangolin, the Pangolin server's). `--public-ip` sets `instance.public_ip` and, without
     `--node-ip`, `livekit.node_ip`. `--no-media-ip` installs without either, but then calls cannot
     reach the SFU from outside the host until you set `livekit.node_ip`.
   - `--public`: init's `acme_tls_alpn`, as in the manual steps above.
5. It sets `/etc/dilla/dilla.toml` to 0640 root:dillad and `/var/lib/dillad` to
   0700. Then it checks the file with dillad's own config check (`dillad doctor`'s `config` leg)
   and refuses to start a file dillad would refuse.
6. It runs `systemctl enable --now dilla` and then `dillad doctor`, whose report goes to stderr.
   The `clock`, `certificate` and `turn` legs need DNS or the proxy, so they are shown as pending,
   not as a failed install. The `udp` leg reads WARN "not probed" by default and never proves the
   UDP forward, because dillad cannot see it from inside; check the forward from outside instead.
   Any other red leg fails the install.
7. It prints the next steps on stderr (the DNS record and forwards for `--public`; the Pangolin
   resource and the UDP forward for `--behind-proxy`). Then it prints the bootstrap invite link
   alone on stdout.

The link is printed on every exit after init, even a failed one, because it cannot be shown again.
It is valid for 24 hours and one sign-up: open it in a browser to make the first account, then start a server and
invite people from the web client ([Start a server](../user/getting-started.md#start-a-server)).
If a step after init fails, the script says how to start over:
`systemctl disable --now dilla; rm -rf /var/lib/dillad /etc/dilla/dilla.toml`, then run it again.

The instance keys and the TURN and LiveKit secrets stay in the files init writes; the script never
prints them. More flags:

- `--version <tag>` installs another release.
- `--release-url` points at another download location.
- `--binary`, `--core` and `--sums` install from local files, checked against that `SHA256SUMS`.
  Without `--sums`, local files are refused unless you pass `--insecure-skip-verify`.
- `--check-args` validates the arguments and changes nothing.

A copy of `install.sh` taken straight from git has no tag stamped in, so it needs `--version` or
`--release-url`.

To upgrade, download the new release's `install.sh` and run it with `--upgrade`. With an older
copy, pass the new tag with `--version`:

```sh
curl -fsSLO https://github.com/jonasthim/dilla/releases/download/v0.1.1/install.sh
sh install.sh --upgrade
```

Add `--check-args` to the same command first: it checks the arguments and changes nothing, and exits 0 when they are valid, printing `arguments are valid; the release was not checked` on stderr: it does not look up the release, so a mistyped `--version` still passes here and is refused later, at the download, before anything on disk changes.

It checks the new binary and core against `SHA256SUMS`, restarts the API while the sidecar SFU
stays up, runs doctor, then restarts the media service. The previous pair stays as `dillad.prev`
and `dilla_core_wasi.wasm.prev`. For a first upgrade from `in_process`, it saves `dilla.toml.prev`,
stops the old combined process, changes `livekit.mode` to `sidecar`, and starts the two services.
That one-time migration briefly interrupts calls. Later API restarts leave the SFU running;
media restarts end their rooms; updated community voice clients try to rejoin during the upgrade
window. The installer writes
`dilla-media.service`; it leaves an existing `dilla.service` untouched, so compare that unit with
the release copy after an upgrade.

Before it replaces anything, it reads the database's schema version with the installed binary
(`dillad migrate status`) and refuses the upgrade if it cannot. If the restart fails, it rolls
back:

1. It stops `dilla`, which systemd would otherwise keep restarting.
2. It reads the schema version again with the **previous** binary. That binary loaded this
   `dilla.toml` before the upgrade, and it reports a newer schema without refusing it. The new
   binary may have failed on the config itself, before it migrated anything.
3. If the new binary migrated the database before it failed, it takes the database back to the
   version it read first, with the **new** binary:
   `dillad migrate down --to=<that version> --i-know-what-i-am-doing`. This is the only time it
   writes to the database.
4. It puts the previous pair back and restarts.

A `dillad` refuses to start on a database newer than its own migrations. So if the second read
fails, if `dilla` cannot be stopped before a down migration, or if the down migration fails, it
does not start the previous binary. It leaves the new binary in place, says whether `dilla` is
stopped, and prints the ways out, which are the manual rollback below.

#### Rolling back by hand

`dillad.prev` alone does not start on a database that the newer binary has migrated: it exits
with "database schema N is newer than this binary's highest migration M: refusing to start". To
go back one release:

1. Stop the service: `systemctl stop dilla`.
2. Find the schema the previous binary reads. `runuser -u dillad -- /usr/local/bin/dillad.prev migrate status --config=/etc/dilla/dilla.toml`
   prints `current N, target M`. `M` is that version.
3. Take the database down to `M` with the **new** binary, which is the only one that knows the
   way down:
   `runuser -u dillad -- /usr/local/bin/dillad migrate down --to=M --i-know-what-i-am-doing --config=/etc/dilla/dilla.toml`.
   Skip this step when `N` is already `M`.
4. Put the previous pair back:
   `cp -p /usr/local/bin/dillad.prev /usr/local/bin/dillad` and
   `cp -p /usr/local/bin/dilla_core_wasi.wasm.prev /usr/local/bin/dilla_core_wasi.wasm`.
5. Start it again: `systemctl start dilla`.

Always do step 3 before step 4. An older `dillad doctor` does not catch a database left too new: the web-3.5 doctor
reports `OK database schema version 17` on a web-3.6 database. Only `serve` refuses it.

If step 3 fails on SQLite, there is a fallback: the copy `dillad serve` takes before it migrates,
`<db.path>.pre-migration-<unix time>` beside the database (`db.pre_migration_backup`, on by
default). With `dilla` stopped, remove the database's `-wal`, `-shm` and `-journal` files and
move that copy over the database file. Then do steps 4 and 5. Everything written since the upgrade is lost.

What a down migration removes is listed per release under "Client data version per release" in §5.

#### Behind Pangolin

Behind Pangolin, `--listen` is the address newt reaches in the container, and `--trusted-proxy` is
newt's own address as a `/32`. With newt in the same container, use
`--listen 127.0.0.1:8080 --trusted-proxy 127.0.0.1/32`. Any host in the trusted range can set
`X-Forwarded-For` and choose the address dillad rate-limits it by, so keep the range to the proxy
itself.

`--trusted-proxy` is repeatable: give it once per newt host, each as its own `/32`, rather than one
range that also covers other hosts. A wide range such as `10.0.0.0/8` is unsafe: anyone in it can forge
`X-Forwarded-For` and choose the client address dillad sees and rate-limits it by. With two newt
hosts, at 10.0.0.5 and 10.0.0.6:

```sh
link=$(sh install.sh --behind-proxy --domain chat.example.org --listen 10.0.0.20:8080 \
  --trusted-proxy 10.0.0.5/32 --trusted-proxy 10.0.0.6/32 --public-ip 203.0.113.10)
```

Each one becomes an entry of `server.trusted_proxy_cidrs`. Then, in Pangolin:

- Create an HTTPS resource for the domain that targets `http://<container address>:<listen port>`.
  Turn Pangolin SSO (resource authentication) off for it, because dilla signs its users in itself.
  WebSocket upgrades must pass through: Traefik passes them by default, so add no middleware that
  strips `Upgrade` or `Connection`.
- Add the raw UDP forward for 7882 described in
  [Behind a WireGuard tunnel (Pangolin)](#behind-a-wireguard-tunnel-pangolin-mtu-and-udp-7882):
  a Traefik UDP entry point with a 30-second timeout (an existing one without a `timeout` falls
  back to 3 seconds), `7882:7882/udp` published on `gerbil`, and a raw UDP resource with exactly
  one target: the container's 7882/udp on the node it runs on. The same section says how to check
  the forward (doctor's `udp` leg never proves it) and when not to recreate `traefik` and `gerbil`.

TURN is off in this mode (see [the `behind_proxy` TURN consequence](#the-behind_proxy-turn-consequence)),
so a member on a network that blocks UDP cannot join calls.

### Proxmox

`deploy/proxmox-lxc.sh` creates a new unprivileged Debian 13 container on the Proxmox host (nesting
on, 2 GiB of memory, an 8 GiB disk), pushes `install.sh` into it and runs it there. Run it as root on
the Proxmox host, from a directory that also holds a release's `install.sh`:

```sh
curl -fsSLO https://github.com/jonasthim/dilla/releases/download/v0.1.0-web3/install.sh
link=$(./proxmox-lxc.sh --domain chat.example.org --public-ip 203.0.113.10 --agree-tos)
link=$(./proxmox-lxc.sh --behind-proxy --domain chat.example.org --listen 10.0.0.20:8080 \
  --trusted-proxy 10.0.0.5/32 --public-ip 203.0.113.10)
```

stdout is the bootstrap invite link and nothing else; every progress line goes to stderr.

- **Checks first.** The arguments are checked with `install.sh --check-args`, and the release's
  `SHA256SUMS` is probed, before any container exists.
- **Release.** It installs the tag stamped into the `install.sh` beside it, or `--version`.
- **Flags.** The container flags (`--ctid`, `--hostname`, `--bridge`, `--storage`,
  `--template-storage`) stay on the host. The mode flags, `--version`, `--release-url`,
  `--udp-port`, `--node-ip` and `--insecure-skip-verify` are passed to `install.sh`.
- **Files.** `--binary`, `--core`, `--spotify-helper` and `--sums` are pushed into the container
  first. Anything not given locally is downloaded and checked inside the container.
- **Failures.** If the install fails after the container was created, the helper prints the
  `pct stop`/`pct destroy` commands that remove it.

Without `--behind-proxy`, forward 443/tcp and 7882/udp from the host to the container. A NATed
container does not hold the public IP either, so leave `turn.relay_ip = "auto"` as init wrote it:
the relay binds the container's own address.

On a container that already exists, such as one from the Proxmox community scripts, skip the helper
and run `install.sh` inside it directly.

### Dilla Bot installation

The Bot's `[rate_limits]` in `/etc/dillabot/bot.toml` also bounds its limiter
caches: `device_cache_limit` defaults to 1,024, `target_cache_limit` to 2,048
KeyPackage targets per device, and `global_target_cache_limit` to 16,384 targets
per client. Zero or absent selects the default; negative or excessively large
capacities fail configuration validation. A cache entry can be evicted only
after five minutes idle, with its allowance fully replenished and no request
waiting on it. Session renewal shares the device budget, including allowance
spent before an anonymous token was bound to the device. Full caches defer
before sending an HTTP request (`E_UNAVAILABLE`, retry after 1 second).
Aggregate cache counts, evictions and capacity-deferral counters contain no
tokens or identities; the Bot logs deferral milestones without flooding logs.


The release includes `install-bot.sh` alongside `install.sh`. Run the Bot installer as root on the
Linux host after `dillad` is installed and its instance is initialized. The installer downloads and
checksum-verifies the architecture-matched `dillabot`, native `dilla-bot-core`, and
`dilla_core_wasi.wasm`; creates the dedicated `dillabot` account; registers one protected runtime
credential through `dillad admin bot-runtime create`; writes Bot files under `/etc/dillabot` and
`/var/lib/dillabot`; runs `dillabot doctor`; and enables `dillabot.service`.
Tagged release builds report their embedded tag with `dillabot version`; development builds report
`0.0.0-dev`.

Get the lowercase hexadecimal instance ID from the `instance_id` field of the server's
`GET /v1/instance` discovery document. The server must permit discovery without a session for that
lookup. Supply the public HTTPS origin without a trailing slash:

```sh
sudo sh install-bot.sh \
  --base-url https://chat.example.org \
  --instance-id 32_LOWERCASE_HEX_CHARACTERS
```

For a checkout copy rather than the tagged release script, also pass `--version TAG` (or
`--release-url URL`). To install locally built assets, pass `--binary FILE --helper FILE --module
FILE --sums SHA256SUMS`. The script refuses to overwrite an existing Bot account, config, service,
credential, binaries or state. It creates an instance-scoped runtime credential; after it starts,
use **Server Settings > Bot** to enable the Bot for a community. If setup fails after dillad created
the runtime record, the installer prints its runtime ID; revoke that record with
`dillad admin bot-runtime revoke --id=ID --config=/etc/dilla/dilla.toml` after inspecting the
partial installation.

To upgrade, use the tagged installer on the same host with `--upgrade`:

```sh
sudo sh install-bot.sh --upgrade --version TAG
```

The upgrade keeps `bot.toml`, the runtime credential and service state. It runs the existing local
doctor, stops the Bot if it is active, saves a root-only cold copy of state and the previous
binaries/unit under `/var/backups/dillabot`, installs the checked assets, runs doctor again, and
restarts the service if it was active before. If installation, doctor or restart fails, it restores
the previous assets and state. The retained copy contains `database.key` and must be protected as a
secret. The upgrade path has not yet been exercised against a prior released Bot database schema.

Each upgrade run writes one such copy, as a new directory `/var/backups/dillabot/<UTC timestamp>-<suffix>`, root-only,
holding the whole state directory (with the database key) plus the three Bot binaries and the unit. After a successful
upgrade the installer keeps the 3 newest copies, the new one included, and removes the older ones; it logs each path
it removes and never prunes after a failed or rolled-back upgrade. `--keep-backups N` (N of at least 1) keeps N
instead. Only directories named like the installer's own copies are touched. This pruning starts with the release after
`v0.1.0-signal1`: on `v0.1.0-signal1` and earlier, remove old copies by hand, keeping the newest.

For a cold operator backup of the complete Bot files, including its config and runtime credential:

```sh
sudo install -d -m 0700 /srv/backups/dillabot
archive="/srv/backups/dillabot/dillabot-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
sudo sh -c 'set -eu
systemctl stop dillabot
trap "systemctl start dillabot" EXIT
umask 077
tar -C / -czf "$1" etc/dillabot var/lib/dillabot
tar -tzf "$1" >/dev/null
systemctl start dillabot
trap - EXIT' sh "$archive"
```

Restore only with the matching Dilla server database/runtime record available, while dillabot is
stopped. Restore as root to `/`, run the local doctor, then start the service:

```sh
sudo systemctl stop dillabot
sudo tar --same-owner -C / -xzf /srv/backups/dillabot/BACKUP.tar.gz
sudo -u dillabot dillabot doctor --config=/etc/dillabot/bot.toml
sudo systemctl start dillabot
```

This is an operator procedure, not a coordinated Bot/dillad backup and restore acceptance run.
`dillad backup` does not include Bot config, credential or state, so a server restore alone is not a
complete Bot restore. Keep the Bot archive protected: it includes the credential and database key.

`dillabot doctor` reads `/etc/dillabot/bot.toml` by default, the file the installer writes and the
unit passes to the service; it prints which config path it read. It reports a missing file
(`no such file at PATH`), an invalid config and an unusable runtime credential as three different
failures, and never prints the credential. Its YouTube tool check prints the `PATH` it searched.
The installer runs the doctor with the service's `PATH` (an `Environment=PATH=` in the unit, else
systemd's default from `systemctl show-environment`, else `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin`),
so `pct exec`, whose `PATH` lacks `/usr/local/bin`, does not report installed tools as unavailable.
Run by hand, `dillabot doctor` uses your shell's `PATH`; under `pct exec` run it as
`PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin dillabot doctor`.

`dillabot doctor --config=/etc/dillabot/bot.toml` checks the local owner-only runtime credential,
native identity helper, compiled WASI identity module, optional Spotify PCM helper and private
state directory. The installer runs it as `dillabot` before enabling the unit. It does not contact
the Dilla server or prove that an installation is reconciled; use `systemctl status dillabot` and
`journalctl -u dillabot` for process and reconciliation diagnostics. A healthy local doctor is not
qualification of Bot interactions or music.

YouTube extraction and playback need separate host tools; the Bot installer does not install them.
On Debian 13, install the system tools and a Node runtime, then install the hash-pinned Python pair
from a repository checkout:

```sh
sudo apt-get update
sudo apt-get install -y bubblewrap curl ffmpeg python3 python3-venv xz-utils
# Install Node 24.15.0, the version used by the recorded adapter smoke.
case "$(uname -m)" in x86_64) node_arch=x64 ;; aarch64) node_arch=arm64 ;; *) exit 1 ;; esac
node_version=24.15.0
node_archive="node-v${node_version}-linux-${node_arch}.tar.xz"
curl -fsSLO "https://nodejs.org/dist/v${node_version}/${node_archive}"
curl -fsSLO "https://nodejs.org/dist/v${node_version}/SHASUMS256.txt"
grep " ${node_archive}$" SHASUMS256.txt | sha256sum -c -
sudo install -d -o root -g root -m 0755 /usr/local/lib/nodejs
sudo tar -xJf "$node_archive" -C /usr/local/lib/nodejs
sudo ln -sfn "/usr/local/lib/nodejs/node-v${node_version}-linux-${node_arch}/bin/node" /usr/local/bin/node
sudo install -d -o root -g root -m 0755 /usr/local/lib/dillabot
sudo python3 -m venv /usr/local/lib/dillabot/.youtube-venv
sudo /usr/local/lib/dillabot/.youtube-venv/bin/python -m pip install \
  --require-hashes --only-binary=:all: --no-deps \
  -r experiments/bot-providers/youtube-requirements.txt
sudo ln -sfn /usr/local/lib/dillabot/.youtube-venv/bin/yt-dlp /usr/local/bin/yt-dlp
sudo -u dillabot dillabot doctor --config=/etc/dillabot/bot.toml --youtube
```

That lock pins `yt-dlp` 2026.10.21 and its matching `yt-dlp-ejs` 0.8.0 with wheel hashes.
`--youtube` checks that exact yt-dlp version, Node 22 or newer, Python 3.10 or newer, `bwrap`,
`ffmpeg`, and FFmpeg's `libopus` encoder are locally available. The recorded host smoke used Node
24.15.0, Python 3.14.8, bubblewrap 0.13.1 and FFmpeg 9.0.2. `--youtube` also runs
`yt-dlp --version` through the real isolated metadata runner to test sandbox startup without
contacting YouTube. `youtube_sandbox` reports OK or FAIL with a failure class and stderr
collapsed to one printable line and truncated to 512 bytes. It does not inspect the
installed EJS package or qualify playback;
the pinned pair must be installed together from the requirements file. YouTube remains optional,
so a missing dependency appears as a `WARN` in the normal doctor and does not block service
installation. `--youtube` makes these checks required and returns a failure if a dependency is
missing, outside the checked minimum/version, or unable to start inside the sandbox.

The earlier 2026.8.19 pin resolved a video and captured a bounded PCM sample on 2026-10-22, but a
later integrated Bot run found yt-dlp 2025.09.26 failed and a direct test with 2026.10.21 succeeded.
The lock now uses 2026.10.21 with EJS 0.8.0, which its upstream project metadata also specifies.
The direct test does not establish cross-server or full provider qualification; keep YouTube music
reported as unavailable until its intended-host acceptance is complete.

The server `install.sh` also places the architecture-matched Spotify PCM helper at
`/usr/local/libexec/dilla-spotify-helper`. Enter the public Spotify application client ID in
community Bot settings and register the callback URL shown there in the Spotify application
dashboard. Linking works without the PCM helper; playback remains unavailable until it is
installed and configured. To enable PCM playback, configure the absolute
`spotify_helper_executable = "/usr/local/libexec/dilla-spotify-helper"` path. Spotify authorization
remains in Dilla server settings. The helper uses librespot's native TLS backend and needs the
OpenSSL 3 runtime libraries (`libssl.so.3` and `libcrypto.so.3`). On Debian 13, install
`libssl3t64` and confirm `ldd /usr/local/libexec/dilla-spotify-helper` has no `not found` entries
before enabling playback. The child receives its short-lived access token through inherited
file descriptor 3, while its arguments contain only the canonical track URI. This wiring and its
synthetic subprocess tests do not qualify real Spotify playback. Do not put provider credentials in
command arguments or logs.

Per-installation state includes `database.key` and an encrypted SQLite database with possible
WAL/journal sidecars under `/var/lib/dillabot`. Keep the key and database files together; losing the
key makes that local state unreadable, while replacing it beside existing state is refused.

For a registration whose server response is uncertain, the Bot retains the exact pending native
group and stops automatic retries. In a development installation, stop `dillabot` before running
the offline recovery command because the helper holds an exclusive state lock:

```sh
sudo systemctl stop dillabot
sudo -u dillabot dillabot interaction-recovery --config=/etc/dillabot/bot.toml
sudo -u dillabot dillabot interaction-recovery --config=/etc/dillabot/bot.toml --installation INSTALLATION_ID --recover GROUP_ID
# If the exact receipt is absent, retry the same staged group with fresh admission:
sudo -u dillabot dillabot interaction-recovery --config=/etc/dillabot/bot.toml --installation INSTALLATION_ID --readmit GROUP_ID
sudo systemctl start dillabot
```

The first command lists only identifiers, a registration hash, and server status. The recovery
`--recover` succeeds only when the server returns a receipt matching the stored operation, group,
registration body hash, and generation. If the receipt is absent, `--readmit` obtains a fresh
admission and signed context, verifies the staged local MLS group, and submits the original
operation ID, group ID, and registration body. A changed identity, policy, generation, roster, or
missing KeyPackage refuses the retry. Keep the bot stopped during both commands and restart it
even if a retry fails. Do not discard the staged group or reissue a registration by hand. This is
an operator repair path, not a completed installation or backup workflow.

## 4. Backup and restore

Backups hold no end-to-end-encrypted plaintext: they contain ciphertext, server-readable channel content, revealed report envelopes, TLS material and the instance keys.

That sentence is the whole threat model of the archive. It cannot be read as messages from
end-to-end-encrypted conversations, but it does hold everything that lets someone impersonate the
instance (the TLS material and the instance keys) and the content of every channel the server can
read. Store it like a private key.

```sh
# Take a backup, as the user that owns the data directory. It may run beside `dillad serve`.
sudo -u dillad dillad backup --config=/etc/dilla/dilla.toml --include-blobs --out=/srv/backups/dilla-$(date -u +%F).tar.gz

# Verify it without writing anything: the manifest and every member's size and SHA-256.
dillad backup verify --from=/srv/backups/dilla-2026-10-01.tar.gz

# Restore. Stop the service first: a restore refuses while a serve or a backup holds the directory.
sudo systemctl stop dilla
sudo -u dillad dillad restore --config=/etc/dilla/dilla.toml --from=/srv/backups/dilla-2026-10-01.tar.gz --dry-run
sudo -u dillad dillad restore --config=/etc/dilla/dilla.toml --from=/srv/backups/dilla-2026-10-01.tar.gz
sudo systemctl start dilla
```

`dillad backup verify --from=<archive>` is the integrity check to run on every archive before you rely on it,
and again after copying it off the host. It reads the archive without writing anything, checks each member
against the SHA-256 in the manifest, and on a damaged or truncated archive prints the failure and exits 65
(`dillad restore --dry-run` runs the same check). Put it after the backup in whatever schedules your backups, and
alert on a non-zero exit.

`--include-blobs` is the default; `--include-blobs=false` leaves the attachment files out. A backup
with a missing blob file is refused unless `--allow-gaps` names the gaps in its manifest, and
`restore` refuses such an archive, or one from another instance, without `--force`.

Run `restore --dry-run` first, then without it. The dry run verifies the archive end to end and
prints what the restore would do, writing nothing. The real restore keeps the entries it replaces
in `<data_dir>/.old-<hex>` unless you pass `--remove-old`.

### The 24-hour heal window

A restored database is older than what the clients remember, so every group on it is
epoch-unknown. The restore ends every live call, purges unused KeyPackages, and bumps the
instance generation so each client notices and resyncs. A member then heals a group by uploading
its member-signed GroupInfo and handshake tail. The window for that is 24 hours from the next
`dillad serve` start: a group that has heard from no member by then is closed, and the channel
owner's device re-creates it. Start the instance promptly after a restore and expect clients to
show a resync notice while they catch up.

## 5. Operating a required second factor

From web-3.6 every human account needs a second factor: a confirmed authenticator app (TOTP) or a passkey. The
instance enforces it: an account without one is refused by every route and by the gateway except the ones that set a
factor up, until it adds one. This section is what the operator does about that. What people see is in the
[user guide](../user/getting-started.md#second-factor).

### What happens when you upgrade

- The migration (`00017_second_factor`) adds `users.factor_owed` and marks every live human account that holds no
  confirmed TOTP secret and no passkey. Those people are not signed out; every browser of theirs stops at "Add a
  second factor" until they add one. Tell your members before you upgrade, and ask them to open Settings → Sign-in
  afterwards.
- **Reload every open dilla tab after upgrading to web-3.6.** A tab loaded from web-3.5 does not know the new
  refusal. For an account that owes a factor, it shows "Connection lost. Reconnecting…" indefinitely, and a send shows
  a raw `E_FACTOR_REQUIRED`. A reload loads the web-3.6 client, which shows "Add a second factor". Tell your members.
- At every start, before it serves, `dillad serve` reconciles the flag both ways. It marks any live human account
  that holds no usable factor and logs "accounts without a usable second factor now owe one", and it clears the flag
  from any marked account that holds a usable factor and logs "accounts that hold a usable second factor no longer
  owe one". A usable passkey is one registered under the current `auth.webauthn.rp_id`, so changing `rp_id` (below)
  makes every account whose only factor is a passkey owe a new one, and so does restoring an archive made before
  web-3.6. **If you changed `rp_id` by mistake, put the old value back and restart:** the passkeys count again and
  the flag is cleared. Adding a factor clears the flag at once, and so does a browser reading its factors while the
  account holds one.
- There is no switch that turns the requirement off.
- `dillad admin user list` has a `factors` column between `flags` and `status`. It reads `totp`, `passkey×N` (the
  passkeys registered under the current `rp_id`), `codes:N` (unused backup codes) and `owed`, in that order and
  comma-separated, or `-` for an account with none. An authenticator app that was never confirmed does not count.
  `owed` marks the accounts that meet the forced set-up screen at their next use: `totp,codes:10` is an account in
  good order, `owed` or `codes:10,owed` one that must still add a factor. `dillad admin user show` adds the `totp`,
  `passkeys`, `backup_codes` and `factor_owed` lines.

### The configuration it needs

- **`auth.methods`** must include `totp` or `passkey` (the default is `password`, `totp`, `passkey`). With
  neither, the configuration is refused, because no account could ever hold a factor. `dillad doctor` warns if
  `totp` is missing, since passkeys are then the only factor and a passkey that stops working locks a new browser out.
- **`auth.totp.recovery_codes`** is how many backup codes a person gets, from 1 to 16 (default 10). It is
  checked at start.
- **`auth.webauthn.rp_id`** defaults to `instance.domain`. It must be a domain name, never an IP address, and
  **a passkey is bound to it for good**: change it and the passkeys already registered stop counting.
- **`auth.webauthn.rp_origins`** lists the origins a browser may use, and defaults to `https://<instance.domain>` plus
  `http.trusted_origins`. The browser's origin includes its port, and the host of each `http` or `https` entry
  must be `rp_id` or a subdomain of it.

Behind a proxy (`tls.mode = "behind_proxy"`) the address the browser uses is the proxy's, so set both explicitly when
the public address is not `https://<instance.domain>` on 443. An instance reached at `https://chat.example.org:8443`:

```toml
[auth.webauthn]
rp_id = "chat.example.org"
rp_origins = ["https://chat.example.org:8443"]
```

`rp_id` has no port; the origin has one. If the browser's origin is not in `rp_origins`, passkeys fail and the
client can say "Passkeys do not work at this address of {instance}. Use an authenticator app, and tell the operator."
Authenticator apps keep working.

### The `webauthn` leg of `dillad doctor`

`dillad doctor` reads the passkey settings and contacts nothing. It reads red when `rp_id` is an IP address or an
`rp_origins` entry's host is neither `rp_id` nor a subdomain of it. It reads WARN when `auth.methods` lacks `totp`,
and **with `tls.mode = "behind_proxy"` it reads WARN only when no `rp_origins` entry is the public origin**: an
`https` origin on the instance domain (compared without case) with no port or port 443. Add the origin the browser
shows, and the warning goes. Otherwise it reads OK with the `rp_id` and the number of origins.

The report ends with one summary line, printed after the legs unless you pass `--quiet`:

```text
WARN  summary      9 checks: 7 OK, 1 WARN, 1 FAIL
```

The first word is the worst status of the run (OK, WARN or FAIL) and the counts follow. The exit code does not change,
and a script that compares the legs' text is unaffected, because the line is not part of it.

### A person has lost their factor: `dillad admin user reset-factors`

```sh
sudo -u dillad dillad admin user reset-factors --config=/etc/dilla/dilla.toml --username=mira --yes
```

It takes `--id=ID` or `--username=NAME`, and `--yes` (it refuses without it, because it cannot be undone). It
removes the person's authenticator app, every passkey and every backup code, marks the account as owing a factor and
writes a `user.factors_reset` audit row, all in one transaction. A bot account and a deleted account are refused.

- **Verify the person first, out of band.** The reset removes the check that stands between a stolen password and a
  new browser: whoever signs in next with the password and the recovery key sets up a new factor. Confirm it is
  them in some other way than dilla (a voice call, in person). If the account may be compromised, **disable it
  instead** (`dillad admin user disable`), which deletes its sessions, and sort out the account before you enable it.
- **Sessions are kept.** The person is not signed out, and each of their browsers meets "Add a second factor" before
  it does anything else. Their recovery key still works, so they can also sign in on a new browser with the password
  and the recovery key and set up a factor there.
- **Live connections close within one heartbeat.** `dillad admin` is another process from `dillad serve`, so it
  closes no socket itself; the serving process notices the owed account on its next liveness tick and closes the
  connection. The heartbeat interval is `gateway.heartbeat_interval` (30 s by default).

### A person has lost every device: `dillad admin device revoke --all`

```sh
sudo -u dillad dillad admin device revoke --all --username=mira --config=/etc/dilla/dilla.toml
```

It takes exactly one of `--username=NAME` or `--id=ID` (the user's id, 32 hex), the same selectors as the other user
verbs. It first prints the resolved user, its id and its number of active devices. It then asks you to type the
username back, and revokes only if the answer matches. Pass `--yes` to skip the question; without `--yes` and without a
terminal to answer on, it refuses. It revokes every device that is not yet revoked, deletes every session of the
person and writes a `device.revoke_all` audit row, all in one transaction under the user lock. A device revoked before
keeps its first revoke time. A deleted account is refused, and an account with no active device is reported and left
alone. It prints each device it revoked.

- **Encrypted history is lost** unless another device still holds it, and after this none of the person's devices is
  active. The person signs in again to use dilla.
- **The signed device list is not changed.** The server cannot sign it, so the list clients read still names the
  revoked devices (marked revoked) until the person's own device removes them.
- **Live connections close within one heartbeat**, as with `device revoke --id`: the serving process notices that the
  sessions are gone on its next liveness tick.
- **Use `--id` for one device.** `device revoke --id=ID` revokes a single device and leaves the others signed in.

### Changing the instance domain

Moving an instance to a new address is a configuration change with consequences for people. Read them before you start.

- **Passkeys.** A passkey is bound to `auth.webauthn.rp_id`, so after the change no existing passkey works. Every
  account then signs in with its password plus an authenticator code or a backup code, and can register a new passkey in
  Settings → Sign-in. An account whose only factor was a passkey needs a backup code, or an operator
  `dillad admin user reset-factors` ([above](#a-person-has-lost-their-factor-dillad-admin-user-reset-factors)). At the
  next start `dillad serve` marks such accounts as owing a factor.
- **Browsers.** The browser store is per origin, so every browser starts fresh at the new address. Signing in there
  needs the password, the second factor and the recovery key. Messages from before the move are not shown in a browser
  that joins after it.
- **Invites.** Links on the old domain stop working; make new ones.
- **A disposable instance** can be wiped and initialised again instead.

**The keys to change in `/etc/dilla/dilla.toml`.** `dillad init` writes the values it derives from the domain out
explicitly (`internal/config/defaults.go`), so changing `[instance] domain` alone changes none of them. Edit each:

- `[instance] domain`.
- `[auth.webauthn]`: `rp_id`, `rp_origins` (`https://<new domain>`, plus any `[http] trusted_origins` entry that names the
  old origin) and `rp_display_name`.
- `[auth.totp] issuer`. This is cosmetic: existing authenticator entries keep the old label, and their codes still work.
- `[auth.oidc] redirect_url`, if OIDC is used.
- `[turn] realm` and `[livekit] stun_servers` (`<new domain>:3478`), which init derives from the domain too.
- DNS for the new name. In the `acme_*` modes the certificate follows the new name.

**The check.** Restart, then run `dillad doctor --config=/etc/dilla/dilla.toml`. The `webauthn` leg reads
`rp_id <new domain>, N origins` with status OK. A WARN or FAIL there names the key to fix.

This cannot be undone for passkeys. Moving back to the old domain does not bring back the passkeys registered on the
new one, and putting the old `rp_id` back brings back only the ones registered under it.

### Client data version per release

The web client keeps its data in the browser, in a store with a schema number. A client refuses a store whose number
is newer than the one it knows (web-3.6 onwards) and shows "This version of dilla is too old for the data saved in
this browser" with both numbers: "That data needs data version {found}; this version of dilla reads up to
{supported}. Update to the newer version of dilla, or ask the host of this server."

| Release | Store schema (browser) | Database schema (dillad) | Note |
|---|---|---|---|
| web-3.4 | 4 | 16 | |
| web-3.5 | 4 | 16 | Three additive tables in the browser store |
| web-3.6 | 4 | 17 | A client now refuses a store newer than it knows; migration `00017_second_factor` |
| web-3.7 | 5 | 18 | One-way: a browser that opened web-3.7 shows the "too old" screen if the server is rolled back to web-3.6; redeploy web-3.7, or clear site data and sign in again with the recovery key. Migration `00018_message_class` |
| web-3.8 (`v0.1.0-web3.8`) | 5 | 20 | No change to the browser store. Migrations `00019_presence` and `00020_afk` came in from the noise betas; web-3.8 adds none. Hello offers wire version 2 only |
| web-3.9 (`v0.1.0-web3.9`) | 5 | 20 | No change to the browser store or the server schema. dillad adds an "AFK" voice channel to every server at its first start. |
| web3.10 and web3.10.1 | 5 | 20 | No change to the browser store or the server schema. |
| web3.11 (`v0.1.0-web3.11`) | 5 | **21** | Migration `00021_afk_away` (the away roster). Rolling back to web3.10.1 needs `migrate down --to=20` with the web3.11 binary first; see the [web3.11 release notes](releases/v0.1.0-web3.11.md). |
| web3.12 (`v0.1.0-web3.12`) | 5 | **23** | Migrations `00022_profile` (`users.status_msg`, `users.bio`) and `00023_device_labels` (`devices.label`). Rolling back to web3.11 needs `migrate down --to=21` with the web3.12 binary first, and loses every status message, bio and device name; see the [web3.12 release notes](releases/v0.1.0-web3.12.md). |
| bot1 (`v0.1.0-bot1`) | **12** | **47** | Dilla Bot and its server/browser support. A rollback to web3.12.4 needs a server down migration to 23 with the new binary first; the older browser cannot read a schema-12 store. See the [bot1 release notes](releases/v0.1.0-bot1.md). |
| Signal wave A (wire 4) | **13** | **52** | Server settings (task 10c): `00048_control_music` turns every DJ role into a role with the **Control music** permission and drops the DJ roles table (its down migration recreates the table empty, so a rollback loses the DJ list; the permission bits stay on the roles), `00049_unlimited_invites` and `00050_bot_installation_settings` (Polls and Reminders on for every existing Bot, default music volume and idle time, restart and version). The instance-signed account kind (task S1, protocol/03 § Account kind attestation): `00051_session_wire4` lets a session record wire 4. `00052_password_changed` (Task 11) adds `password_credentials.changed_at`, set by a password change, so Settings → Sign-in can say "Changed {when}"; existing passwords read as unknown (its down migration drops the column). **Do not roll `dillad` back below a wire-4 build**: a browser that has stored an attestation leaves every person without one pending, so after a rollback it stops showing other people's messages. One-way for the browser too: an older bundle refuses the schema-13 store ("too old" screen); redeploy the wire-4 build, or clear site data and sign in again with the recovery key. |

**The browser side, web-3.4 to web-3.6.** The store schema is the same in all three (4: no change
from web-3.5 to web-3.6), so a browser keeps working across a server rollback between these releases, and a person is
not asked for anything new.

**The browser side, web-3.7 is one-way.** A browser that has opened web-3.7 has moved its store to schema 5. If you
then roll the server back to web-3.6, that browser shows the "too old" screen above, with data version 5 found and 4
supported, and does not open the app. Redeploy web-3.7 and it opens again. If you must stay on web-3.6, each person
clears this site's data in the browser and signs in again with their recovery key: the messages that browser held are
gone from it, and a browser that never opened web-3.7 is not affected. Tell your members before you roll back.

**The server side is not automatic.** `dillad serve` refuses to start on a database schema newer than its own, so
the web-3.5 binary (`dillad.prev`) does not start on a database that web-3.6 has migrated to 17. Migrate down first:
use the procedure in §3, "Rolling back by hand". Stop dilla, run `dillad migrate down --to=16
--i-know-what-i-am-doing` with the web-3.6 binary, put `dillad.prev` back and start it. `install.sh --upgrade` does
the same by itself when the new binary fails to start. Do not rely on the web-3.5 `dillad doctor` to catch a
missed step: it reports `OK database schema version 17` and does not say that the schema is newer than it knows.

Going down from 17 to 16 drops only the `users.factor_owed` column. The second-factor tables (TOTP secrets, passkeys,
backup codes) are older than web-3.6 and are kept with their rows. web-3.5 then runs on the data as it did before,
with these differences:

- It no longer requires a second factor. An account without one signs in and works without one.
- A person who set up an authenticator app is still asked for its code at sign-in.
- It has no backup-code sign-in, so someone who has lost the app cannot sign in with a backup code until you upgrade
  again.

If you upgrade to web-3.6 again, migration 00017 again marks every account that holds no factor.

### Upgrading to web-3.7: migration 00018

Migration `00018_message_class` adds a nullable `class` column to the stored messages and a small table, `slowmode_marks`,
that holds the time of each person's last post in a channel with slow mode. It changes no existing row. From web-3.7 the
host refuses a post from a person without **send messages** and enforces slow mode (a refused post says how long to
wait), in server channels; DMs are not checked. A page that a person loaded before the upgrade keeps working but does
not know slow mode until it reloads (it skips the wait, and the host still checks who may post), so ask people to
reload their tabs.

To go back to web-3.6 run the rollback by hand above with `--to=17`. Going down from 18 to 17 drops the `class` column and
the `slowmode_marks` table and keeps every message. The browser side is one-way, as in the table above.

### Upgrading to web-3.8: wire version 2

web-3.8 adds no migration and no browser-store change. The instance now offers **wire version 2 only**, and the browser
client identifies with it: a client from web-3.7.x or earlier is refused with `E_VERSION` (close 4006), and an upload in the
old two-element form is refused with `400 E_INVALID_REQUEST`. `/v1/instance` lists `wire_versions [2]`.

What people see: a tab left open from before the upgrade shows "needs a newer client" with a **Reload**, which keeps the
draft. Nothing is lost silently, but ask people to reload their tabs, and tell them that any tab loaded from web-3.7 or
earlier cannot work until it does.

When you check that the served client files all load, skip one name: `dilla_core_wasm_bg.js`, which appears inside the
client chunk, is the name of the wasm module's import (`WebAssembly.Module.imports` lists only
`./dilla_core_wasm_bg.js`), not a file. Nothing fetches it, and it is not on the server, so a 404 check for it is a false
alarm.

### Upgrading to web-3.9: the AFK channel

web-3.9 adds no migration and no browser-store change. The first time the new `dillad` starts, it gives every server that
has no built-in AFK channel one: a voice channel called "AFK", listen-only (everyone in it is muted), which a server
manager can rename and move but not delete. The start logs one line, `afk backfill` with the number of servers it made a
channel for, and a second start changes nothing. New servers get the channel when they are made.

A server that chose its own AFK channel in web-3.8 keeps its inactive timeout. That channel stays where it is, with its
name and permissions, and becomes an ordinary voice channel: it no longer moves people and is no longer silent. The
server's AFK channel is the new built-in one, and there is no longer a picker for it (server settings → overview holds
the inactive timeout only). A manager who wants the old channel gone deletes it like any other.

Rolling back to web-3.8 leaves the new "AFK" channel in place, and web-3.8 treats it as the server's chosen AFK channel.

Ask people to reload their tabs after the upgrade, as after any release: a tab from before it does not know the AFK
channel's rules until it reloads.

### The optional GIF picker: a Giphy key per server

GIFs need no configuration in `dilla.toml`. A person who manages a server enters that server's own Giphy API key in
server settings → overview ("GIFs (Giphy)"; get a key at developers.giphy.com), and that server gets a **gif** button in
its channels. A server without a key has none; there are no GIFs in direct messages (a known limit). The key is
**write-only**: the page shows dots afterwards, with **replace** and **remove**, and no route, log line, audit row or
`dillad doctor` line ever contains it. There is no migration: the key is kept in the instance's settings table
(`instance_settings`, key `community_gifs:{server id}`) as plain text, like the TOTP secrets, so a database backup holds
it and should be protected as it is. Deleting a server forgets its key. Setting, replacing or removing a key, and
changing the rating, are audited (`community.gifs.key_set`, `community.gifs.key_remove`, `community.gifs.rating`) without
the key.

What this means for privacy:

- Search and the picked GIF go through `dillad`, so **Giphy sees your server, never the people on it**. `dillad` sees what
  people search for, which GIF they picked and when, and can probably link a pick to the attachment that follows by its
  timing and size; it cannot read the message.
- A picked GIF is downloaded by `dillad` and sent as an ordinary encrypted attachment, so **no reader's browser
  contacts giphy.com**. The server's content-security policy needs no Giphy host.
- `dillad` fetches only from Giphy's own hosts (`api.giphy.com` and `media*.giphy.com`) over HTTPS, builds each URL
  itself from a checked Giphy id, refuses redirects off that list and refuses to connect to a non-public address
  (including private, loopback, link-local, carrier-grade NAT, documentation, 6to4, Teredo and NAT64 ranges). It uses no
  proxy: the host needs outbound HTTPS to `*.giphy.com`. An outage at Giphy shows people "could not reach the gif
  service" and nothing else.
- A GIF is capped at 25 MB (a preview at 5 MB) counted on the bytes received; a fetch times out after 15 seconds. Each
  fetch holds one of **eight** instance-wide slots (at most **three** per person and **four** per server) from before the request to Giphy until
  the reply has been written to the person's browser, and a reply that cannot be written within 30 seconds is dropped, so
  the memory a slow or silent client can pin is bounded by eight GIFs. A request waits up to two seconds for a slot, then
  gets `503` with `Retry-After`.
- Search is limited per person, across all the servers they are in (10 at once, then 1 a second), and so is the media route
  (60 at once, then 20 a second).
- The rating (g, pg, pg-13 by default, r) filters **search**; the media route fetches any Giphy id a member asks for, so
  the rating is not a hard limit on what a member can attach (they can attach any file anyway).

Rolling back to a release without GIFs leaves the `community_gifs:*` settings rows in the table, which nothing reads.

### Upgrading with profile text: migration 00022

**Release note.** New display names, status messages and bios refuse control, bidi, zero-width and other invisible characters
(a joiner is kept where Persian, Sinhala, Devanagari and similar scripts, and emoji, need it); existing display names are kept
as they are, and sign-up, rename and `PATCH /v1/accounts/me` apply the rule only to a new or changed name.

Migration `00022_profile` adds two columns to `users`, `status_msg` (at most 190 characters) and `bio` (at most 500), both
empty for every existing account. The server stores them readable, like the display name: they are not end-to-end
encrypted, and the operator can read them. `PATCH /v1/accounts/me` now writes them (it answered 501 before), and
`GET /v1/accounts/me` and the member list answer them as their last two elements. A client from before this change reads
the extra elements as absent and keeps working; ask people to reload their tabs to see the new Settings → profile.

To go back run the rollback by hand above with `--to=21`. Going down from 22 drops `status_msg` and `bio` and discards every status message and bio; the accounts are kept.

### Device names (schema 23)

Migration `00023_device_labels` adds a nullable `label` column to `devices`, empty for every existing device. A person
names a device in Settings → Devices (`PATCH /v1/devices/{device_id}`). The name is not part of the signed device list: the
server stores it readable and could read or change it, so a client always shows the first characters of the device id
beside it. It follows the same text rule as a display name (at most 64 characters, one line, no control or invisible
character).

To go back run the rollback by hand above with `--to=22`. Going down from 23 drops `devices.label` and discards every device name; the devices are kept.

## Capacity

Every figure in this section is a measured cell from `docs/spikes/2026-10-capacity.md` (SP-29), on the
hardware and commit that document names; nothing here is an estimate.

The founder-LXC measurement is pending (NV-7): no LAN figure is published yet. Loopback numbers from a
development machine are not capacity figures and are not quoted here.
