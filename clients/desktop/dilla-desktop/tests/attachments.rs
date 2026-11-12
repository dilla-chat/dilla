//! Task 12's parity, behaviour and keyboard tests for the attachments area (standard procedure S4-S6).
//!
//! The parity tests compare `parity/fixtures/attachments/*.json` with their references. The references are not shot
//! yet: a message's attachments go out with its post, so `parity_world/main.rs`'s posting loop has to call
//! `seed::attachments::send` for a message with attachments (the one-line change in Task 12's report; `main.rs` is not
//! Task 12's file). Until the controller makes it and the references are shot, the parity tests are ignored with that
//! reason, and the facet stays refused by `seed::attachments::unseedable`.
//!
//! The behaviour tests of `e2e/web/conversation.spec.ts` run at the client level against a real dillad
//! (`dilla-client/tests/attachments.rs`: `images_and_files_go_both_ways`, `file_cap_exact_passes_one_more_refused`);
//! their app-level drive needs the composer's send to carry the tray (Task 9's composer), which goes to Task 28's list.

use std::path::PathBuf;

use dilla_client::testing::spec::ScreenFixture;
use dilla_parity::screen::assert_screen;

fn fixture(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/attachments")
        .join(format!("{name}.json"))
}

const STATES: [&str; 6] = [
    "cards",
    "cards-b",
    "lightbox",
    "lightbox-b",
    "media-player",
    "media-player-b",
];

/// Every fixture parses, and each B fixture varies its A in at least three ways (S3): the window, and longer names or
/// more files.
#[test]
fn fixtures_parse_and_b_varies_a() {
    for name in STATES {
        let raw = std::fs::read_to_string(fixture(name)).unwrap();
        let f: ScreenFixture = serde_json::from_str(&raw).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.name, format!("attachments/{name}"));
    }
    let load = |n: &str| -> ScreenFixture {
        serde_json::from_str(&std::fs::read_to_string(fixture(n)).unwrap()).unwrap()
    };
    let (a, b) = (load("cards"), load("cards-b"));
    assert_eq!((a.window, b.window), ((1280, 800), (1440, 900)));
    let files = |f: &ScreenFixture| {
        f.world
            .messages
            .iter()
            .map(|m| m.attachments.len())
            .sum::<usize>()
    };
    assert!(
        files(&b) >= 10 && files(&a) == 2,
        "nine images and a file against two"
    );
    let longest = |f: &ScreenFixture| {
        f.world
            .messages
            .iter()
            .flat_map(|m| &m.attachments)
            .map(|x| x.name.len())
            .max()
            .unwrap_or(0)
    };
    assert!(longest(&b) > 60 && longest(&a) < 20, "long file names");
}

macro_rules! parity {
    ($($test:ident => $name:literal),* $(,)?) => {$(
        #[test]
        #[ignore = "no reference yet: parity_world/main.rs must post attachments (Task 12's report), then shoot.mjs"]
        fn $test() {
            if let Err(e) = assert_screen(&fixture($name)) {
                panic!("{e}");
            }
        }
    )*};
}

parity! {
    parity_attachments_cards => "cards",
    parity_attachments_cards_b => "cards-b",
    parity_attachments_lightbox => "lightbox",
    parity_attachments_lightbox_b => "lightbox-b",
    parity_attachments_media_player => "media-player",
    parity_attachments_media_player_b => "media-player-b",
}
