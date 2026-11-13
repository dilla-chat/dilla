//! Task 1's area scaffold: with every slot stubbed, the hosts get nothing to draw, and the settings registry names every
//! section of the web registry with the area that owns it. That the stubs change no pixel is the slice's parity tests'
//! (`tests/slice.rs`: `slice_parity_channel_view`, `slice_sign_in_read_send`); the route forms are `router.rs`'s unit
//! tests.

use dilla_client::Area;
use dilla_client::state::{
    Channel, ChannelGroupState, ChannelId, CommunityId, TimelineItem, TimelineItemState,
};
use dilla_desktop::router::{Route, ServerSection, UserSection};
use dilla_desktop::slots::{self, SectionId, SlotCx};
use std::cell::Cell;
use std::rc::Rc;

use gpui::{Context, IntoElement, Render, TestAppContext, Window};

fn item() -> TimelineItem {
    TimelineItem {
        key: "s1".into(),
        state: TimelineItemState::Ok,
        reason: String::new(),
        sender_user: None,
        sender_device: None,
        own: true,
        bot: false,
        ts: 1_794_476_484,
        body: "hello".into(),
        msg_id: None,
        seq: Some(1),
        edited: false,
        reply: None,
        reactions: Vec::new(),
        pinned: false,
        attachments: Vec::new(),
        mention: false,
    }
}

#[gpui::test]
fn stubs_change_nothing(cx: &mut TestAppContext) {
    let route = Route::Channel {
        community: CommunityId([1; 16]),
        channel: Some(ChannelId([2; 16])),
    };
    let ctx = SlotCx::default();
    assert!(slots::message_actions(&item(), &ctx).is_empty());
    let window = cx.add_empty_window();
    window.update(|window, cx| {
        assert!(slots::composer_popup(&ctx, window, cx).is_none());
        assert!(slots::aside(&route, window, cx).is_none());
        assert!(slots::dock(window, cx).is_none());
        assert!(slots::overlays(&slots::SlotCx::default(), window, cx).is_empty());
        assert!(slots::non_modal_overlays(window, cx).is_empty());
        assert!(slots::banners(&ctx, window, cx).is_empty());
        // Stubbed extras take no height, so the timeline's rows keep theirs.
        let (_, height) = slots::message_extras(&item(), &ctx, window, cx);
        assert_eq!(height, 0.0);
        // The other element slots always answer an element; empty, it is a bare flex container (see slots.rs).
        let _ = slots::composer_leading(&ctx, window, cx);
        let _ = slots::channel_row_extras(&voice_channel(), window, cx);
    });
    // The composer's buttons are filled (Tasks 10 and 12): the emoji button keeps its focus in element state, which
    // exists only while a view renders, so the slot is drawn by a view here.
    let drawn = Rc::new(Cell::new(false));
    let (_view, vcx) = cx.add_window_view({
        let drawn = drawn.clone();
        move |_, _| Buttons { drawn }
    });
    vcx.run_until_parked();
    assert!(
        drawn.get(),
        "the composer's buttons slot drew inside a render"
    );
}

/// Draws the composer's buttons slot.
struct Buttons {
    drawn: Rc<Cell<bool>>,
}

impl Render for Buttons {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.drawn.set(true);
        slots::composer_buttons(&SlotCx::default(), window, cx)
    }
}

fn voice_channel() -> Channel {
    Channel {
        id: ChannelId([3; 16]),
        community_id: CommunityId([1; 16]),
        kind: 1,
        mode: 0,
        name: "Lounge".into(),
        topic: String::new(),
        parent_id: None,
        position: 0,
        slowmode: 0,
        text_group_id: None,
        group: ChannelGroupState::default(),
    }
}

/// Every user and server section of settings/registry.ts, in its order, owned by the area the plan gives it.
#[test]
fn settings_registry_covers_every_section_once() {
    let sections = slots::settings_sections();
    let ids: Vec<SectionId> = sections.iter().map(|s| s.id).collect();
    let want: Vec<SectionId> = UserSection::ALL
        .iter()
        .map(|&s| SectionId::User(s))
        .chain(ServerSection::ALL.iter().map(|&s| SectionId::Server(s)))
        .collect();
    assert_eq!(ids, want);
    let owner = |id: SectionId| sections.iter().find(|s| s.id == id).map(|s| s.area);
    for (id, area) in [
        (SectionId::User(UserSection::Profile), Area::UserSettings),
        (SectionId::User(UserSection::Signin), Area::Factors),
        (SectionId::User(UserSection::Devices), Area::UserSettings),
        (SectionId::User(UserSection::Notifications), Area::Notify),
        (SectionId::User(UserSection::Voice), Area::VoiceSettings),
        (SectionId::User(UserSection::Keyboard), Area::UserSettings),
        (SectionId::User(UserSection::Appearance), Area::UserSettings),
        (SectionId::User(UserSection::Privacy), Area::Dms),
        (SectionId::User(UserSection::Diagnostics), Area::Diagnostics),
        (
            SectionId::Server(ServerSection::Overview),
            Area::ServerSettings,
        ),
        (
            SectionId::Server(ServerSection::Channels),
            Area::ServerSettings,
        ),
        (SectionId::Server(ServerSection::Gifs), Area::ServerSettings),
        (SectionId::Server(ServerSection::Members), Area::Members),
        (SectionId::Server(ServerSection::Roles), Area::Members),
        (
            SectionId::Server(ServerSection::Invites),
            Area::ServerSettings,
        ),
        (SectionId::Server(ServerSection::SelfRoles), Area::Members),
        (SectionId::Server(ServerSection::Bot), Area::BotAdmin),
        (
            SectionId::Server(ServerSection::Welcome),
            Area::ServerSettings,
        ),
        (
            SectionId::Server(ServerSection::JoinRoles),
            Area::ServerSettings,
        ),
        (SectionId::Server(ServerSection::Music), Area::BotAdmin),
        (
            SectionId::Server(ServerSection::ModerationLog),
            Area::Members,
        ),
        (SectionId::Server(ServerSection::Bans), Area::Members),
        (
            SectionId::Server(ServerSection::Danger),
            Area::ServerSettings,
        ),
    ] {
        assert_eq!(owner(id), Some(area), "{id:?}");
    }
}
