//! Every key binding of the app, registered once per app by [`bind_all`]: the shared focus keys
//! ([`crate::screens::bind_keys`]), then each area's own (`crate::screens::<area>::keys::bind`, each in its task's files).
//! The list is Task 1's; in Wave 1 only the controller edits it, at merge (clients/desktop/OWNERSHIP.md).

use gpui::App;

use crate::screens;

pub fn bind_all(cx: &mut App) {
    screens::bind_keys(cx);
    dilla_desktop_ui::components::dialog::bind_keys(cx);
    screens::shell::keys::bind(cx);
    screens::messages::keys::bind(cx);
    screens::reactions::keys::bind(cx);
    screens::threads::keys::bind(cx);
    screens::attachments::keys::bind(cx);
    screens::dms::keys::bind(cx);
    screens::members::keys::bind(cx);
    screens::server_settings::keys::bind(cx);
    screens::user_settings::keys::bind(cx);
    screens::read_aloud::bind(cx);
    screens::voice::keys::bind(cx);
    screens::ring::keys::bind(cx);
    screens::voice_settings::keys::bind(cx);
    screens::commands::keys::bind(cx);
    screens::bot_admin::keys::bind(cx);
    screens::notify::keys::bind(cx);
    screens::search::keys::bind(cx);
    screens::diagnostics::keys::bind(cx);
    screens::onboarding::keys::bind(cx);
    screens::factor::keys::bind(cx);
    screens::instances::keys::bind(cx);
}
