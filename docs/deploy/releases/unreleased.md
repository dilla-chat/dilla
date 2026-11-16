# dilla, unreleased

A draft for the next release. It describes what changes for someone who runs the Bot on their own server. Rename it to
the release's tag when the tag is cut.

## The Bot's store moved to SQLite3 Multiple Ciphers

The Bot keeps its state in an encrypted database. This build opens and writes that database with SQLite3 Multiple Ciphers
instead of SQLCipher. Nothing needs doing when you upgrade, but a few things are worth knowing.

- **An existing Bot store keeps opening.** A store made by an earlier release (SQLCipher 4) is read as it is and stays a
  SQLCipher 4 store. It is not converted.
- **A new Bot store is encrypted with ChaCha20.** Stores this build creates use ChaCha20. An older `dillad` or Bot cannot
  open them.
- **Going back to an older release fails closed.** If you downgrade after this build has created a Bot store, the older
  Bot refuses to open it. It does not start with an empty store and does not damage the file. Upgrade again to use the
  store, or remove the Bot's state to start over.
- **An interrupted setup from an older build stops the Bot.** If an earlier build was stopped in the middle of its first
  setup and left an unfinished write behind (a hot rollback journal), the Bot refuses to start instead of guessing. Start
  the older build once so it can finish, or remove the Bot's state and set the Bot up again.

## Memory hardening

The new store cannot lock its memory pages, so the service no longer asks for that. `dilla.service` sets
`MemorySwapMax=0` instead: the unit, and the Bot it runs, can never be written to swap, so the store key and decrypted
pages stay out of swap. `LimitMEMLOCK=256M` is gone from the unit, so the locked-memory limit drops to systemd's default
of 8 MiB: a trade, not an addition. This needs systemd with cgroup v2, which is what the supported Debian setup uses.

**Upgrade `dillad` and the Bot together.** Only `install-bot.sh --upgrade` writes the new `dilla.service`;
`install.sh --upgrade` leaves the old unit and logs `dilla.service was not rewritten`. Run both installers of this
release, in this order:

```sh
sudo sh install.sh --upgrade
sudo sh install-bot.sh --upgrade
```

## Installer changes

- `install-bot.sh` keeps the Bot binaries it replaces as `/usr/local/bin/dillabot.prev` and
  `/usr/local/lib/dilla/dilla-bot-core.prev`, as `install.sh` keeps `dillad.prev`.
- Both installers log `no live rooms, so restarting dilla ends no call` when the live-call check passes, so a run shows
  the check ran.
- The Bot's journal line `bot channel group needs the Bot re-added: no Welcome to join it from` now reads
  `bot channel group needs the Bot re-added: no Welcome on this pass`. Its meaning is unchanged: no Welcome was found on
  that pass, and it can precede a successful `bot joined Bot channel group`.
