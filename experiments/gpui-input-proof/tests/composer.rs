//! Headless composer tests on GPUI's test platform. IME itself is not
//! reachable here (the test platform has no OS input method); composition is
//! driven through the same `EntityInputHandler` calls the platform makes.

use gpui::{ClipboardItem, Entity, EntityInputHandler, TestAppContext, VisualTestContext};
use gpui_component::input::{Paste, TextareaState};
use gpui_input_proof::{Composer, mention_at};

fn setup(cx: &mut TestAppContext) -> (Entity<Composer>, &mut VisualTestContext) {
    cx.update(gpui_component::init);
    let (composer, vcx) = cx.add_window_view(Composer::new);
    vcx.update(|window, cx| {
        let input = composer.read(cx).input.clone();
        input.update(cx, |s, cx| s.focus(window, cx));
    });
    vcx.run_until_parked();
    (composer, vcx)
}

fn text(c: &Entity<Composer>, cx: &mut VisualTestContext) -> String {
    c.read_with(cx, |c, cx| c.value(cx).to_string())
}

fn input(c: &Entity<Composer>, cx: &mut VisualTestContext) -> Entity<TextareaState> {
    c.read_with(cx, |c, _| c.input.clone())
}

#[gpui::test]
fn enter_sends_shift_enter_newlines(cx: &mut TestAppContext) {
    let (c, cx) = setup(cx);
    cx.simulate_input("hi");
    cx.simulate_keystrokes("shift-enter");
    cx.simulate_input("there");
    assert_eq!(text(&c, cx), "hi\nthere");
    assert!(c.read_with(cx, |c, _| c.sent.is_empty()));

    cx.simulate_keystrokes("enter");
    assert_eq!(c.read_with(cx, |c, _| c.sent.clone()), ["hi\nthere"]);
    assert_eq!(text(&c, cx), "", "draft clears after send");

    // Enter on an empty draft sends nothing and adds no newline.
    cx.simulate_keystrokes("enter");
    assert_eq!(c.read_with(cx, |c, _| c.sent.len()), 1);
    assert_eq!(text(&c, cx), "");
}

#[gpui::test]
fn dead_key_e_acute(cx: &mut TestAppContext) {
    let (c, cx) = setup(cx);
    let state = input(&c, cx);
    // The platform shows the dead acute as marked text (the IME's callback), then delivers the follow-up as a keystroke
    // whose `key_char` is the composed character: that keystroke goes through the window's own key dispatch, which hands
    // its text to the input handler, not through a direct `replace_text_in_range` call.
    cx.update(|window, app| {
        state.update(app, |s, cx| {
            s.replace_and_mark_text_in_range(None, "´", Some(1..1), window, cx);
        });
    });
    assert_eq!(text(&c, cx), "´", "dead key is shown while composing");
    assert!(cx.update(|window, app| state.update(app, |s, cx| s.marked_text_range(window, cx).is_some())));
    cx.simulate_input("é");
    assert_eq!(text(&c, cx), "é", "the composed character replaces the dead key");
    assert!(
        cx.update(|window, app| state.update(app, |s, cx| s.marked_text_range(window, cx).is_none())),
        "composition ended"
    );
    // Enter during composition must not send; after it, it does.
    assert!(c.read_with(cx, |c, _| c.sent.is_empty()));
    cx.update(|window, app| {
        state.update(app, |s, cx| {
            s.replace_and_mark_text_in_range(None, "´", Some(1..1), window, cx);
        });
    });
    cx.simulate_keystrokes("enter");
    assert!(
        c.read_with(cx, |c, _| c.sent.is_empty()),
        "Enter while composing must not send"
    );
}

#[gpui::test]
fn paste_multiline_keeps_newlines(cx: &mut TestAppContext) {
    let (c, cx) = setup(cx);
    // The platform's paste chord is a real binding of the `Paste` action in the input's context: the chord is read back
    // from the keymap, never assumed, and pressing it pastes (no direct action dispatch).
    let stroke = cx.update(|window, _| {
        window
            .bindings_for_action(&Paste)
            .first()
            .and_then(|b| b.keystrokes().first().map(|k| k.inner().clone()))
    });
    let stroke = stroke.expect("Paste has a binding");
    assert_eq!(stroke.key, "v");
    let mac = cfg!(target_os = "macos");
    assert!(
        stroke.modifiers.platform == mac && stroke.modifiers.control == !mac,
        "the paste chord is the platform's, got {stroke:?}"
    );
    let chord = if mac { "cmd-v" } else { "ctrl-v" };
    cx.write_to_clipboard(ClipboardItem::new_string(
        "line one\nline two\n\nline four".into(),
    ));
    cx.simulate_keystrokes(chord);
    assert_eq!(text(&c, cx), "line one\nline two\n\nline four");
    assert!(
        c.read_with(cx, |c, _| c.sent.is_empty()),
        "paste must not send"
    );
}

#[gpui::test]
fn emoji_insert_at_caret(cx: &mut TestAppContext) {
    let (c, cx) = setup(cx);
    cx.simulate_input("ab");
    cx.simulate_keystrokes("left");
    let state = input(&c, cx);
    cx.update(|window, app| {
        state.update(app, |s, cx| s.insert("😀👍🏽", window, cx));
    });
    assert_eq!(text(&c, cx), "a😀👍🏽b");
    // Caret sits right after the inserted emoji: typing continues there.
    cx.simulate_input("!");
    assert_eq!(text(&c, cx), "a😀👍🏽!b");
    // One backspace removes the `!`.
    cx.simulate_keystrokes("backspace");
    assert_eq!(text(&c, cx), "a😀👍🏽b");
    // A second removes the skin-tone scalar only: gpui-component deletes by scalar, not by grapheme cluster, so a bare
    // thumbs-up is left. (Chromium's textarea deletes the whole cluster; this is the measured difference the composer
    // carries until the input crate changes, not a behaviour to rely on.)
    cx.simulate_keystrokes("backspace");
    assert_eq!(text(&c, cx), "a😀👍b");
    // A third removes the thumbs-up, and the text is valid UTF-8 throughout.
    cx.simulate_keystrokes("backspace");
    assert_eq!(text(&c, cx), "a😀b");
}

#[gpui::test]
fn mention_trigger_opens_popup_at_at_sign(cx: &mut TestAppContext) {
    let (c, cx) = setup(cx);
    cx.simulate_input("hello ");
    assert!(c.read_with(cx, |c, _| c.mention.is_none()));
    cx.simulate_input("@");
    let m = c
        .read_with(cx, |c, _| c.mention.clone())
        .expect("popup open");
    assert_eq!((m.at, m.query.as_str()), (6, ""));
    cx.simulate_input("jo");
    assert_eq!(
        c.read_with(cx, |c, _| c.mention.clone().unwrap().query),
        "jo"
    );
    // Whitespace closes it.
    cx.simulate_input(" ");
    assert!(c.read_with(cx, |c, _| c.mention.is_none()));
    // An `@` inside a word (an e-mail address) does not trigger.
    cx.simulate_input("me@x");
    assert!(c.read_with(cx, |c, _| c.mention.is_none()));
}

/// The cursor the rules see stays inside the text: a cursor past its end, or inside a character, finds no mention instead
/// of panicking, and every mention found starts at or before the cursor, on a character boundary, with its query exactly
/// the text between the `@` and the cursor.
#[test]
fn mention_at_cursor_stays_inside_the_text() {
    for text in ["", "@", "@ab", "é @ada", "x\n@日本語", "a @😀 b", "@a@b", "naïve @zoë"] {
        for cursor in 0..=text.len() + 3 {
            let found = mention_at(text, cursor);
            if cursor > text.len() || !text.is_char_boundary(cursor) {
                assert_eq!(found, None, "{text:?} at {cursor}");
                continue;
            }
            if let Some(m) = found {
                assert!(m.at < cursor, "{text:?} at {cursor}: the @ is before the cursor");
                assert!(text.is_char_boundary(m.at));
                assert_eq!(&text[m.at..m.at + 1], "@");
                assert_eq!(&text[m.at + 1..cursor], m.query, "{text:?} at {cursor}");
            }
        }
    }
}

#[test]
fn mention_at_rules() {
    assert!(mention_at("a@b", 3).is_none());
    assert_eq!(mention_at("@", 1).unwrap().at, 0);
    assert_eq!(mention_at("x\n@al", 5).unwrap().query, "al");
    assert!(mention_at("é @a b", 8).is_none());
}
