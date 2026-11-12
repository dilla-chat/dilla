//! Window for the manual IME checklist (MANUAL.md): `cargo run --example composer`.
//! Sent messages are printed to stdout; the mention popup state is shown below
//! the composer.

use gpui::{
    AppContext as _, Context, Entity, IntoElement, ParentElement, Render, Styled, Subscription,
    Window, WindowOptions, div,
};
use gpui_component::Root;
use gpui_input_proof::{Composer, Sent};

struct Demo {
    composer: Entity<Composer>,
    _subs: Vec<Subscription>,
}

impl Render for Demo {
    fn render(&mut self, _: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let status = match &self.composer.read(cx).mention {
            Some(m) => format!("mention popup: OPEN, query {:?}, @ at byte {}", m.query, m.at),
            None => "mention popup: closed".to_owned(),
        };
        div()
            .size_full()
            .p_4()
            .flex()
            .flex_col()
            .gap_2()
            .child(self.composer.clone())
            .child(status)
    }
}

fn main() {
    gpui_platform::application().run(|cx| {
        gpui_component::init(cx);
        cx.on_window_closed(|cx, _| {
            if cx.windows().is_empty() {
                cx.quit();
            }
        })
        .detach();
        cx.open_window(WindowOptions::default(), |window, cx| {
            let composer = cx.new(|cx| Composer::new(window, cx));
            let input = composer.read(cx).input.clone();
            input.update(cx, |s, cx| s.focus(window, cx));
            let demo = cx.new(|cx| {
                let subs = vec![
                    cx.observe(&composer, |_, _, cx| cx.notify()),
                    cx.subscribe(&composer, |_, _, Sent(text), _| {
                        println!("SENT: {text:?}");
                    }),
                ];
                Demo {
                    composer,
                    _subs: subs,
                }
            });
            cx.new(|cx| Root::new(demo, window, cx))
        })
        .expect("the window opens");
        cx.activate(true);
    });
}
