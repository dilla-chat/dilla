//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	youtubemusic "github.com/jonasthim/dilla/internal/bot/music/youtube"
	"github.com/jonasthim/dilla/internal/mlswasi"
)

type botDoctorCheck struct {
	name   string
	status string
}

const qualifiedYouTubeYTDLPVersion = "2026.10.21"

var doctorWASI = func(ctx context.Context, binary []byte) error {
	runtime, err := mlswasi.New(ctx, binary, mlswasi.Options{PoolSize: 1})
	if err != nil {
		return err
	}
	return runtime.Close(ctx)
}

var doctorLookPath = exec.LookPath

// runDoctorCommand checks the local files and permissions dillabot needs before
// it can start. It does not contact Dilla or a music provider, and never prints
// credential contents or configured filesystem paths.
func runDoctorCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dillabot doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to the bot service TOML config")
	youtube := flags.Bool("youtube", false, "also check local YouTube extraction and playback tools")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "dillabot doctor: unexpected argument")
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stdout, doctorConfigFailure(*configPath, err))
		return 1
	}
	fmt.Fprintf(stdout, "config OK: read %s\n", *configPath)

	checks := []botDoctorCheck{
		{name: "runtime_credential", status: "OK: protected owner-only credential is available"},
	}
	var youtubeResult strings.Builder
	searchPath := "PATH searched: " + os.Getenv("PATH")
	if checkYouTubeDependencies(&youtubeResult, doctorLookPath) {
		checks = append(checks, botDoctorCheck{name: "youtube_dependencies", status: "OK: " + strings.TrimSpace(youtubeResult.String()) + " (" + searchPath + ")"})
	} else if *youtube {
		checks = append(checks, botDoctorCheck{name: "youtube_dependencies", status: "FAIL: " + strings.TrimSpace(youtubeResult.String()) + " (" + searchPath + ")"})
	} else {
		checks = append(checks, botDoctorCheck{name: "youtube_dependencies", status: "WARN: " + strings.TrimSpace(youtubeResult.String()) + " (" + searchPath + ")"})
	}
	if *youtube {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		checks = append(checks, checkYouTubeSandbox(ctx, doctorLookPath))
		cancel()
	}
	if err := validateHelperExecutable(cfg.HelperExecutable); err != nil {
		checks = append(checks, botDoctorCheck{name: "native_helper", status: "FAIL: executable helper is unavailable or unsafe"})
	} else {
		checks = append(checks, botDoctorCheck{name: "native_helper", status: "OK: executable helper is available"})
	}
	if cfg.SpotifyHelperExecutable != "" {
		if err := validateHelperExecutable(cfg.SpotifyHelperExecutable); err != nil {
			checks = append(checks, botDoctorCheck{name: "spotify_pcm_helper", status: "FAIL: executable helper is unavailable or unsafe"})
		} else {
			checks = append(checks, botDoctorCheck{name: "spotify_pcm_helper", status: "OK: executable helper is available"})
		}
	}
	if binary, readErr := readDoctorWASI(cfg.WASIModule); readErr != nil {
		checks = append(checks, botDoctorCheck{name: "wasi_identity_module", status: "FAIL: module is unavailable or unsafe"})
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		healthErr := doctorWASI(ctx, binary)
		cancel()
		clear(binary)
		if healthErr != nil {
			checks = append(checks, botDoctorCheck{name: "wasi_identity_module", status: "FAIL: module failed startup validation"})
		} else {
			checks = append(checks, botDoctorCheck{name: "wasi_identity_module", status: "OK: module compiles and initializes"})
		}
	}
	if err := validatePrivateStateDir(cfg.StateDir); err != nil {
		checks = append(checks, botDoctorCheck{name: "state_directory", status: "FAIL: directory must be owned by the service account with mode 0700"})
	} else {
		checks = append(checks, botDoctorCheck{name: "state_directory", status: "OK: private service-owned directory is ready"})
	}

	failed := false
	for _, check := range checks {
		fmt.Fprintf(stdout, "%s %s\n", check.name, check.status)
		if len(check.status) >= 4 && check.status[:4] == "FAIL" {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

// doctorConfigFailure tells a missing config file, an invalid config and an
// unusable runtime credential apart. It names the config path the operator
// passed (or the default) and never includes secret material.
func doctorConfigFailure(path string, err error) string {
	switch {
	case errors.Is(err, errConfigMissing):
		return "FAIL config: no such file at " + path + " (pass --config=PATH; the installer writes " + defaultConfigPath + ")"
	case errors.Is(err, errConfigUnreadable):
		return "FAIL config: cannot read " + path + " (check its owner and mode)"
	case errors.Is(err, errCredentialBad):
		return "FAIL runtime_credential: the credential file named in " + path + " is missing, unreadable, not an owner-only 0600 file, or not a valid runtime credential"
	default:
		return "FAIL config: " + path + " is not a valid Bot configuration"
	}
}

// checkYouTubeDependencies is a local prerequisite check only. It does not
// invoke yt-dlp against a provider or test bwrap namespace support.
func checkYouTubeDependencies(stdout io.Writer, lookPath func(string) (string, error)) bool {
	var missing []string
	version := func(name string, args ...string) (string, bool) {
		path, err := lookPath(name)
		if err != nil {
			missing = append(missing, name+" unavailable")
			return "", false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- executable is resolved by LookPath and arguments are fixed prerequisites.
		out := &boundedVersionOutput{remaining: 2048}
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil || ctx.Err() != nil {
			missing = append(missing, name+" did not run")
			return "", false
		}
		return strings.TrimSpace(out.String()), true
	}
	ytdlp, ytdlpOK := version("yt-dlp", "--version")
	if ytdlpOK && ytdlp != qualifiedYouTubeYTDLPVersion {
		missing = append(missing, "yt-dlp version unsupported (expected "+qualifiedYouTubeYTDLPVersion+")")
	}
	node, nodeOK := version("node", "--version")
	if nodeOK {
		var major int
		if _, err := fmt.Sscanf(node, "v%d.", &major); err != nil || major < 22 {
			missing = append(missing, "Node version unsupported (requires >=22)")
		}
	}
	python, pythonOK := version("python3", "--version")
	if pythonOK {
		var major, minor int
		if _, err := fmt.Sscanf(python, "Python %d.%d", &major, &minor); err != nil || major < 3 || (major == 3 && minor < 10) {
			missing = append(missing, "python3 version unsupported (requires >=3.10)")
		}
	}
	for _, tool := range []struct {
		name string
		args []string
	}{{"bwrap", []string{"--version"}}, {"ffmpeg", []string{"-hide_banner", "-h", "encoder=libopus"}}} {
		_, _ = version(tool.name, tool.args...)
	}
	if len(missing) != 0 {
		fmt.Fprintf(stdout, "%s", strings.Join(missing, "; "))
		return false
	}
	fmt.Fprintf(stdout, "yt-dlp %s; Node %s; bwrap, python3 and ffmpeg present", ytdlp, node)
	return true
}

type boundedVersionOutput struct {
	bytes     strings.Builder
	remaining int
}

func (w *boundedVersionOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.remaining {
		n = w.remaining
	}
	if n > 0 {
		_, _ = w.bytes.Write(p[:n])
		w.remaining -= n
	}
	return len(p), nil
}

func (w *boundedVersionOutput) String() string { return w.bytes.String() }

func readDoctorWASI(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, os.ErrInvalid
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "wasi-module")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 64<<20 || !trustedLocalCode(info) {
		return nil, os.ErrInvalid
	}
	return io.ReadAll(f)
}

func validatePrivateStateDir(path string) error {
	if !filepath.IsAbs(path) {
		return os.ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return os.ErrPermission
	}
	return nil
}

// checkYouTubeSandbox exercises the real isolated runner without provider access.
func checkYouTubeSandbox(ctx context.Context, lookPath func(string) (string, error)) botDoctorCheck {
	check := botDoctorCheck{name: "youtube_sandbox"}
	paths := make(map[string]string)
	for _, name := range []string{"bwrap", "python3", "yt-dlp"} {
		path, err := lookPath(name)
		if err != nil {
			check.status = "FAIL: " + name + " unavailable"
			return check
		}
		paths[name] = path
	}
	runner := youtubemusic.IsolatedMetadataRunner{Bubblewrap: paths["bwrap"], Python: paths["python3"]}
	_, err := runner.Run(ctx, paths["yt-dlp"], []string{"--version"}, 2048)
	if err != nil {
		check.status = "FAIL: " + youtubemusic.FailureClass(err)
		if stderr := youtubemusic.FailureStderr(err); stderr != "" {
			check.status += ": " + stderr
		}
	} else {
		check.status = "OK: isolated yt-dlp --version ran without provider access"
	}
	return check
}
