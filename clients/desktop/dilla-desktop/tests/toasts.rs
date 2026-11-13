//! Task 28 (integration minor): a toast shows in the window its action came from. Every caller in the app knows its
//! window (a click's, a view's, a per-window area's `WindowId`), so it shows the toast there with `show_toast_in`;
//! `show_toast` (the window that has the keyboard) would put another instance window's toast in front of the wrong
//! account.

use std::path::Path;

fn sources(dir: &Path, out: &mut Vec<(String, String)>) {
    for entry in std::fs::read_dir(dir).unwrap() {
        let path = entry.unwrap().path();
        if path.is_dir() {
            sources(&path, out);
        } else if path.extension().is_some_and(|e| e == "rs") {
            out.push((
                path.display().to_string(),
                std::fs::read_to_string(&path).unwrap(),
            ));
        }
    }
}

#[test]
fn every_toast_names_its_window() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let mut files = Vec::new();
    sources(&root.join("src"), &mut files);
    sources(&root.join("../dilla-desktop-ui/src"), &mut files);
    let mut found = Vec::new();
    for (path, text) in &files {
        for (n, line) in text.lines().enumerate() {
            let code = line.split("//").next().unwrap_or("");
            if code.contains("show_toast(") && !code.contains("pub fn show_toast(") {
                found.push(format!("{path}:{}: {}", n + 1, line.trim()));
            }
        }
    }
    assert!(
        found.is_empty(),
        "these toasts do not name their window (use show_toast_in):\n{}",
        found.join("\n")
    );
}
