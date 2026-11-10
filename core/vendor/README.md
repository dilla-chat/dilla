# Vendored crates

Third-party crates that dilla builds from a patched copy, through `[patch.crates-io]` in the root
`Cargo.toml`. Each copy is the published crate as it was downloaded from crates.io, minus the
registry's `.cargo-ok` and `Cargo.lock`, plus the changes listed below. Every change is marked
`DILLA PATCH` in the source. The directory is excluded from the workspace, so `cargo fmt`, `cargo
clippy` and the workspace lints do not touch it.

## sqlite-wasm-vfs 0.2.0

Upstream: <https://github.com/Spxg/sqlite-wasm-rs> (`.cargo_vcs_info.json` has the commit). MIT.

One change, in `src/sahpool.rs` `xCheckReservedLock`: it answers 0 ("no other connection holds a
RESERVED lock") instead of 1.

Why: SQLite treats a leftover rollback journal as hot, and plays it back, only when no connection
holds RESERVED. With 1, the journal of a write that a page unload interrupted was never played back,
and the next open read the half-written database: "database disk image is malformed", or committed
rows silently replaced by uncommitted ones (`.superpowers/sdd/2026-10-08-dilla-web-2c/opfs-report.md`;
regression test `e2e/tests/mls-store.spec.ts`, "a write interrupted by a reload rolls back on the
next open"). The pool's OPFS access handles are exclusive to one worker, so a journal found when the
store opens belongs to a connection that no longer exists. sqlite-wasm-vfs 0.3.0 reports no RESERVED
lock for a fresh connection too.

Remove this copy and the patch entry when the store moves to sqlite-wasm-vfs 0.3.0 or later (that
release needs sqlite-wasm-rs 0.6, so it moves together with rusqlite).

## libsqlite3-sys 0.39.1

Upstream: <https://github.com/rusqlite/rusqlite> (`libsqlite3-sys/`; `.cargo_vcs_info.json` has the
commit). MIT.

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
Regression test: `core/dilla-core/tests/sqlcipher_build.rs` (`sqlite_version()` >= 3.54.1 with
`PRAGMA cipher_version` non-empty on a keyed connection); it fails with this patch removed.

Known leftover: the pre-generated `sqlcipher/bindgen_bundled_version.rs` still comes from the
3.53.4 headers, so the compile-time `SQLITE_VERSION*` constants in the bindings say 3.53.4. The
runtime (`sqlite3_libversion*`, `SELECT sqlite_version()`) reports 3.54.1; SQLite's C ABI is
append-only, so older bindings against the newer library are sound.

Not covered: the browser core (`wasm32-unknown-unknown`) does not use libsqlite3-sys; it compiles
SQLite3MC from sqlite-wasm-rs, which carries its own SQLite.

Remove this copy and the patch entry once upstream ships sqlcipher-src >= 420 (a libsqlite3-sys
release whose `sqlcipher-src` requirement is 420.0.0 or later).
