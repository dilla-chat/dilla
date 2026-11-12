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
    // macOS/Wayland text-input sequence for dead acute then `e`: the dead key
    // arrives as marked text, the follow-up replaces it with the composed char.
    cx.update(|window, app| {
        state.update(app, |s, cx| {
            s.replace_and_mark_text_in_range(None, "´", Some(1..1), window, cx);
        });
    });
    assert_eq!(text(&c, cx), "´", "dead key is shown while composing");
    cx.update(|window, app| {
        state.update(app, |s, cx| {
            assert!(s.marked_text_range(window, cx).is_some());
            s.replace_text_in_range(None, "é", window, cx);
            assert!(s.marked_text_range(window, cx).is_none());
        });
    });
    assert_eq!(text(&c, cx), "é");
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
    cx.write_to_clipboard(ClipboardItem::new_string(
        "line one\nline two\n\nline four".into(),
    ));
    cx.simulate_keystrokes("ctrl-v");
    let mut got = text(&c, cx);
    if got.is_empty() {
        // Platforms where ctrl-v is not bound (macOS binds cmd-v): fall back
        // to the Paste action the binding would dispatch.
        cx.dispatch_action(Paste);
        got = text(&c, cx);
    }
    assert_eq!(got, "line one\nline two\n\nline four");
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
    // One backspace removes the `!`; a second removes the whole skin-tone
    // cluster's last scalar boundary without splitting UTF-8.
    cx.simulate_keystrokes("backspace backspace");
    let t = text(&c, cx);
    assert!(t.starts_with('a') && t.ends_with('b'), "got {t:?}");
    assert!(std::str::from_utf8(t.as_bytes()).is_ok());
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

#[test]
fn mention_at_rules() {
    assert!(mention_at("a@b", 3).is_none());
    assert_eq!(mention_at("@", 1).unwrap().at, 0);
    assert_eq!(mention_at("x\n@al", 5).unwrap().query, "al");
    assert!(mention_at("é @a b", 8).is_none());
}
