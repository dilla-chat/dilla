//! Task 6b link proof: dilla-core's encrypted store and LiveKit's libwebrtc live in one binary.
//!
//! Before 6b the native store was SQLCipher on vendored OpenSSL, and LiveKit's prebuilt libwebrtc
//! carries BoringSSL under an incomplete rename list, so any binary that used both failed to link
//! with ~208 duplicate symbols (EVP_MD_CTX_get0_md, d2i_PKCS8_PRIV_KEY_INFO, ASN1_*_it, ...). The
//! store is now SQLite3 Multiple Ciphers with its own crypto. This test binary links both and
//! exercises both: it keys, writes and reads a ChaCha20 store, derives an SFrame secret, and
//! creates a WebRTC peer connection (whose DTLS certificate BoringSSL generates).

use std::sync::atomic::{AtomicU64, Ordering};

use dilla_core::native_store::{StoreCipher, open_keyed};
use livekit::webrtc::peer_connection_factory::{PeerConnectionFactory, RtcConfiguration};
use rusqlite::OpenFlags;

#[test]
fn dilla_core_store_and_livekit_webrtc_link_into_one_binary() {
    static NEXT: AtomicU64 = AtomicU64::new(0);
    let dir = std::env::temp_dir().join(format!(
        "dilla-media-link-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join("store.db");
    std::fs::File::create(&path).unwrap();

    let keyed = open_keyed(
        &path,
        OpenFlags::SQLITE_OPEN_READ_WRITE,
        &[0x3c; 32],
        |_| Ok(()),
    )
    .expect("keyed store");
    assert_eq!(keyed.cipher, StoreCipher::ChaCha20);
    keyed
        .conn
        .execute_batch("CREATE TABLE t(v TEXT); INSERT INTO t VALUES('linked');")
        .unwrap();
    let v: String = keyed
        .conn
        .query_row("SELECT v FROM t", [], |r| r.get(0))
        .unwrap();
    assert_eq!(v, "linked");
    drop(keyed);

    let secret = dilla_core::sframe::sframe_secret(&[7; dilla_core::sframe::NK]);
    assert_ne!(secret, [0; 32]);

    let factory = PeerConnectionFactory::default();
    let pc = factory
        .create_peer_connection(RtcConfiguration::default())
        .expect("peer connection");
    pc.close();

    let _ = std::fs::remove_dir_all(dir);
}
