//! `RecoveryKeyField` (packages/ui/src/RecoveryKeyField) in the onboarding frame: where a person types or pastes the
//! recovery key. Paste is allowed and nothing is cut off: no length limit, nothing offers to remember it. The value is
//! shown as typed (the client normalises it); the hint (the frame's 11.5px `--fg-3` length line) and the error follow
//! the field. In the frame the input is the 40px auth field with 13px text.
//!
//! The host owns the `InputState`, reads its value into a zeroizing buffer and clears it when the step is left.

use gpui::{App, ElementId, Entity, IntoElement, RenderOnce, SharedString, Window};
use gpui_component::input::InputState;

use super::frame::OnboardingField;

#[derive(IntoElement)]
pub struct RecoveryKeyField {
    field: OnboardingField,
}

impl RecoveryKeyField {
    pub fn new(
        id: impl Into<ElementId>,
        label: impl Into<SharedString>,
        input: &Entity<InputState>,
    ) -> Self {
        Self {
            field: OnboardingField::new(id, label, input),
        }
    }

    /// The length line (`signin.key.hint`: "{n} of 52 characters").
    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.field = self.field.hint(hint);
        self
    }

    pub fn error(mut self, error: Option<SharedString>) -> Self {
        self.field = self.field.error(error);
        self
    }
}

impl RenderOnce for RecoveryKeyField {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        self.field
    }
}
