//! The fixture as a GPUI view. `m` maps fixture px to logical px so that one fixture px is one device px at the
//! requested scale whatever scale factor the window really has (headless test windows are always 2.0).
use crate::fixture::Fixture;
use gpui::{Context, FontWeight, IntoElement, ParentElement, Render, Styled, Window, div, px, rgb};

pub struct Proof {
    pub fx: Fixture,
    pub m: f32,
}

impl Render for Proof {
    fn render(&mut self, _w: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        let fx = &self.fx;
        let m = self.m;
        let l = &fx.layout;
        let family = fx.font.family.clone();
        let p = |v: f32| px(v * m);

        let mut feed = div().flex().flex_col();
        for msg in &fx.messages {
            let author = &fx.authors[msg.author];
            let head = div()
                .flex()
                .items_center()
                .gap(p(l.head_gap))
                .h(p(l.line_h))
                .child(
                    div()
                        .h(p(l.line_h))
                        .line_height(p(l.line_h))
                        .text_size(p(fx.font.base))
                        .font_weight(FontWeight(600.0))
                        .text_color(rgb(fx.rgb("fg")))
                        .child(author.name.clone()),
                )
                .child(
                    div()
                        .h(p(l.line_h))
                        .line_height(p(l.line_h))
                        .text_size(p(fx.font.small))
                        .text_color(rgb(fx.rgb("fg4")))
                        .child(msg.time.clone()),
                );
            let mut col = div().flex().flex_col().child(head);
            for line in &msg.lines {
                col = col.child(
                    div()
                        .h(p(l.line_h))
                        .line_height(p(l.line_h))
                        .text_size(p(fx.font.base))
                        .text_color(rgb(fx.rgb("fgBody")))
                        .child(line.clone()),
                );
            }
            if !msg.code.is_empty() {
                let mut code = div()
                    .mt(p(l.code_margin_top))
                    .w(p(l.code_w))
                    .px(p(l.code_pad_x))
                    .py(p(l.code_pad_y))
                    .bg(rgb(fx.rgb("bg")))
                    .border(px(m))
                    .border_color(rgb(fx.rgb("hairline2")))
                    .rounded(p(fx.radius))
                    .flex()
                    .flex_col();
                for line in &msg.code {
                    code = code.child(
                        div()
                            .h(p(l.code_line_h))
                            .line_height(p(l.code_line_h))
                            .text_size(p(fx.font.code))
                            .text_color(rgb(fx.rgb("fgBody")))
                            .child(line.clone()),
                    );
                }
                col = col.child(code);
            }
            if !msg.reactions.is_empty() {
                let mut row = div().mt(p(l.react_margin_top)).flex().gap(p(l.react_gap));
                for (label, count) in &msg.reactions {
                    row = row.child(
                        div()
                            .h(p(l.pill_h))
                            .px(p(l.pill_pad_x))
                            .flex()
                            .items_center()
                            .bg(rgb(fx.rgb("surface2")))
                            .border(px(m))
                            .border_color(rgb(fx.rgb("controlEdge")))
                            .rounded(p(999.0))
                            .child(
                                div()
                                    .line_height(p(l.pill_h - 2.0))
                                    .text_size(p(fx.font.pill))
                                    .text_color(rgb(fx.rgb("fg2")))
                                    .child(format!("{label} {count}")),
                            ),
                    );
                }
                col = col.child(row);
            }
            let avatar = div()
                .flex_none()
                .size(p(l.avatar))
                .flex()
                .items_center()
                .justify_center()
                .rounded(p(999.0))
                .bg(rgb(fx.rgb("surfaceHi")))
                .border(px(m))
                .border_color(rgb(fx.rgb("hairline2")))
                .text_size(p(fx.font.small))
                .text_color(rgb(fx.rgb("fg2")))
                .child(author.initials.clone());
            feed = feed.child(
                div()
                    .flex()
                    .gap(p(l.row_gap))
                    .py(p(l.row_pad_y))
                    .child(avatar)
                    .child(col),
            );
        }

        div()
            .size_full()
            .bg(rgb(fx.rgb("bg")))
            .p(p(l.outer_pad))
            .font_family(family)
            .child(
                div()
                    .size_full()
                    .overflow_hidden()
                    .bg(rgb(fx.rgb("surface")))
                    .border(px(m))
                    .border_color(rgb(fx.rgb("hairline2")))
                    .rounded(p(fx.radius))
                    .p(p(l.panel_pad))
                    .child(feed),
            )
    }
}
