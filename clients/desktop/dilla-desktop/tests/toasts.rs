//! Task 28 (integration minor): a toast shows in the window its action came from. Every caller in the app knows its
//! window (a click's, a view's, a per-window area's `WindowId`), so it shows the toast there with `show_toast_in`;
//! `show_toast` (the window that has the keyboard) would put another instance window's toast in front of the wrong
//! account. `show_toast_in(None, ..)` (every window) has the same effect, so it is flagged too, but for the one call that
//! means it (the toast module's own test of "every window").

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

/// The calls `show_toast_in(None, ..)` that mean every window: `lib.rs`'s test of the per-window toasts.
const EVERY_WINDOW_ALLOWED: &[(&str, &str)] = &[("src/lib.rs", "\"everywhere\"")];

/// The calls of `show_toast_in(None, ..)` in `text` (the file `path`) but the allowed ones, with the window argument on
/// the call's line or the next.
fn every_window_calls(path: &str, text: &str) -> Vec<String> {
    // Comments out, so a doc that names the call is no call.
    let code: Vec<&str> = text
        .lines()
        .map(|l| l.split("//").next().unwrap_or(""))
        .collect();
    let mut found = Vec::new();
    for (n, line) in code.iter().enumerate() {
        let Some(at) = line.find("show_toast_in(") else {
            continue;
        };
        if line.contains("pub fn show_toast_in(") {
            continue;
        }
        let rest = &line[at + "show_toast_in(".len()..];
        let (arg, whole) = if rest.trim().is_empty() {
            let next = code[n + 1..]
                .iter()
                .find(|l| !l.trim().is_empty())
                .copied()
                .unwrap_or("");
            (next.trim(), format!("{} {}", line.trim(), next.trim()))
        } else {
            (rest.trim(), line.trim().to_owned())
        };
        if !arg.starts_with("None") {
            continue;
        }
        let allowed = EVERY_WINDOW_ALLOWED.iter().any(|(file, needle)| {
            path.replace('\\', "/").ends_with(file) && whole.contains(needle)
        });
        if !allowed {
            found.push(format!("{path}:{}: {whole}", n + 1));
        }
    }
    found
}

/// Review L m9: a toast for every window (`show_toast_in(None, ..)`) puts it in front of another instance window's
/// account too.
#[test]
fn no_toast_is_shown_in_every_window() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let mut files = Vec::new();
    sources(&root.join("src"), &mut files);
    sources(&root.join("../dilla-desktop-ui/src"), &mut files);
    let found: Vec<String> = files
        .iter()
        .flat_map(|(path, text)| every_window_calls(path, text))
        .collect();
    assert!(
        found.is_empty(),
        "these toasts show in every window (name the window with Some(..)):\n{}",
        found.join("\n")
    );
}

/// The guard reads the argument on the call's line or the next, and lets only the allowed call through.
#[test]
fn the_every_window_guard_reads_the_argument() {
    let one_line = "    show_toast_in(None, t(\"x\"), cx);";
    let two_lines =
        "    toast::show_toast_in(\n        None,\n        t(\"x\"),\n        cx,\n    );";
    let named =
        "    toast::show_toast_in(\n        Some(window),\n        t(\"x\"),\n        cx,\n    );";
    let commented = "    // show_toast_in(None, ..) would show it everywhere";
    assert_eq!(every_window_calls("src/a.rs", one_line).len(), 1);
    assert_eq!(every_window_calls("src/a.rs", two_lines).len(), 1);
    assert!(every_window_calls("src/a.rs", named).is_empty());
    assert!(every_window_calls("src/a.rs", commented).is_empty());
    let allowed = "            show_toast_in(None, \"everywhere\", cx);";
    assert!(every_window_calls("x/src/lib.rs", allowed).is_empty());
    assert_eq!(every_window_calls("x/src/other.rs", allowed).len(), 1);
}
