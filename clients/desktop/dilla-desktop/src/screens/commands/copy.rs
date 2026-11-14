//! The command picker's and the private results' copy that the web keeps outside its string tables:
//! packages/web/src/screens/commands/copy.ts (`commandPickerCopy`, `privateResultCopy`) and picker-catalog.ts, verbatim.
//! `scripts/gen-gpui-strings.mjs` reads only `packages/web/src/strings/*.ts`, so these are not in `crate::strings`; the
//! keyed strings this area uses (`commands.*`, `poll.*`, `privateResult.*`, `reminder.*`) still come from there.

/// `commandPickerCopy.commands`.
pub const COMMANDS: &str = "Commands";
/// `commandPickerCopy.listHint`.
pub const LIST_HINT: &str = "↑↓ move · tab complete · esc close";
/// `commandPickerCopy.fieldHint`.
pub const FIELD_HINT: &str = "tab next field · ↵ run · esc leave";
/// `commandPickerCopy.sendAsText`.
pub const SEND_AS_TEXT: &str = "Send as text";
/// `commandPickerCopy.pressToRun`.
pub const PRESS_TO_RUN: &str = "press ↵ to run";
/// `commandPickerCopy.whenHint`.
pub const WHEN_HINT: &str = "Try “in 1 hour” or “tomorrow 09:00”";

/// `commandPickerCopy.joinVoiceFirst`: a music command's bar with this device in no voice room.
pub const JOIN_VOICE_FIRST: &str = "join voice first";
/// `commandPickerCopy.separator`.
pub const SEPARATOR: &str = " · ";
/// `commandPickerCopy.sugTitle`: the suggestions' title, by what they suggest.
pub const SUG_DURATIONS: &str = "Durations";
pub const SUG_WHEN: &str = "When";
pub const SUG_SUGGESTIONS: &str = "Suggestions";
pub const SUG_COMMANDS: &str = "Commands";

/// `commandPickerCopy.noMatch`.
pub fn no_match(draft: &str) -> String {
    format!("No command matches {draft}")
}

/// `commandPickerCopy.remindsYou`.
pub fn reminds_you(preview: &str) -> String {
    format!("Reminds you {preview}")
}

/// picker-catalog.ts `BUILT_IN_SOURCE`, `OPENS_A_FORM`, `OPTIONAL_PLACEHOLDER`.
pub const BUILT_IN_SOURCE: &str = "built-in";
pub const OPENS_A_FORM: &str = "opens a form";
pub const OPTIONAL_PLACEHOLDER: &str = "optional";

/// `privateResultCopy.author`.
pub const AUTHOR_BOT: &str = "Glenn";
pub const AUTHOR_BUILTIN: &str = "dilla";
/// `privateResultCopy.labels`.
pub const STATUS_PENDING: &str = "Working";
pub const STATUS_SUCCESS: &str = "Done";
pub const STATUS_FAILED: &str = "Failed";
pub const STATUS_CANCELLED: &str = "Not confirmed";
pub const ONLY_YOU: &str = "Only you can see this";
pub const DISMISS: &str = "Dismiss";

/// `privateResultCopy.pending(id)`.
pub fn pending(id: &str) -> String {
    match id {
        "remind" => "Saving your reminder…".to_owned(),
        "help" => "Looking that up…".to_owned(),
        "play" | "pause" | "resume" | "skip" | "stop" | "queue" | "volume" => {
            "Checking your voice channel…".to_owned()
        }
        other => format!("/{other}…"),
    }
}

/// `privateResultCopy.action`: the moderation dialog's confirm button.
pub fn action(id: &str) -> &'static str {
    if id == "ban" { "Ban" } else { "Kick" }
}

/// `privateResultCopy.title`. The web isolates the names (U+2068/U+2069); GPUI's shaper has no bidi isolation to give
/// them, so they are left out.
pub fn moderation_title(action: &str, name: &str, server: &str) -> String {
    format!("{action} {name} from {server}?")
}

/// `privateResultCopy.kickBody`.
pub fn kick_body(name: &str) -> String {
    format!("{name} leaves now and can come back only with a new invite.")
}

/// `privateResultCopy.banBody`.
pub fn ban_body(name: &str, duration: &str) -> String {
    if duration == "permanent" {
        format!("{name} leaves now and can’t rejoin permanently.")
    } else {
        format!("{name} leaves now and can’t rejoin for {duration}.")
    }
}

/// `privateResultCopy.server`, `.duration`, `.reason`, `.noReason`, `.logged`, `.loggedNote`.
pub const SERVER: &str = "this server";
pub const DURATION: &str = "Duration";
pub const REASON: &str = "Reason";
pub const NO_REASON: &str = "none given";
pub const LOGGED: &str = "Logged";
pub const LOGGED_NOTE: &str = "modlog · visible to moderators";
