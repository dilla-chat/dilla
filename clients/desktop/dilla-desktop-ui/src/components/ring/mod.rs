//! Task 18's components (`components/ring/`): the incoming-ring banner (packages/ui/src/RingBanner) and the AFK page
//! (the `afk` body of packages/ui/src/CallStage).

pub mod afk_page;
pub mod ring_banner;

pub use afk_page::AfkPage;
pub use ring_banner::{
    Answer, CONTEXT, DECLINE_CONTEXT, Decline, OnRing, RingBanner, RingFocus, RingRow,
};
