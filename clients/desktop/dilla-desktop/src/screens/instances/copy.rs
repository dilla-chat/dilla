//! The manager window's words: the copy of `packages/desktop/src/manager.{html,ts}` (the beta.5 Electron window, tag desktop-v0.1.0-beta.5), plus
//! the new removal, invite and 32-instance lines. None of them is in the web client's string catalogue
//! (`strings.rs` is generated from `packages/web/src/strings/*.ts`), so they live here until the generator learns the
//! manager's file; the report lists them.

pub const WINDOW_TITLE: &str = "Dilla \u{2014} your instances";
pub const TITLEBAR_NAME: &str = "dilla";
pub const TITLEBAR_SECTION: &str = "Your instances";
pub const BRAND: &str = "dilla";
pub const BRAND_NOTE: &str = "desktop preview";
pub const HEADING_1: &str = "Your people.";
pub const HEADING_2: &str = "Your instance.";
pub const LEAD: &str = "Connect to the Dilla address your community uses. Your account and conversations stay with that instance.";
pub const ADDRESS_LABEL: &str = "Instance address";
pub const ADDRESS_PLACEHOLDER: &str = "https://chat.example.com";
pub const CONNECT: &str = "Connect";
pub const CONNECT_BUSY: &str = "Connecting\u{2026}";
pub const INVITE_LABEL: &str = "Invite link";
pub const INVITE_PLACEHOLDER: &str = "https://chat.example.com/i/code";
pub const INVITE_HINT: &str = "Paste an invite link to join its community.";
pub const INVITE_CONNECT: &str = "Join";
pub const LIST_TITLE: &str = "Your instances";
pub const EMPTY: &str = "An instance you connect to will appear here.";
pub const FOOTER: &str = "This validation build is not ready for daily use. Keep your existing browser device and recovery key.";
pub const REMOVE: &str = "Remove";
pub const REMOVE_HINT: &str = "Delete";
pub const REMOVE_TITLE: &str = "Remove this instance?";
pub const REMOVE_BODY: &str = "{host} and everything this device holds for it are deleted from this device, and its encryption key is erased. Your account stays on the server.";
pub const REMOVE_CONFIRM: &str = "Remove";
pub const REMOVE_BUSY: &str = "Removing\u{2026}";
pub const CANCEL: &str = "Cancel";
pub const NEW_TITLE: &str = "Connect to a Dilla instance?";
/// main.ts `confirmInstance` as the sign-up shows it (Task 25): a title naming the host, the instance, the warning.
pub const CONFIRM_TITLE: &str = "Connect to {host}?";
pub const CONFIRM_INSTANCE: &str = "Instance: {instance}";
pub const CONFIRM_DETAIL: &str = "Only continue if you recognize this address.";
pub const CONFIRM_CONNECT: &str = "Connect";
pub const NEW_BODY: &str = "Connect to {host}? Only continue if you recognize this address.";
pub const ERROR_CONNECT: &str =
    "Could not connect. Enter a complete HTTPS instance address, then try again.";
/// The Invite link field's own refusal: the address field's words would point at the wrong field.
pub const ERROR_INVITE: &str = "Could not join. Paste a complete invite link, such as https://chat.example.com/i/code, then try again.";
pub const ERROR_TOO_MANY: &str =
    "This device already has {max} instances. Remove one to connect another.";
pub const ERROR_IN_USE: &str = "This instance is open in a window. Close it first.";
pub const ERROR_KEYCHAIN: &str =
    "The system keychain would not delete this instance's key, so nothing was removed. Try again.";
pub const ERROR_STORE: &str = "Could not read your instances. Restart Dilla to try again.";
pub const ERROR_REMOVING: &str = "This instance is still shutting down, so its removal is not finished. Remove it again in a moment.";
pub const ERROR_CHANGED: &str =
    "Your instances changed in another window. Check the list and try again.";
pub const ERROR_WRONG_INSTANCE: &str =
    "This window belongs to another instance. Sign in to its own address.";
