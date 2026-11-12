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
pages stay out of swap. `LimitMEMLOCK` is gone from the unit. The installers write the new unit when you upgrade.
This needs systemd with cgroup v2, which is what the supported Debian setup uses.
