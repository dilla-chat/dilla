//! Settings -> Appearance (packages/web/src/screens/settings/Appearance.tsx): the theme, the density and reduced motion.
//!
//! A choice applies at once. Density and reduced motion are kept in the store (`setSetting`, the client's
//! `Profile::set_density` and `set_reduce_motion`); the density shown is what the section's own rows and the shared components
//! step by ([`density_of`], `metrics::Density`). The theme has the one entry, Signal.
//!
//! The brief names three densities (compact, regular, cozy); the web client admits two (`isSettingValue`: `regular`, `compact`;
//! the stylesheet's `cozy` is a Storybook step only), and parity wins, so the Segmented has the two.

use gpui::{AnyElement, Context, IntoElement, ParentElement, Styled, div};

use dilla_client::state::profile::{DensitySetting, MotionSetting};
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::settings::frame::{Section, section_header};
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::icon_names::IconName;
use dilla_desktop_ui::metrics::Density;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};

use super::section::UserSettings;
use crate::strings::t;

/// The shared components' density step for a setting.
pub fn density_of(setting: DensitySetting) -> Density {
    match setting {
        DensitySetting::Compact => Density::Compact,
        DensitySetting::Regular => Density::Regular,
    }
}

impl UserSettings {
    fn set_density(&mut self, value: &str, cx: &mut Context<Self>) {
        let Some(setting) = DensitySetting::from_id(value) else {
            return;
        };
        let Some(client) = self.ctx.client.clone() else {
            return;
        };
        self.appearance_failed = false;
        self.appearance_pending = true;
        let rx = client.profile().set_density(setting);
        self.settled(rx, cx);
    }

    fn set_motion(&mut self, value: &str, cx: &mut Context<Self>) {
        let Some(setting) = MotionSetting::from_id(value) else {
            return;
        };
        let Some(client) = self.ctx.client.clone() else {
            return;
        };
        self.appearance_failed = false;
        self.appearance_pending = true;
        let rx = client.profile().set_reduce_motion(setting);
        self.settled(rx, cx);
    }

    fn settled(
        &mut self,
        rx: tokio::sync::oneshot::Receiver<Result<(), dilla_client::ClientError>>,
        cx: &mut Context<Self>,
    ) {
        cx.spawn(async move |this, cx| {
            let r = rx.await;
            let _ = this.update(cx, |this, cx| {
                this.appearance_pending = false;
                this.appearance_failed = !matches!(r, Ok(Ok(())));
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    pub(super) fn appearance_view(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let state = &self.ctx.state.profile;
        let this = cx.entity();

        let theme = SettingsRow::new("appearance-theme-row", t("appearance.theme.label"))
            .icon(IconName::Eye)
            .child(Segmented::new(
                "appearance-theme",
                &[("signal", t("appearance.theme.signal"))],
                "signal",
            ));
        let density_row = SettingsRow::new("appearance-density-row", t("workspace.density"))
            .icon(IconName::LayoutGrid)
            .child(
                Segmented::new(
                    "appearance-density",
                    &[
                        ("regular", t("workspace.density.regular")),
                        ("compact", t("workspace.density.compact")),
                    ],
                    state.density.id(),
                )
                .option_child(|v| option_probe("appearance-density", v))
                .on_change({
                    let this = this.clone();
                    move |v, _, cx| {
                        let v = v.to_owned();
                        this.update(cx, |s, cx| s.set_density(&v, cx));
                    }
                }),
            );
        let motion_row = SettingsRow::new("appearance-motion-row", t("workspace.motion"))
            .icon(IconName::Activity)
            .child(
                Segmented::new(
                    "appearance-motion",
                    &[
                        ("system", t("workspace.motion.system")),
                        ("on", t("workspace.motion.on")),
                        ("off", t("workspace.motion.off")),
                    ],
                    state.reduce_motion.id(),
                )
                .option_child(|v| option_probe("appearance-motion", v))
                .on_change({
                    let this = this.clone();
                    move |v, _, cx| {
                        let v = v.to_owned();
                        this.update(cx, |s, cx| s.set_motion(&v, cx));
                    }
                }),
            );

        // The three rows touch (`.d-settings-row + .d-settings-row`): one group, its edges shared.
        let rows = div()
            .flex()
            .flex_col()
            .child(theme.position(GroupPosition::First))
            .child(density_row.position(GroupPosition::Middle))
            .child(motion_row.position(GroupPosition::Last));
        let mut section = Section::new(section_header(t("appearance.title"), None)).block(rows);
        if self.appearance_pending {
            section = section.paragraph(line(
                t("workspace.preference.pending"),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_2,
            ));
        }
        if self.appearance_failed {
            section = section.block(Banner::new(
                "appearance-failed",
                BannerTone::Danger,
                t("workspace.preference.error"),
            ));
        }
        let col = section.into_div();
        col.into_any_element()
    }
}

/// The automation build's name of a Segmented option (`<group>-<value>`, the fixtures' `gpui` step ids); none otherwise.
fn option_probe(group: &str, value: &str) -> Option<gpui::AnyElement> {
    #[cfg(feature = "automation")]
    {
        use gpui::IntoElement as _;
        Some(
            crate::automation::bounds_probe(crate::screens::channel_list::probe_id(group, value))
                .into_any_element(),
        )
    }
    #[cfg(not(feature = "automation"))]
    {
        let _ = (group, value);
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_setting_steps_the_shared_components_density() {
        assert_eq!(density_of(DensitySetting::Compact), Density::Compact);
        assert_eq!(density_of(DensitySetting::Regular), Density::Regular);
        assert!(Density::Compact.group_gap() < Density::Regular.group_gap());
    }
}
