// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// Test support for the dilla-sframe/1 hook: synthetic frames through a real `DillaFrameTransformer`
// with no PeerConnection. Built only with the `dilla-test-frames` feature, which only dilla-media's
// dev-dependency enables, so no shipping binary contains it.

/// A transformer like one a sender or receiver would get; 0 without an installed hook.
pub fn attach(send: bool, video: bool, track_id: &str) -> u64 {
    ffi::dilla_test_attach(send, video, track_id)
}

pub fn frame(handle: u64, mime: &str, data: &[u8], key_frame: bool) {
    ffi::dilla_test_frame(handle, mime, data, key_frame);
}

/// The frames the transformer passed on since the last call, in order.
pub fn take_emitted(handle: u64) -> Vec<Vec<u8>> {
    let flat = ffi::dilla_test_take_emitted(handle);
    let mut out = Vec::new();
    let mut rest = flat.as_slice();
    while rest.len() >= 4 {
        let n = u32::from_be_bytes([rest[0], rest[1], rest[2], rest[3]]) as usize;
        out.push(rest[4..4 + n].to_vec());
        rest = &rest[4 + n..];
    }
    out
}

pub fn held(handle: u64) -> usize {
    ffi::dilla_test_held(handle)
}

pub fn emitted_count(handle: u64) -> usize {
    ffi::dilla_test_emitted_count(handle)
}

/// Drops the transformer (the hook hears `detached`).
pub fn release(handle: u64) {
    ffi::dilla_test_release(handle);
}

#[cxx::bridge(namespace = "livekit_ffi")]
mod ffi {
    unsafe extern "C++" {
        include!("livekit/dilla_transform_testing.h");

        fn dilla_test_attach(send: bool, video: bool, track_id: &str) -> u64;
        fn dilla_test_frame(handle: u64, mime: &str, data: &[u8], key_frame: bool);
        fn dilla_test_take_emitted(handle: u64) -> Vec<u8>;
        fn dilla_test_emitted_count(handle: u64) -> usize;
        fn dilla_test_held(handle: u64) -> usize;
        fn dilla_test_release(handle: u64);
    }
}
