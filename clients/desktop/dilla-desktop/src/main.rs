use dilla_client::instances::Instances;
use dilla_client::seams::InstanceId;
use dilla_desktop::{Launch, windows};

/// What the command line asks for. `--instance=<n>` opens only instance `n` (a dev flag: a second copy of the app on
/// another instance beside the first); `--allow-loopback` admits `http://localhost` instances (a debug build only);
/// `--version` / `-V` prints [`version_line`] and exits before any window or display is touched (the release
/// packages' smoke tests run it in containers with no display).
#[derive(Debug, PartialEq, Eq)]
struct Args {
    instance: Option<InstanceId>,
    allow_loopback: bool,
    version: bool,
}

/// `Dilla <version> (<commit>)`: the crate version (equal to the release tag's) and the first 12 characters of the
/// commit the release workflow stamps through `DILLA_BUILD_SHA`, or `dev` for any other build.
fn version_line(build_sha: Option<&str>) -> String {
    let sha = build_sha
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map_or("dev", |s| s.get(..12).unwrap_or(s));
    format!("Dilla {} ({sha})", env!("CARGO_PKG_VERSION"))
}

/// An argument that is not a known flag is ignored (the platform adds its own); a malformed `--instance` is an error.
fn parse_args(args: impl IntoIterator<Item = String>) -> Result<Args, String> {
    let mut parsed = Args {
        instance: None,
        allow_loopback: false,
        version: false,
    };
    for arg in args {
        if let Some(n) = arg.strip_prefix("--instance=") {
            let id: u8 = n
                .parse()
                .map_err(|_| format!("--instance takes a number from 0 to 31, not `{n}`"))?;
            if usize::from(id) >= dilla_client::instances::MAX_INSTANCES {
                return Err(format!("--instance takes a number from 0 to 31, not `{n}`"));
            }
            parsed.instance = Some(InstanceId(id));
        } else if arg == "--allow-loopback" {
            parsed.allow_loopback = cfg!(debug_assertions);
        } else if arg == "--version" || arg == "-V" {
            parsed.version = true;
        }
    }
    Ok(parsed)
}

fn main() {
    #[cfg(feature = "automation")]
    if std::env::args().any(|arg| arg == "--automation") {
        dilla_desktop::automation::run(std::env::args().collect());
        return;
    }
    let args = match parse_args(std::env::args().skip(1)) {
        Ok(args) => args,
        Err(e) => {
            eprintln!("dilla-desktop: {e}");
            std::process::exit(2);
        }
    };
    if args.version {
        println!("{}", version_line(option_env!("DILLA_BUILD_SHA")));
        return;
    }
    // The icons are served from the binary (dilla-desktop-ui theme::Assets).
    gpui_platform::application()
        .with_assets(dilla_desktop_ui::theme::Assets)
        .run(move |cx| {
            // Videos handed to the system player are wiped when the app quits (Task 12); live calls are left, with a
            // bounded wait (Task 28 item 3).
            cx.on_app_quit(|cx| {
                let _ = dilla_desktop::screens::voice::on_quit(cx);
                dilla_desktop::screens::attachments::ui::on_quit(cx);
                async {}
            })
            .detach();
            // Each instance window opens in Task 8's start shape (`open_main_window_with` on a shipping launch) and
            // takes the authentication and workspace shapes from there; one window and one client per instance (27).
            let launch = Launch::os();
            let listed = launch
                .root
                .as_deref()
                .and_then(|root| Instances::load(root).ok())
                .map(|list| list.list().to_vec())
                .unwrap_or_default();
            // `--instance=<n>` opens that instance's window alone; otherwise one window per listed instance, and the
            // manager when there is none (the shell's start-up: `for (instance of instances) openInstance`, then
            // `if (windows.size === 0) showManager()`).
            let wanted: Vec<_> = match args.instance {
                Some(id) => listed.into_iter().filter(|c| c.id == id).collect(),
                None => listed,
            };
            let mut opened = false;
            for cfg in wanted {
                match windows::open_instance(cx, &launch, cfg, None, args.allow_loopback) {
                    Ok(()) => opened = true,
                    Err(e) => eprintln!("dilla-desktop: an instance did not open: {e}"),
                }
            }
            if !opened {
                windows::open_manager(cx, &launch, args.allow_loopback)
                    .expect("the manager window opens");
            }
            cx.activate(true);
        });
}

#[cfg(test)]
mod tests {
    use super::*;

    fn parse(args: &[&str]) -> Result<Args, String> {
        parse_args(args.iter().map(|s| (*s).to_owned()))
    }

    #[test]
    fn instance_flag_takes_zero_to_thirty_one() {
        assert_eq!(parse(&[]).unwrap().instance, None);
        assert_eq!(
            parse(&["--instance=0"]).unwrap().instance,
            Some(InstanceId(0))
        );
        assert_eq!(
            parse(&["--instance=31"]).unwrap().instance,
            Some(InstanceId(31))
        );
        for bad in [
            "--instance=32",
            "--instance=",
            "--instance=-1",
            "--instance=x",
        ] {
            assert!(parse(&[bad]).is_err(), "{bad}");
        }
    }

    #[test]
    fn unknown_arguments_are_ignored() {
        let args = parse(&["--other", "-psn_0_1"]).unwrap();
        assert_eq!(args.instance, None);
        assert!(!args.version);
    }

    #[test]
    fn version_flag_is_recognised_in_both_spellings() {
        assert!(parse(&["--version"]).unwrap().version);
        assert!(parse(&["-V"]).unwrap().version);
        assert!(parse(&["--other", "--version"]).unwrap().version);
        assert!(!parse(&["--versions", "-v"]).unwrap().version);
    }

    #[test]
    fn version_line_names_the_crate_version_and_a_short_commit() {
        let v = env!("CARGO_PKG_VERSION");
        assert_eq!(version_line(None), format!("Dilla {v} (dev)"));
        assert_eq!(version_line(Some("")), format!("Dilla {v} (dev)"));
        assert_eq!(
            version_line(Some("0123456789abcdef0123456789abcdef01234567")),
            format!("Dilla {v} (0123456789ab)")
        );
        assert_eq!(version_line(Some("abc123")), format!("Dilla {v} (abc123)"));
    }
}
