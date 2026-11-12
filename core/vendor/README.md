# Vendored crates

Third-party crates that dilla builds from a patched copy, through `[patch.crates-io]` in the root
`Cargo.toml`. Each copy is the published crate as it was downloaded from crates.io, minus the
registry's `.cargo-ok` and `Cargo.lock`, plus the changes listed below. Every change is marked
`DILLA PATCH` in the source. The directory is excluded from the workspace, so `cargo fmt`, `cargo
clippy` and the workspace lints do not touch it.

## libsqlite3-sys 0.39.1

Upstream: <https://github.com/rusqlite/rusqlite> (`libsqlite3-sys/`; `.cargo_vcs_info.json` has the
commit). MIT.

Two changes. The second is what the native store builds with today.

### 2. `bundled-sqlite3mc`: SQLite3 Multiple Ciphers, no OpenSSL (Task 6b, 2026-11-12)

A new feature, `bundled-sqlite3mc = ["bundled", "dep:sqlite3mc-src"]`, with the build-dependency
`sqlite3mc-src = "=206.1.0"` (SQLite3MC 2.6.1 on SQLite 3.54.0; MIT, with the CC0-1.0 / Unlicense
parts `deny.toml` already allows for that crate). It is the same release, from the same crate,
that the browser core compiles through sqlite-wasm-rs 0.6.3, so the native and browser stores are
one cipher implementation. Edits, all marked `DILLA PATCH`:

- `Cargo.toml` and `Cargo.toml.orig`: the feature and the optional build-dependency.
- `build.rs`:
  - a `compile_error!` when `bundled-sqlcipher` and `bundled-sqlite3mc` are both on (one SQLite
    per binary);
  - `build_bundled` compiles `sqlite3mc_src::source_dir()/sqlite3mc_amalgamation.c` instead of
    `sqlite3/sqlite3.c`, with the usual bundled flags plus `-DCODEC_TYPE=CODEC_TYPE_CHACHA20` (the
    default cipher), `-DSQLITE3MC_SECURE_MEMORY=1` (makes `PRAGMA memory_security = fill`
    available: freed SQLite memory is wiped; sqlite3mc has no mlock), `-DSQLITE_TEMP_STORE=2` and
    `-DHAVE_STDINT_H=1`. The crypto is sqlite3mc's own C code; nothing links OpenSSL or a system
    crypto library (`cargo tree -i openssl-sys` is empty).

Rusqlite has no such feature, so each native crate that opens a store (`dilla-core`,
`dilla-bot-core`, `clients/desktop/dilla-client`, `testkit`) asks for
`rusqlite = { features = ["bundled"] }` plus `libsqlite3-sys = { features = ["bundled-sqlite3mc"] }`,
and feature unification hands rusqlite that one copy.

Bindings: the committed `sqlite3/bindgen_bundled_version.rs` (SQLite 3.54.1 headers) is used; the
library is 3.54.0. Patch releases add no API, so the bindings are sound; `buildtime_bindgen` is not
supported with this feature (the amalgamation's header is `sqlite3mc_amalgamation.h`, not
`sqlite3.h`). `DEP_SQLITE3_INCLUDE` points at the sqlite3mc directory.

Why: dilla-core's SQLCipher linked vendored OpenSSL, and LiveKit's prebuilt libwebrtc carries
BoringSSL with an incomplete rename list, so the desktop client could not link dilla-core and
LiveKit into one binary (208 duplicate symbols; Jonas's ruling R14). Regression tests:
`core/dilla-core/tests/sqlite3mc_build.rs` (sqlite3mc linked, SQLite >= 3.54.0, no
openssl-sys / sqlcipher-src in Cargo.lock), `core/dilla-core/tests/native_store.rs` (ChaCha20
stores, and SQLCipher 4 files from the old build still open) and
`clients/desktop/dilla-media/tests/link.rs` (one binary uses both).

RUSTSEC-2026-0459 is fixed in SQLite 3.54.0, which sqlite3mc-src 206.1.0 bundles.

Keep this patch until upstream libsqlite3-sys can build SQLite3 Multiple Ciphers itself.

### 1. `sqlcipher-src` 420 (RUSTSEC-2026-0459; no longer built)

No crate enables `bundled-sqlcipher` since Task 6b, so this pin is dormant (sqlcipher-src and
openssl-sys left Cargo.lock); it stays so the feature still builds a fixed SQLite if anyone turns
it on, and is dropped with the copy.

One change, in `Cargo.toml` (and the same line in `Cargo.toml.orig`): the `sqlcipher-src`
build-dependency requirement moves from `=419.0.6` to `=420.0.0`. Nothing else is edited; `build.rs`
uses the same `sqlcipher_src::source_dir()` / `SOURCE_FILE` API, which 420.0.0 keeps unchanged
(its `src/` differs from 419.0.6 only in the version constants).

Why: RUSTSEC-2026-0459 / CVE-2026-119375 (SQLite before 3.54.0 can return uninitialized
page-cache memory when a database is opened with a crafted sibling `-wal` file of a different page
size). The advisory itself notes that `bundled-sqlcipher` in libsqlite3-sys 0.39.x stays affected. rusqlite 0.41.1 / libsqlite3-sys 0.39.1, the newest release, pins `sqlcipher-src =419.0.6`,
which bundles SQLCipher 4.19.0 on SQLite 3.53.4, so the native store dilla-core, dilla-bot-core and
the testkit build with `bundled-sqlcipher-vendored-openssl` stayed vulnerable even with the
advisory resolved in Cargo.lock. `sqlcipher-src 420.0.0` bundles SQLCipher 4.20.0 on SQLite 3.54.1.
Its regression test, `core/dilla-core/tests/sqlcipher_build.rs`, became `sqlite3mc_build.rs` in
Task 6b.

Known leftover: the pre-generated `sqlcipher/bindgen_bundled_version.rs` still comes from the
3.53.4 headers, so the compile-time `SQLITE_VERSION*` constants in the bindings say 3.53.4. The
runtime (`sqlite3_libversion*`, `SELECT sqlite_version()`) reports 3.54.1; SQLite's C ABI is
append-only, so older bindings against the newer library are sound.

The browser core (`wasm32-unknown-unknown`) does not use libsqlite3-sys; it compiles SQLite3MC
from sqlite-wasm-rs, which carries its own SQLite. That side is fixed without a vendored copy:
sqlite-wasm-rs 0.6.3 builds SQLite3MC 2.6.1 on SQLite 3.54.0 (`sqlite3mc-src =206.1.0`), see
"Retired" below.

Since Task 6b the native store builds its own SQLite3MC too (change 2), from the same
`sqlite3mc-src` release.

## Retired: sqlite-wasm-vfs 0.2.0

Vendored from 2026-10-08 until the RUSTSEC-2026-0459 fix moved the browser core to sqlite-wasm-rs
0.6.3 and sqlite-wasm-vfs 0.3.0. The one change was in `src/sahpool.rs` `xCheckReservedLock`: it
answered 0 ("no other connection holds a RESERVED lock") instead of 1, because SQLite plays a
leftover rollback journal back only when no connection holds RESERVED. With 1, the journal of a
write that a page unload interrupted was never played back, and the next open read the half-written
database (`.superpowers/sdd/2026-10-08-dilla-web-2c/opfs-report.md`).

sqlite-wasm-vfs 0.3.0 answers from the handle's own lock level (`self.lock_level >=
LockLevel::Reserved`), so a fresh connection reports no RESERVED lock and plays the journal back.
Still covered by `e2e/tests/mls-store.spec.ts` ("a write interrupted by a reload rolls back on the
next open"), and across the upgrade by `core/dilla-core-wasm/tests/sqlite_compat.rs` (a hot journal
written by the 0.5.5 build is played back by the current one).
