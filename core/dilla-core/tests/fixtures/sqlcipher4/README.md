# SQLCipher 4 fixtures (Task 6b)

Real files written by the native **SQLCipher** build, before the native store moved to SQLite3
Multiple Ciphers. They prove that stores already on disk (deployed Bots on CT 144) keep opening
after the switch: `core/dilla-core/tests/native_store.rs` reads the files here, and
`core/dilla-bot-core/src/store.rs` (`a_store_the_sqlcipher_build_wrote_opens_and_stays_sqlcipher`)
reads the Bot stores in `core/dilla-bot-core/tests/fixtures/sqlcipher4/`.

## How they were made

On commit `2b3e271056191a81d634aac2602323f2f22ef734` ("Merge Task 8: gateway client"), whose
native store is rusqlite 0.41.1 `bundled-sqlcipher-vendored-openssl` (SQLCipher 4.20.0 on SQLite
3.54.1, the vendored libsqlite3-sys of that commit), rustc 1.98.1, Linux x86_64:

```sh
git apply core/dilla-core/tests/fixtures/sqlcipher4/generate.patch
cd core/dilla-bot-core
cargo test --lib generate_sqlcipher4_fixtures -- --ignored
cargo test --lib check_sqlcipher4_fixtures_on_old_build -- --ignored --nocapture
git checkout src/store.rs
```

`generate.patch` adds the generator and a check that the same old build reads the files back. It
only works on that commit (or any other SQLCipher build); the current build no longer links
SQLCipher. Re-running it gives different bytes (random salts and nonces), so the files are kept,
not regenerated.

## Files

All keyed with a raw key in SQLCipher's form, `PRAGMA key = "x'<64 hex>'"`, SQLCipher 4 defaults
(4096-byte pages, HMAC-SHA512), exactly as dilla-bot-core's `open_keyed` did.

| file | key | contents |
|------|-----|----------|
| `legacy.db` | `000102…1f` (bytes 0x00..=0x1f) | rollback journal; table `legacy(id, label, payload)` with rows 1 `legacy-plaintext-marker` / `x'00010203'`, 2 `second row` / 3000 zero bytes, 3 `third row` / `x'ff'` |
| `legacy-wal.db` + `legacy-wal.db-wal` | same | WAL mode; table `early` (`checkpointed`) is in the database file, table `legacy` (same rows) only in the WAL, copied while the writer was open: a hot WAL, as a crash leaves it |
| `../../../../dilla-bot-core/tests/fixtures/sqlcipher4/clean/state.db` | `41` × 32 | a store made by `Store::open(dir, [1;16], [2;16], [3;16], &[0x41;32])`, plus `bot_state` row area 1, key `fixture-key`, value `legacy-bot-value`; closed cleanly |
| `…/sqlcipher4/hot-wal/state.db` + `state.db-wal` | `41` × 32 | the same, copied while the store was open: the row is only in the WAL |

None of the files contains `SQLite format 3`, the marker strings or table names in plaintext.

SHA-256:

```
52c4f73150df26c151ac9a11be32e0afc62fd5d5279e2502c58f31c17f6bc35e  legacy.db
c38e3a0df828063c023925d9906b3a0d0c9b723bc1cdf587ddee0af468abf58e  legacy-wal.db
fc98f515542d112e10393fbeec94379ea953351062231806d1970b872f0a3ea3  legacy-wal.db-wal
6f8dd6d642e96fcce97d21c538a6a2b7aab01038334c534bd1a58b364832bb58  dilla-bot-core/…/clean/state.db
f80324effca06748dbfbc3cf75c1ca91dcd2121d38ef5a44c7fbfee96f84b7b7  dilla-bot-core/…/hot-wal/state.db
6b2ee086c7a37e323679141168e64f8528eb40ca4abfcb5764c11fe25cab68f6  dilla-bot-core/…/hot-wal/state.db-wal
```

The keys are test keys, committed on purpose.
