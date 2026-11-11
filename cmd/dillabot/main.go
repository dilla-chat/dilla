//go:build linux

// Command dillabot reconciles managed bot identity, authorized MLS groups,
// paused music queue metadata, and durable group-event moderation handling.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/dilla-chat/dilla/internal/bot/announcements"
	"github.com/dilla-chat/dilla/internal/bot/client"
	"github.com/dilla-chat/dilla/internal/bot/gatewayclient"
	"github.com/dilla-chat/dilla/internal/bot/helper"
	"github.com/dilla-chat/dilla/internal/bot/moderation"
	"github.com/dilla-chat/dilla/internal/bot/music"
	"github.com/dilla-chat/dilla/internal/bot/music/session"
	"github.com/dilla-chat/dilla/internal/bot/native"
	"github.com/dilla-chat/dilla/internal/bot/service"
	"github.com/dilla-chat/dilla/internal/clock"
	"github.com/dilla-chat/dilla/internal/id"
	"github.com/dilla-chat/dilla/internal/mlswasi"
)

const maxConfigBytes = 64 << 10
const maxCredentialBytes = 4096

// Version is stamped by the release workflow. Development builds retain the
// explicit fallback so operators can distinguish them from tagged releases.
var Version = "0.0.0-dev"

type config struct {
	RateLimits            client.RateLimits `toml:"rate_limits"`
	BaseURL               string            `toml:"base_url"`
	InstanceIDText        string            `toml:"instance_id"`
	RuntimeCredentialFile string            `toml:"runtime_credential_file"`
	HelperExecutable      string            `toml:"helper_executable"`
	WASIModule            string            `toml:"wasi_module"`
	StateDir              string            `toml:"state_dir"`
	ReconcileIntervalText string            `toml:"reconcile_interval"`
	AllowLoopbackHTTP     bool              `toml:"allow_loopback_http"`
	InstanceID            id.ID             `toml:"-"`
	ReconcileInterval     time.Duration     `toml:"-"`
	// Socket is dillad's Bot socket when the supervisor started this process (supervised.go); "" for a Bot that
	// reaches dillad at base_url.
	Socket string `toml:"-"`
	// launch is the supervised Bot's launch token; nil otherwise.
	launch *gatewayclient.LaunchToken
}

// httpClient is the Bot's HTTP client: over dillad's Bot socket when supervised, the network otherwise. timeout 0
// leaves requests unbounded (the gateway's upgrade bounds itself).
func (c config) httpClient(timeout time.Duration) *http.Client {
	if c.Socket == "" {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: unixTransport(c.Socket)}
}

// defaultConfigPath is where install-bot.sh writes the config and where the
// systemd unit reads it.
const defaultConfigPath = "/etc/dillabot/bot.toml"

var (
	errConfigMissing    = errors.New("config file does not exist")
	errConfigUnreadable = errors.New("config file is unavailable")
	errConfigInvalid    = errors.New("config file is invalid")
	errCredentialBad    = errors.New("runtime credential file is unavailable or unsafe")
)

func loadConfig(path string) (config, error) {
	var c config
	if !filepath.IsAbs(path) {
		return c, errors.New("config path must be absolute")
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the explicit absolute --config operator argument.
	if errors.Is(err, os.ErrNotExist) {
		return c, errConfigMissing
	}
	if err != nil {
		return c, errConfigUnreadable
	}
	if len(raw) > maxConfigBytes {
		return c, fmt.Errorf("%w: exceeds size limit", errConfigInvalid)
	}
	decoder := toml.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	// Accept and discard only the retired keys from older installed files.
	// Unknown fields, including misspelled legacy keys, remain configuration errors.
	legacy := struct {
		*config
		RetiredClientID    string `toml:"spotify_client_id"`
		RetiredRedirectURL string `toml:"spotify_redirect_url"`
		RetiredHelper      string `toml:"spotify_helper_executable"`
	}{config: &c}
	err = decoder.Decode(&legacy)
	clear(raw)
	if err != nil {
		return c, errConfigInvalid
	}
	var errs []error
	if err := c.RateLimits.Validate(); err != nil {
		errs = append(errs, errors.New("rate_limits requires positive finite rates and bounded cache capacities"))
	}
	if c.BaseURL == "" {
		errs = append(errs, errors.New("base_url is required"))
	}
	c.InstanceID, err = id.Parse(c.InstanceIDText)
	if err != nil || c.InstanceID.IsZero() {
		errs = append(errs, errors.New("instance_id must be a non-zero lowercase 32-character ID"))
	}
	for name, value := range map[string]string{
		"runtime_credential_file": c.RuntimeCredentialFile,
		"helper_executable":       c.HelperExecutable,
		"wasi_module":             c.WASIModule,
		"state_dir":               c.StateDir,
	} {
		if value == "" || !filepath.IsAbs(value) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path", name))
		}
	}
	c.ReconcileInterval, err = time.ParseDuration(c.ReconcileIntervalText)
	if err != nil || c.ReconcileInterval < time.Second || c.ReconcileInterval > 24*time.Hour {
		errs = append(errs, errors.New("reconcile_interval must be between 1s and 24h"))
	}
	if len(errs) != 0 {
		return config{}, fmt.Errorf("%w: %w", errConfigInvalid, errors.Join(errs...))
	}
	credential, err := readCredential(c.RuntimeCredentialFile)
	if err != nil {
		return config{}, errCredentialBad
	}
	clear(credential)
	return c, nil
}

func readCredential(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("credential path must be absolute")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("credential file is unavailable or unsafe")
	}
	f := os.NewFile(uintptr(fd), "runtime-credential")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, errors.New("credential file is unavailable or unsafe")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0o600 {
		return nil, errors.New("credential file must be an owner-only 0600 regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCredentialBytes+1))
	if err != nil || len(raw) > maxCredentialBytes {
		clear(raw)
		return nil, errors.New("credential file cannot be read")
	}
	token := strings.TrimSpace(string(raw))
	clear(raw)
	encoded := strings.TrimPrefix(token, "dbr_")
	decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(encoded)
	canonical := decodeErr == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == encoded
	clear(decoded)
	if !strings.HasPrefix(token, "dbr_") || !canonical || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("credential file must contain one runtime credential")
	}
	return []byte(token), nil
}

func unixSecondsOrZero(now time.Time) uint64 {
	seconds := now.Unix()
	if seconds < 0 {
		return 0
	}
	return uint64(seconds)
}

func loadDatabaseKey(root, installation string) ([]byte, string, error) {
	if !filepath.IsAbs(root) || len(installation) != 32 {
		return nil, "", errors.New("invalid state directory or installation ID")
	}
	if err := ensurePrivateDir(root); err != nil {
		return nil, "", err
	}
	installDir := filepath.Join(root, installation)
	if err := ensurePrivateDir(installDir); err != nil {
		return nil, "", err
	}
	path := filepath.Join(installDir, "database.key")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == nil {
		f := os.NewFile(uintptr(fd), "database-key")
		defer func() { _ = f.Close() }()
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, "", errors.New("database key is unavailable or unsafe")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0o600 {
			return nil, "", errors.New("database key must be an owner-only 0600 regular file")
		}
		key, readErr := io.ReadAll(io.LimitReader(f, 33))
		if readErr != nil || len(key) != 32 {
			clear(key)
			return nil, "", errors.New("database key must contain exactly 32 bytes")
		}
		return key, path, nil
	}
	if !errors.Is(err, syscall.ENOENT) {
		return nil, "", errors.New("database key is unavailable or unsafe")
	}
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return nil, "", errors.New("installation state directory cannot be inspected")
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "state.db" || name == "state.db-wal" || name == "state.db-shm" || name == "state.db-journal" || strings.HasPrefix(name, "state.db.init-") || name == "state.init" || strings.HasPrefix(name, "state.init-tmp-") {
			return nil, "", errors.New("database key is missing while encrypted state exists; refusing to replace it")
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		clear(key)
		return nil, "", errors.New("database key generation failed")
	}
	fd, err = syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		clear(key)
		return nil, "", errors.New("database key could not be created")
	}
	f := os.NewFile(uintptr(fd), "database-key")
	if _, err = f.Write(key); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		if dir, openErr := os.Open(installDir); openErr == nil { // #nosec G304 -- installDir is derived under the private state directory.
			err = dir.Sync()
			_ = dir.Close()
		} else {
			err = openErr
		}
	}
	if err != nil {
		clear(key)
		return nil, "", errors.New("database key could not be persisted")
	}
	return key, path, nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("state directory is unavailable")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("state directory is unavailable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0o077 != 0 {
		return errors.New("state directories must be owned by the service account and inaccessible to group/other")
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- this is a private directory and needs owner execute permission.
			return errors.New("state directory permissions could not be secured")
		}
	}
	return nil
}

func validateHelperExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("helper executable path must be absolute")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("native helper executable is unavailable")
	}
	f := os.NewFile(uintptr(fd), "native-helper")
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !trustedLocalCode(info) || !serviceCanExecute(info) {
		return errors.New("native helper must be a regular executable file")
	}
	return nil
}

func trustedLocalCode(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == 0 || int64(st.Uid) == int64(os.Geteuid())) && info.Mode().Perm()&0o022 == 0 && info.Mode()&(os.ModeSetuid|os.ModeSetgid) == 0
}

func serviceCanExecute(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	if os.Geteuid() == 0 {
		return mode&0o111 != 0
	}
	if int64(st.Uid) == int64(os.Geteuid()) {
		return mode&0o100 != 0
	}
	if int64(st.Gid) == int64(os.Getegid()) {
		return mode&0o010 != 0
	}
	groups, err := os.Getgroups()
	if err != nil {
		return false
	}
	for _, group := range groups {
		if int64(st.Gid) == int64(group) {
			return mode&0o010 != 0
		}
	}
	return mode&0o001 != 0
}

func main() {
	if runVersionCommand(os.Args[1:], os.Stdout) {
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctorCommand(os.Args[2:], os.Stdout, os.Stderr))
	}
	// SIGHUP is dillad's "your installations changed" (Supervisor.Nudge). It is caught before anything else, so the
	// default action (exit) cannot end a Bot that dillad nudges while it starts.
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The service takes its configuration from dillad's supervisor (its environment, fd 3 and its state directory) or,
	// for development and tests against an instance without a supervisor, from the --remote flags (remote.go). There
	// is no --config and no bot.toml (Bot v2 task 14).
	var cfg config
	var err error
	if supervisedMode() {
		cfg, err = loadSupervisedConfig(ctx)
	} else {
		cfg, err = loadRemoteConfig(ctx, os.Args[1:], os.Stderr)
	}
	if err != nil {
		logger.Error("bot service configuration failed", "error", err.Error())
		stop()
		os.Exit(2) //nolint:gocritic // exitAfterDefer: stop already ran
	}
	if err := run(ctx, cfg, logger, reload); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("bot service stopped", "error", err.Error())
		stop()
		os.Exit(1)
	}
}

func runVersionCommand(args []string, stdout io.Writer) bool {
	if len(args) != 1 || args[0] != "version" {
		return false
	}
	fmt.Fprintf(stdout, "dillabot %s\n", Version)
	return true
}

func run(ctx context.Context, cfg config, logger *slog.Logger, reload <-chan os.Signal) error {
	credential, err := readCredential(cfg.RuntimeCredentialFile)
	if err != nil {
		return errors.New("runtime credential could not be loaded")
	}
	defer clear(credential)
	wasmBinary, err := readDoctorWASI(cfg.WASIModule)
	if err != nil {
		return errors.New("wasi identity module is unavailable")
	}
	defer clear(wasmBinary)
	if err := ensurePrivateDir(cfg.StateDir); err != nil {
		return err
	}
	if err := validateHelperExecutable(cfg.HelperExecutable); err != nil {
		return err
	}
	logger.Info("bot service starting", "instance_id", cfg.InstanceID.String(), "identity_reconciliation", "enabled", "group_sync", "enabled", "events", "durable_poll_welcome_interaction_and_moderation_dispatch", "music", "paused_restoration_with_configured_pcm_starters")
	wasm, err := mlswasi.New(ctx, wasmBinary, mlswasi.Options{PoolSize: 1, Priority: client.RequestPriority})
	if err != nil {
		return errors.New("wasi identity module failed startup validation")
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = wasm.Close(closeCtx)
	}()
	api, err := client.New(client.Config{
		BaseURL: cfg.BaseURL, InstanceID: cfg.InstanceID,
		AllowLoopbackHTTP: cfg.AllowLoopbackHTTP,
		Timeout:           30 * time.Second,
		RateLimits:        cfg.RateLimits,
		HTTPClient:        cfg.httpClient(30 * time.Second),
	})
	if err != nil {
		return errors.New("bot HTTP client configuration is invalid")
	}
	discovery, err := api.Discover(ctx)
	if err != nil || discovery.InstanceID != cfg.InstanceID {
		return errors.New("configured Dilla instance discovery failed")
	}
	runtime, err := api.Runtime(string(credential))
	if err != nil {
		return errors.New("runtime credential could not be bound to the client")
	}
	logger.Info("bot service healthy", "instance_id", discovery.InstanceID.String(), "generation", discovery.Generation, "discovery", "ok")
	musicRegistry := newMusicQueueRegistry()
	defer func() {
		if err := musicRegistry.Close(); err != nil {
			logger.Error("bot music queue shutdown incomplete", "error", err.Error())
		}
	}()
	gateways := newBotGatewayRuntime(ctx, api, cfg.AllowLoopbackHTTP, logger)
	if cfg.Socket != "" {
		gateways.httpClient, gateways.launch = cfg.httpClient(0), cfg.launch
	}
	defer gateways.Close()
	statuses := newRuntimeStatusTracker(Version)
	// Bot v2 task 14: no reconcile interval. A pass runs at start and then on an event (event_loop.go).
	loop := botLoop{
		clock: clock.System(), log: logger,
		pass: func(passCtx context.Context, targets map[id.ID]bool) error {
			return reconcile(passCtx, cfg, api, runtime, wasm, musicRegistry, gateways, statuses, logger, targets)
		},
		passes: gateways.Pass(), take: gateways.takePassTargets, reload: reload,
		wake: gateways.Wake(), poll: gateways.PollText,
		maintain: func(maintainCtx context.Context) error {
			return gateways.maintainDue(client.WithBackground(maintainCtx))
		},
		due: dueInterval,
	}
	return loop.run(ctx)
}

// reconcile runs one pass: over targets, or over every installation when targets is nil (fix round 1, I-3: an event
// of one installation runs that installation only). The installation list itself is always read.
func reconcile(ctx context.Context, cfg config, api *client.Client, runtime *client.Runtime, wasm *mlswasi.Runtime, musicRegistry *musicQueueRegistry, gateways *botGatewayRuntime, statuses *runtimeStatusTracker, logger *slog.Logger, targets map[id.ID]bool) error {
	// Refresh inventory before retained maintenance so removed installations are
	// pruned before their tasks can run.
	installations, err := runtime.ListInstallations(ctx)
	if err != nil {
		if refusal := reconciliationRateLimit(err); refusal != nil {
			logBotRateLimit(logger, id.ID{}, refusal)
			return nil
		}
		return errors.New("installation discovery failed")
	}
	activeInstallations := make([]id.ID, 0, len(installations))
	activeCommunities := make(map[id.ID]bool, len(installations))
	for _, installation := range installations {
		if installation.State < 4 {
			activeInstallations = append(activeInstallations, installation.ID)
			activeCommunities[installation.CommunityID] = true
		}
	}
	// A removed installation's post gate goes with it (Task 17 fix round 2, M1); its community then posts nowhere.
	service.RetainPostGates(activeCommunities)
	// Retire command contexts before helper/voice teardown can block.
	gateways.RetainOnly(activeInstallations)
	if err := musicRegistry.RetainOnly(activeInstallations); err != nil {
		logger.Error("stale bot music runtime cleanup incomplete", "error", err.Error())
	}
	statuses.RetainOnly(activeInstallations)
	maintained := make(map[id.ID]bool)
	maintenanceErr := gateways.maintain(ctx, maintained)
	var eligible, succeeded, failed int
	// unsucceeded is the eligible installations this pass did not reconcile: the ones the loop retries.
	unsucceeded := map[id.ID]bool{}
	// incomplete is the installations to run again, each with its delay (0: the loop's joining backoff).
	incomplete := map[id.ID]time.Duration{}
	for _, installation := range installations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if targets != nil && !targets[installation.ID] {
			continue
		}
		if installation.State >= 4 {
			_ = musicRegistry.Disable(installation.ID, helper.ReasonInstallationGone)
			continue
		}
		eligible++
		unsucceeded[installation.ID] = true
		// Task 10c: a manager's Restart. Tear down everything this process holds for the
		// installation (native helper, music queues and voice rooms,
		// gateway connection); the rest of this pass starts them again from durable state.
		if statuses.Observe(installation) {
			logger.Info("bot installation restarting on a manager's request", "installation_id", installation.ID.String())
			gateways.Drop(installation.ID)
			if err := musicRegistry.DisableAndWait(ctx, installation.ID); err != nil {
				logger.Error("bot installation restart deferred until native helper closes", "installation_id", installation.ID.String(), "error", err)
				failed++
				continue
			}
		}
		musicRegistry.SetMusicDefaults(installation.ID, installation.Widened, installation.MusicVolume, installation.MusicIdleMinutes)
		if err := statuses.Report(ctx, runtime, installation); err != nil {
			if client.IsDeferred(err) {
				if refusal := reconciliationRateLimit(err); refusal != nil {
					logBotRateLimit(logger, installation.ID, refusal)
				}
				succeeded++
				delete(unsucceeded, installation.ID)
				continue
			}
			logger.Warn("bot runtime status report deferred", "installation_id", installation.ID.String())
		}
		oldTextPoller := gateways.TextPoller(installation.ID)
		// Keep the current callbacks live during slow dependency rebuilding.
		// Successful publication swaps in the callbacks for the rebuilt runtime.
		nativeClient, retainedNative := retainedNativeClient(musicRegistry, installation.ID, installation.CommunityID)
		if !retainedNative {
			// Fix round, I-2: the helper starts off the loop (helper_start.go). Its Open may run the one-off v1 cleanup
			// for minutes; meanwhile the loop serves every other installation, and this one waits for the pass its
			// start asks for when it ends.
			var pending bool
			nativeClient, pending, err = passHelper(&gateways.starts, installation.ID)
			if err != nil {
				_ = musicRegistry.Disable(installation.ID, helper.ReasonStartDropped)
				logger.Error("bot installation unavailable", "installation_id", installation.ID.String(), "stage", "helper_start", "error_class", helper.Class(err))
				continue
			}
			if nativeClient == nil && !pending {
				key, _, keyErr := loadDatabaseKey(cfg.StateDir, installation.ID.String())
				if keyErr != nil {
					_ = musicRegistry.Disable(installation.ID, helper.ReasonStartDropped)
					logger.Error("bot installation unavailable", "installation_id", installation.ID.String(), "stage", "state_key")
					continue
				}
				startOptions := helper.Options{Executable: cfg.HelperExecutable, StateDir: filepath.Join(cfg.StateDir, installation.ID.String()), Logger: logger.With("installation_id", installation.ID.String())}
				binding := native.Binding{Instance: cfg.InstanceID, Community: installation.CommunityID, Installation: installation.ID}
				beginPassHelper(&gateways.starts, installation.ID, key, func(startCtx context.Context, key []byte) (*native.Client, error) {
					return native.Start(startCtx, startOptions, binding, key)
				})
				logger.Info("bot native helper starting", "installation_id", installation.ID.String())
				pending = true
			}
			if pending {
				// Not a failure: the pass the start asks for when it ends reconciles the installation.
				succeeded++
				delete(unsucceeded, installation.ID)
				incomplete[installation.ID] = helperStartRecheck
				continue
			}
		}
		// The gateway starts as soon as the device session is joining (OnSession): every later call of the session is a
		// request frame on it (Bot v2 task 14).
		installationID, communityID := installation.ID, installation.CommunityID
		lifecycle, err := service.NewIdentityLifecycle(cfg.InstanceID, installation.CommunityID, installation.ID, nativeClient, runtime, api, service.LifecycleOptions{
			OnSession: func(user, device id.ID, session *client.Session) error {
				return gateways.Ensure(installationID, communityID, device, user, session.BearerToken())
			},
		})
		if err != nil {
			if retainedNative {
				gateways.Drop(installation.ID)
			} else {
				_ = nativeClient.CloseWithReason(helper.ReasonLifecycleBind)
			}
			_ = musicRegistry.Disable(installation.ID, helper.ReasonLifecycleBind)
			logger.Error("bot installation unavailable", "installation_id", installation.ID.String(), "stage", "lifecycle_bind")
			continue
		}
		var runtimeOwnedNative bool
		var musicQueueCount int
		reconcileStage := "identity"
		result, ensureErr := lifecycle.Ensure(ctx)
		ready := false
		membershipReady := false
		var gatewayErr error
		if ensureErr == nil {
			if result.Installation.BotDevice != nil && result.Installation.BotUser != nil && result.Session != nil {
				if err := gateways.Ensure(installation.ID, installation.CommunityID, *result.Installation.BotDevice, *result.Installation.BotUser, result.Session.BearerToken()); err != nil {
					// A gateway that cannot start is a failure of this installation's pass, not a transient gap: the
					// socket-only routes below answer ErrNoGateway (deferred), which must not hide it.
					gatewayErr = fmt.Errorf("bot gateway startup: %w", err)
					logger.Warn("bot gateway startup unavailable", "installation_id", installation.ID.String(), "err", err.Error())
				}
				// The feed handler is bound now, with the helper only (fix round 1, M-2): device lists, revocations
				// included, are ingested while the installation is still joining; membership work binds the rest later.
				gateways.feedFor(installation.ID, installation.CommunityID, *result.Installation.BotUser, feedDeps{core: nativeClient})
			}
			reconcileStage = "membership_source_sync"
			var membership *service.MembershipReconciler
			membership, ensureErr = service.NewMembershipReconciler(nativeClient, service.SourceSyncOptions{}, 32)
			if ensureErr == nil {
				// Bot v2 Task 16: the Bot stays in Bot channel groups only, as its feed last applied them.
				membership.UseBotChannels(gateways.BotChannels(installation.ID), logger.With("installation_id", installation.ID.String()))
				// Bot v2 Task 17: the same marks gate every post the Bot makes, at the send layer.
				service.SetPostGate(installation.CommunityID, gateways.BotChannels(installation.ID))
			}
			if ensureErr == nil {
				{
					ready, reconcileStage, ensureErr = reconcileReadiness(ctx, result.Installation, result.Session, membership)
					if ensureErr == nil {
						// A 409 means required MLS joins are still pending. Keep
						// reconciling membership, but do not call Ready-only bot APIs.
						if ready {
							membershipReady = true
							if err := gateways.SubscribeNativeGroups(ctx, installation.ID, nativeClient); err != nil {
								logger.Warn("bot gateway native group discovery unavailable", "installation_id", installation.ID.String())
							}
							// Bot v2 Task 14: dillad pushes membership, roles, device lists, Bot channels and channel groups as
							// op 65 on this Bot's gateway connection; the installation's feed handler (feed_handlers.go) gets
							// this pass's helper and session here, and the feed applies what it buffered.
							members, membersErr := result.Session.CommunityMembers(installation.CommunityID)
							if membersErr != nil {
								logger.Info("bot member welcome unavailable", "installation_id", installation.ID.String(), "stage", "member_lookup")
							} else {
								welcomeHandler, welcomeErr := newWelcomeHandler(nativeClient, result.Session, api, installation.CommunityID, installation.ID, result.Installation.Capabilities)
								if welcomeErr != nil {
									logger.Warn("bot welcome and join role unavailable", "installation_id", installation.ID.String(), "error", welcomeErr.Error())
								}
								botUser := id.ID{}
								if result.Installation.BotUser != nil {
									botUser = *result.Installation.BotUser
								}
								gateways.feedFor(installation.ID, installation.CommunityID, botUser, feedDeps{
									core: nativeClient, episodes: members, welcome: welcomeHandler,
								})
							}
							var musicDeferred error
							var musicInteractions *service.MusicInteractions
							var musicProviderStatus service.MusicProviderStatusInteractions
							if result.Installation.Capabilities&botMusicCapability != 0 {
								musicProviderStatus = newMusicProviderStatusInteractions(installation.ID, installation.CommunityID, result.Session)
								// Bot v2 Task 19 fix round 1: one installation row per installation, read again after an op 54, and the
								// feed-state permissions; each room's guards follow its call group and admit every epoch it publishes in.
								musicInstallationStore := runtimeMusicAuthorityStore{runtime: runtime, session: result.Session, installation: installation.ID, community: installation.CommunityID, recent: &recentMusicInstallation{}}
								musicAuthz := gateways.musicAuthz(installation.ID, installation.CommunityID)
								musicBotUser := id.ID{}
								if result.Installation.BotUser != nil {
									musicBotUser = *result.Installation.BotUser
								}
								musicSourceSync, musicSyncErr := service.NewSourceSync(nativeClient, service.SourceSyncOptions{})
								if musicSyncErr != nil {
									logger.Warn("bot music call catch-up unavailable", "installation_id", installation.ID.String())
								}
								channels, channelsErr := result.Session.Channels(ctx, installation.CommunityID)
								if channelsErr == nil {
									musicPolicy := &musicPolicyCache{installation: installation.ID}
									starterFor := func(channel id.ID) (session.QueueStarter, error) {
										catchUp := musicCallCatchUp{sync: musicSourceSync, session: result.Session, community: installation.CommunityID, channel: channel,
											local: nativeClient.InspectGroup, remote: result.Session.GroupInfo}
										joinCallGroup := func(joinCtx context.Context) error {
											// Bot v2 Task 19: the policy version is read once per op 54, not per join (fix round 1, M2); an
											// already-member join catches the call group up first (C1).
											joinCtx = withMusicCatchUp(joinCtx, catchUp.run)
											return musicPolicy.joinWithPolicy(joinCtx, func(readCtx context.Context) (uint64, error) {
												communityRow, communityErr := result.Session.Community(readCtx, installation.CommunityID)
												return communityRow.PolicyVersion, communityErr
											}, func(ctx context.Context, policy uint64) error {
												return ensureMusicCallGroupJoined(ctx, result.Session, nativeClient, gateways.musicJoinTrust(installation.ID), installation.ID, installation.CommunityID, channel, policy)
											})
										}
										guards := newMusicCallGuards(installation.ID, catchUp,
											musicAdmission{community: installation.CommunityID, channel: channel, botUser: musicBotUser, authz: musicAuthz,
												trust: gateways.musicJoinTrust(installation.ID), ready: installationMusicReady(musicInstallationStore)},
											func(subCtx context.Context, groups []id.ID) {
												gateways.SubscribeGroups(subCtx, installation.ID, groups)
											})
										starter, starterErr := newInstallationMusicQueueStarter(cfg.InstanceID, installation.ID, installation.CommunityID, channel, result.Session, nativeClient, nil, guards, joinCallGroup)
										if starterErr == nil {
											starter.IdleLeaveAfter = func() time.Duration { return musicRegistry.IdleLeave(installation.ID) }
											device := id.ID{}
											if result.Installation.BotDevice != nil {
												device = *result.Installation.BotDevice
											}
											starter.Logger = logger.With("installation_id", installation.ID.String(), "device_id", device.String(), "channel_id", channel.String())
											// Bot v2 Task 19: no LogContext call read per start (it sat on the cold /play's path behind the Bot's
											// read limiter); the start tags its logs with the call group once the call is prepared.
											notices := musicRegistry.SkipNotices(installation.ID)
											starter.OnSkipped = func(skipCtx context.Context, skips []music.SkippedTrack) { notices.Notify(skipCtx, channel, skips) }
											starter.OnCallEnded = func() { notices.Clear(channel) }
											// Bot v2 Task 17: the room's members hear that the queue or the playing track changed.
											// Fix round 2 (I4): asynchronous and coalesced, so the supervisor's save never waits on the socket.
											hints := client.NewAsyncHints(client.SessionHints{Session: result.Session})
											starter.OnStateChange = func(changeCtx context.Context) {
												_ = hints.Hint(changeCtx, client.StateScopeCall, channel)
											}
											if starter.SetCallEnded != nil {
												starter.SetCallEnded(starter.OnCallEnded)
											}
										}
										return starter, starterErr
									}
									runtimeOwnedNative, channelsErr = musicRegistry.ReconcileWithStarterFactory(ctx, installation.ID, installation.CommunityID, result.Installation.Capabilities, channels, nativeClient, starterFor)
								}
								if channelsErr != nil {
									musicDeferred = cleanupFailedMusicRestore(logger, installation.ID, installationGone(result.Installation), channelsErr, musicRegistry)
									ensureErr = errors.Join(ensureErr, channelsErr) //nolint:nilnesserr // false positive: channelsErr is non-nil here (the != nil guard above), the analyzer loses it across the reassigning if block
								} else {
									var musicErr error
									musicInteractions, musicErr = newRuntimeMusicInteractions(installation.ID, installation.CommunityID, result.Installation.Capabilities, runtime, result.Session, musicRegistry, logger, musicInstallationStore, musicAuthz, musicBotUser)
									if musicErr != nil {
										logger.Warn("bot music interaction handlers unavailable", "installation_id", installation.ID.String(), "error", musicErr.Error())
									}
									if musicInteractions != nil {
										if result.Installation.BotDevice != nil {
											musicInteractions.DeviceID = *result.Installation.BotDevice
										}
										musicInteractions.DefaultVolume = func() uint8 { return musicRegistry.DefaultVolume(installation.ID) }
									}
								}
							}
							eventPoller, interactionErr := service.NewGroupEventPoller(nativeClient)
							if musicDeferred != nil {
								// Keep the prior music handlers until their dependencies can be rebuilt.
								interactionErr = errors.Join(interactionErr, musicDeferred)
							}
							var scheduler *announcements.Scheduler
							var utilities *utilityRuntime
							var scheduleErr error
							scheduler, scheduleErr = service.NewAnnouncementScheduler(nativeClient, result.Session, api)
							if scheduleErr != nil {
								interactionErr = errors.Join(interactionErr, scheduleErr)
							}
							if interactionErr == nil {
								var utilitySetupErr error
								utilities, utilitySetupErr = newUtilityRuntime(nativeClient, result.Session, api, installation.CommunityID, installation.ID, time.Now)
								if utilitySetupErr != nil {
									logger.Warn("bot utility handlers unavailable", "installation_id", installation.ID.String(), "error", utilitySetupErr.Error())
								}
								utilities.gateCreation(currentInstallationCapabilities(runtime, installation.ID, installation.CommunityID))
							}
							if interactionErr == nil {
								var coordinator *service.SendCoordinator
								coordinator, interactionErr = service.NewSendCoordinator(nativeClient, api)
								if interactionErr == nil {
									{
										// The sealed intake (Bot v2 Task 15) serves the feature handlers; it is configured once the
										// helper is retained by the runtime.
										var sealedConfig sealedIntakeConfig
										var caseSink *service.ModerationCaseSink
										caseSink, interactionErr = service.NewModerationCaseSink(nativeClient, time.Now)
										if interactionErr == nil {
											var caseReader *service.ModerationCaseReader
											caseReader, interactionErr = service.NewModerationCaseReader(caseSink, result.Session)
											if interactionErr == nil {
												var announcementAuthority *service.CommunityActorAuthority
												announcementAuthority, interactionErr = service.NewCommunityActorAuthority(result.Session, installation.CommunityID, nil)
												if interactionErr == nil {
													handlers := runtimeInteractionHandlers(
														service.OverviewInteractions{Authority: result.Session, CommunityID: installation.CommunityID},
														service.AnnouncementInteractions{Scheduler: scheduler, Authority: announcementAuthority, CommunityID: installation.CommunityID},
														service.ModerationInteractions{Reader: caseReader, CommunityID: installation.CommunityID,
															InstallationID: installation.ID, ManualRemovalLookup: manualRemovalLookup{Session: result.Session},
															EvidenceWriter:      moderation.NewEvidenceWriter(nativeClient),
															EvidenceReader:      moderation.NewEvidenceReader(nativeClient, result.Session),
															ManualWarningLookup: manualWarningLookup{Session: result.Session}},
														musicInteractions,
														nativeClient,
														musicProviderStatus,
													)
													for operation, handler := range runtimeRolePanelHandlers(result.Installation.Capabilities, service.RolePanelInteractions{
														Authority:   result.Session,
														Destination: service.WelcomeDestinationResolver{Session: result.Session, Native: nativeClient, Feature: "role_panels"},
														Sender:      coordinator, Discovery: api, Session: result.Session, CommunityID: installation.CommunityID, Now: time.Now,
													}) {
														handlers[operation] = handler
													}
													// Jonas 2026-10-28: /play that starts music is announced publicly with the live player card.
													musicAnnouncements := service.MusicAnnouncements{
														Music: musicInteractions, Authority: result.Session, Channels: result.Session, Limiter: &service.MusicAnnounceLimiter{},
														Destination: service.WelcomeDestinationResolver{Session: result.Session, Native: nativeClient, Feature: "music"},
														Sender:      coordinator, Discovery: api, Session: result.Session, CommunityID: installation.CommunityID, Now: time.Now,
														Notices: musicRegistry.SkipNotices(installation.ID),
													}
													musicAnnouncements.Notices.SetState(nativeClient)
													musicAnnouncements.Notices.Configure(musicAnnouncements, logger.With("installation_id", installation.ID.String()))
													for operation, handler := range runtimeMusicAnnounceHandlers(musicInteractions, musicAnnouncements) {
														handlers[operation] = handler
													}
													botUser := id.ID{}
													if result.Installation.BotUser != nil {
														botUser = *result.Installation.BotUser
													}
													sealedConfig = newSealedIntakeConfig(gateways, installation.ID, installation.CommunityID, botUser,
														result.Installation.Capabilities, nativeClient, result.Session, handlers, utilities)
												}
											}
											if interactionErr == nil {
												if interactionErr = musicRegistry.RetainInteractions(installation.ID, installation.CommunityID, nativeClient); interactionErr == nil {
													runtimeOwnedNative = true
													sealedConfig.healthDir = filepath.Join(cfg.StateDir, installation.ID.String())
													sealedConfig.musicStartP95 = func() time.Duration { return musicRegistry.StartP95(installation.ID, installation.CommunityID) }
													sealedConfig.musicStarts = func() []music.StartTiming {
														return musicRegistry.StartTimings(installation.ID, installation.CommunityID)
													}
													// v0.1.0-bot2.2 (F4): every intake call uses the helper registered now, not this pass's.
													sealedConfig.resolve = func() sealedHelper {
														current, _ := musicRegistry.Native(installation.ID, installation.CommunityID).(sealedHelper)
														return current
													}
													gateways.SetSealedIntake(sealedConfig)
													// Bot v2 Task 17: incoming webhooks are posted through this installation's send path.
													gateways.SetWebhookPoster(installation.ID, newWebhookPoster(installation.CommunityID,
														service.WelcomeDestinationResolver{Session: result.Session, Native: nativeClient, Feature: "webhooks"},
														coordinatorWebhookSend{coordinator: coordinator, session: result.Session, authority: api}, time.Now,
														logger.With("installation_id", installation.ID.String())))
													gateways.SetTextPoller(installation.ID, helperBoundPoller(musicRegistry, installation.ID, installation.CommunityID, nativeClient,
														newTextPoller(membership, result.Session,
															func(pollCtx context.Context) error {
																return gateways.SubscribeNativeGroups(pollCtx, installation.ID, nativeClient)
															}, eventPoller)))
													gateways.SetMaintenance(installation.ID, backgroundMaintenance{
														current: boundHelperCurrent(musicRegistry, installation.ID, installation.CommunityID, nativeClient),
														usable:  nativeClient.Usable,
														client:  nativeClient,
														run: func(maintenanceCtx context.Context) error {
															var utilityTask func(context.Context) error
															if utilities != nil {
																utilityTask = utilities.maintain
															}
															return runBackgroundMaintenance(maintenanceCtx, scheduler.RunDue, utilityTask)
														},
													})
												}

											}
										}
									}
								}
							}
							if refusal := reconciliationRateLimit(interactionErr); refusal != nil {
								logBotRateLimit(logger, installation.ID, refusal)
								interactionErr = nil
							}
							if interactionErr != nil {
								if !client.IsDeferred(interactionErr) {
									logger.Error("bot interaction event dispatch paused", "installation_id", installation.ID.String(), "error", interactionErr.Error())
								}
								ensureErr = errors.Join(ensureErr, interactionErr)
							}
							if result.Installation.Capabilities&botMusicCapability == 0 {
								if err := musicRegistry.DisableMusic(installation.ID); err != nil {
									logger.Error("bot music runtime cleanup incomplete", "installation_id", installation.ID.String(), "error", err.Error())
								}
							} else {
								musicQueueCount = musicRegistry.QueueCount(installation.ID, installation.CommunityID)
								providers := "youtube"
								logger.Info("bot music queue metadata retained paused", "installation_id", installation.ID.String(), "voice_channels", musicQueueCount, "configured_pcm_providers", providers)
							}
							scheduleErr = errors.Join(scheduleErr, gateways.maintainOne(ctx, installation.ID, maintained))
							reconcileStage = "moderation_evidence"
							caseSink, caseErr := service.NewModerationCaseSink(nativeClient, time.Now)
							if caseErr == nil {
								caseErr = maintainModerationEvidence(ctx, logger, installation.ID, caseSink)
							}
							ensureErr = errors.Join(ensureErr, scheduleErr, caseErr)
						}
					}
				}
			}
		}
		ensureErr = withGatewayStartup(ensureErr, gatewayErr)
		cleanupUnreadyInstallation(installation.ID, membershipReady, installationGone(result.Installation), ensureErr, musicRegistry)
		var closeErr error
		if !retainedNative && !runtimeOwnedNative {
			closeErr = nativeClient.CloseWithReason(helper.ReasonPassUnretained)
		}
		if client.IsDeferred(ensureErr) && closeErr == nil {
			if gateways.TextPoller(installation.ID) == nil {
				gateways.SetTextPoller(installation.ID, oldTextPoller)
			}
			if refusal := reconciliationRateLimit(ensureErr); refusal != nil {
				logBotRateLimit(logger, installation.ID, refusal)
			}
			succeeded++
			delete(unsucceeded, installation.ID)
			continue
		}
		if ensureErr != nil || closeErr != nil {
			failed++
			attrs := []any{"installation_id", installation.ID.String(), "stage", reconcileStage}
			var membershipErr *service.MembershipPhaseError
			if errors.As(ensureErr, &membershipErr) {
				attrs = append(attrs, "membership_phase", membershipErr.Phase.String())
				attrs = append(attrs, safeMembershipErrorAttrs(membershipErr)...)
			}
			if ensureErr != nil {
				attrs = append(attrs, "error_class", helper.Class(ensureErr))
			} else {
				attrs = append(attrs, "error_class", helper.Class(closeErr))
			}
			logger.Error("bot installation reconciliation failed", attrs...)
			continue
		}
		succeeded++
		delete(unsucceeded, installation.ID)
		if !ready {
			incomplete[installation.ID] = 0
		}
		logger.Info("bot installation identity and membership reconciled", "installation_id", installation.ID.String(), "ready", ready)
	}
	var passErr error
	if succeeded != eligible || failed > 0 {
		passErr = fmt.Errorf("%d of %d eligible installations reconciled", succeeded, eligible)
	}
	if maintenanceErr != nil {
		return errors.Join(passErr, maintenanceErr)
	}
	var pending *passIncomplete
	if len(incomplete) > 0 {
		pending = &passIncomplete{after: incomplete}
	}
	if passErr != nil {
		// Each failure is one installation's: the loop retries only those, each on its own backoff (fix round 2, N-3).
		return &passFailed{installations: unsucceeded, incomplete: pending, err: passErr}
	}
	if len(incomplete) > 0 {
		// An installation is joining (its required MLS joins are pending) or an interaction-group registration is in
		// flight; no event of the Bot's own reports either as done. The loop runs that installation again (Bot v2
		// task 14, fix round 1 M-3).
		return &passIncomplete{after: incomplete}
	}
	return nil
}

func safeMembershipErrorAttrs(err error) []any {
	var helperErr *helper.StatusError
	if errors.As(err, &helperErr) {
		name := "other"
		switch helperErr.Status {
		case 1:
			name = "invalid"
		case 2:
			name = "forbidden"
		case 3:
			name = "expired"
		case 4:
			name = "conflict"
		case 5:
			name = "unavailable"
		case 6:
			name = "busy"
		case 7:
			name = "not_found"
		case 8:
			name = "upgrade_required"
		case 9:
			name = "internal"
		}
		return []any{"membership_failure", "native_helper", "helper_status", name}
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		code := "other"
		switch apiErr.Code {
		case "E_BAD_INPUT", "E_CONFLICT", "E_FORBIDDEN", "E_INTERNAL", "E_NOT_FOUND", "E_PRUNED", "E_UNAVAILABLE":
			code = apiErr.Code
		}
		return []any{"membership_failure", "api", "api_status", apiErr.Status, "api_code", code}
	}
	return []any{"membership_failure", "other"}
}

// withGatewayStartup joins a failed gateway start into the pass result. The joined error is never deferred
// (client.IsDeferred is false for a tree with a permanent cause), so the installation counts as failed even when
// every socket route it tried answered ErrNoGateway.
func withGatewayStartup(ensureErr, gatewayErr error) error {
	if gatewayErr == nil {
		return ensureErr
	}
	return errors.Join(ensureErr, gatewayErr)
}
