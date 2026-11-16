//! TLS for LiveKit's signalling socket. dillad hands out `wss://<host>` rooms; LiveKit's native transport
//! (livekit-net, tokio-tungstenite 0.29) opens them with rustls, built here with the `rustls-tls-native-roots` feature
//! (the OS's roots). Its `ClientConfig::builder()` takes the process-wide rustls provider, and rustls is built with both
//! ring and aws-lc-rs in this binary, so none is chosen on its own: [`ensure_provider`] installs aws-lc-rs (the one the
//! gateway socket uses, dilla-client gateway) before a room is entered.

/// Installs aws-lc-rs as the process's rustls provider unless one is installed already. Idempotent and cheap.
pub fn ensure_provider() {
    if rustls::crypto::CryptoProvider::get_default().is_none() {
        // Another thread may install one first: either provider is fine, so that error is ignored.
        let _ = rustls::crypto::aws_lc_rs::default_provider().install_default();
    }
}

/// `url` fit for a log: no query or fragment (a room URL may carry `access_token`) and no user information.
pub fn redacted_url(url: &str) -> String {
    let end = url.find(['?', '#']).unwrap_or(url.len());
    let url = &url[..end];
    let (scheme, rest) = url.split_once("://").unwrap_or(("", url));
    let auth_end = rest.find('/').unwrap_or(rest.len());
    let rest = match rest[..auth_end].rfind('@') {
        Some(at) => &rest[at + 1..],
        None => rest,
    };
    if scheme.is_empty() {
        rest.to_owned()
    } else {
        format!("{scheme}://{rest}")
    }
}

#[cfg(test)]
mod tests {
    use std::io::Write;

    #[test]
    fn redacted_urls_keep_no_token_or_credentials() {
        assert_eq!(
            super::redacted_url("wss://dilla.example/rtc?access_token=eyJ.secret&x=1"),
            "wss://dilla.example/rtc"
        );
        assert_eq!(
            super::redacted_url("wss://user:pw@dilla.example:7880/a#frag"),
            "wss://dilla.example:7880/a"
        );
        assert_eq!(
            super::redacted_url("wss://dilla.example"),
            "wss://dilla.example"
        );
        assert_eq!(super::redacted_url("host/x?t=1"), "host/x");
    }

    /// The regression of the macOS report "Could not join voice": a `wss://` room failed before any byte left the
    /// machine because the signalling socket had no TLS. Here the socket reaches a local listener that is no TLS server:
    /// the attempt must get as far as a TLS handshake (and fail it), never "TLS support not compiled in", and never
    /// panic for want of a rustls provider.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn wss_rooms_reach_a_tls_handshake() {
        super::ensure_provider();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let server = std::thread::spawn(move || {
            if let Ok((mut s, _)) = listener.accept() {
                let _ = s.write_all(b"HTTP/1.1 400 Not TLS\r\n\r\n");
            }
        });
        let ws = livekit_net::ws_client().expect("the native transport");
        let res = ws
            .connect(format!("wss://127.0.0.1:{port}/rtc"), Vec::new(), 5_000)
            .await;
        let err = match res {
            Ok(_) => panic!("a plain TCP listener completed a TLS handshake"),
            Err(e) => e.to_string(),
        };
        assert!(
            !err.contains("TLS support not compiled in"),
            "the signalling socket has no TLS: {err}"
        );
        let _ = server.join();
    }
}
