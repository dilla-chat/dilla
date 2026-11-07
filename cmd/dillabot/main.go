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
	if len(os.Args) > 1 && os.Args[1] == "interaction-recovery" {
		os.Exit(runInteractionRecoveryCommand(os.Args[2:], os.Stdout, os.Stderr))
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
	autoModerationState := newAutoModerationEventState()
	gateways := newBotGatewayRuntime(ctx, api, cfg.AllowLoopbackHTTP, logger)
	if cfg.Socket != "" {
		gateways.httpClient, gateways.launch = cfg.httpClient(0), cfg.launch
	}
	gateways.StartCommandWorkers()
	defer gateways.Close()
	statuses := newRuntimeStatusTracker(Version)
	// Bot v2 task 14: no reconcile interval. A pass runs at start and then on an event (event_loop.go).
	loop := botLoop{
		clock: clock.System(), log: logger,
		pass: func(passCtx context.Context) error {
			return reconcile(passCtx, cfg, api, runtime, wasm, musicRegistry, autoModerationState, gateways, statuses, logger)
		},
		passes: gateways.Pass(), reload: reload,
		wake: gateways.Wake(), poll: gateways.PollText,
		maintain: func(maintainCtx context.Context) error {
			return gateways.maintain(client.WithBackground(maintainCtx), map[id.ID]bool{})
		},
		due: dueInterval,
	}
	return loop.run(ctx)
}

func reconcile(ctx context.Context, cfg config, api *client.Client, runtime *client.Runtime, wasm *mlswasi.Runtime, musicRegistry *musicQueueRegistry, autoModerationState *autoModerationEventState, gateways *botGatewayRuntime, statuses *runtimeStatusTracker, logger *slog.Logger) error {
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
	for _, installation := range installations {
		if installation.State < 4 {
			activeInstallations = append(activeInstallations, installation.ID)
		}
	}
	// Retire command contexts before helper/voice teardown can block.
	gateways.RetainOnly(activeInstallations)
	if err := musicRegistry.RetainOnly(activeInstallations); err != nil {
		logger.Error("stale bot music runtime cleanup incomplete", "error", err.Error())
	}
	autoModerationState.RetainOnly(activeInstallations)
	statuses.RetainOnly(activeInstallations)
	maintained := make(map[id.ID]bool)
	maintenanceErr := gateways.maintain(ctx, maintained)
	var eligible, succeeded, failed int
	incomplete := false
	for _, installation := range installations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if installation.State >= 4 {
			_ = musicRegistry.Disable(installation.ID)
			autoModerationState.Disable(installation.ID)
			continue
		}
		eligible++
		// Task 10c: a manager's Restart. Tear down everything this process holds for the
		// installation (native helper, music queues and voice rooms, automatic moderation state,
		// gateway connection); the rest of this pass starts them again from durable state.
		if statuses.Observe(installation) {
			logger.Info("bot installation restarting on a manager's request", "installation_id", installation.ID.String())
			gateways.Drop(installation.ID)
			autoModerationState.Disable(installation.ID)
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
				continue
			}
			logger.Warn("bot runtime status report deferred", "installation_id", installation.ID.String())
		}
		oldTextPoller := gateways.TextPoller(installation.ID)
		oldInteractionPoller := gateways.InteractionPoller(installation.ID)
		// Keep the current callbacks live during slow dependency rebuilding.
		// Successful publication swaps in the callbacks for the rebuilt runtime.
		nativeClient, retainedNative := musicRegistry.Native(installation.ID, installation.CommunityID).(*native.Client)
		if !retainedNative {
			key, _, keyErr := loadDatabaseKey(cfg.StateDir, installation.ID.String())
			if keyErr != nil {
				_ = musicRegistry.Disable(installation.ID)
				logger.Error("bot installation unavailable", "installation_id", installation.ID.String(), "stage", "state_key")
				continue
			}
			nativeClient, err = native.Start(ctx, helper.Options{Executable: cfg.HelperExecutable, StateDir: filepath.Join(cfg.StateDir, installation.ID.String())}, native.Binding{Instance: cfg.InstanceID, Community: installation.CommunityID, Installation: installation.ID}, key)
			clear(key)
			if err != nil {
				_ = musicRegistry.Disable(installation.ID)
				logger.Error("bot installation unavailable", "installation_id", installation.ID.String(), "stage", "helper_start")
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
				_ = nativeClient.Close()
			}
			_ = musicRegistry.Disable(installation.ID)
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
			}
			reconcileStage = "membership_source_sync"
			var membership *service.MembershipReconciler
			membership, ensureErr = service.NewMembershipReconciler(nativeClient, service.SourceSyncOptions{}, 32)
			if ensureErr == nil {
				membership.UseInteractionPollSchedule(gateways.InteractionPollSchedule(installation.ID))
			}
			if ensureErr == nil {
				var trust *service.SourceTrust
				trust, ensureErr = service.NewSourceTrust(nativeClient)
				if ensureErr == nil {
					var healed int
					// Senders whose signed device-list history is stale: held in this pass's drain.
					ready, healed, reconcileStage, _, ensureErr = reconcileMembershipReadiness(ctx, result.Installation, nativeClient, result.Session, commandOwnedMembership{membership, nativeClient, gateways, installation.ID}, trust, unrefreshedSendersLogger(logger, installation.ID))
					if ensureErr == nil && healed > 0 {
						logger.Info("bot interaction groups healed after restore", "installation_id", installation.ID.String(), "groups", healed)
					}
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
							// this pass's helper and session here, and the feed applies what it buffered. Interaction
							// registration still stages from the current member list too.
							registration, registrationErr := service.NewInteractionRegistrationCoordinator(cfg.InstanceID, installation.CommunityID, installation.ID, nativeClient, api, result.Session)
							if registrationErr != nil {
								ensureErr = registrationErr
							} else if catchup, catchupErr := service.NewMemberRegistrationCatchup(nativeClient, service.MemberRegistrationCatchupOptions{}); catchupErr != nil {
								ensureErr = catchupErr
							} else {
								members, membersErr := result.Session.CommunityMembers(installation.CommunityID)
								if membersErr != nil {
									logger.Info("bot interaction registration worker unavailable", "installation_id", installation.ID.String(), "stage", "member_lookup")
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
										ingest: nativeClient.IngestDeviceList, episodes: members, stage: registration.QueueMemberJoin, welcome: welcomeHandler,
									})
									catchupResult, stageErr := catchup.StageOnce(ctx, members, registration)
									if stageErr != nil {
										logger.Info("bot member registration catch-up remains pending", "installation_id", installation.ID.String(), "error", stageErr.Error())
									} else if catchupResult.More {
										logger.Info("bot member registration catch-up remains bounded", "installation_id", installation.ID.String(), "members_visited", catchupResult.MembersVisited, "pages", catchupResult.Pages)
									}
									registration.WithLogger(logger).WithBackoff(registrationBackoff)
									workErr := registration.ProcessPending(ctx, 4, 8, members, result.Session)
									if registrationWorkReports.changed(installation.ID, workErr) {
										logger.Info("bot interaction registration work remains pending", "installation_id", installation.ID.String(), "error", workErr.Error())
									}
									// An interaction group registered just now is not among the groups subscribed above: subscribe it now, or the
									// member's first command never reaches the Bot as a hint (Bot v2 task 14: no periodic pass re-subscribes).
									if err := gateways.SubscribeNativeGroups(ctx, installation.ID, nativeClient); err != nil {
										logger.Warn("bot gateway native group discovery unavailable", "installation_id", installation.ID.String())
									}
									// While a registration is in flight (a member's device not registered yet, e.g. waiting for its
									// KeyPackages, or a registered group not active yet) the pass is incomplete: the loop runs it
									// again on its backoff, which also subscribes the group once it is active. Nothing the Bot
									// hears says when either happens.
									pendingWork, pendingErr := registration.HasPendingWork(ctx)
									if workErr != nil || pendingErr != nil || pendingWork || interactionGroupsPending(ctx, nativeClient) {
										incomplete = true
									}
								}
							}
							var musicDeferred error
							var musicInteractions *service.MusicInteractions
							var musicProviderStatus service.MusicProviderStatusInteractions
							if result.Installation.Capabilities&botMusicCapability != 0 {
								musicProviderStatus = newMusicProviderStatusInteractions(installation.ID, installation.CommunityID, result.Session)
								channels, channelsErr := result.Session.Channels(ctx, installation.CommunityID)
								if channelsErr == nil {
									starterFor := func(channel id.ID) (session.QueueStarter, error) {
										joinCallGroup := func(joinCtx context.Context) error {
											communityRow, communityErr := result.Session.Community(joinCtx, installation.CommunityID)
											if communityErr != nil {
												return communityErr
											}
											return ensureMusicCallGroupJoined(joinCtx, result.Session, nativeClient, installation.ID, installation.CommunityID, channel, communityRow.PolicyVersion)
										}
										starter, starterErr := newInstallationMusicQueueStarter(cfg.InstanceID, installation.ID, installation.CommunityID, channel, result.Session, nativeClient, nil, joinCallGroup)
										if starterErr == nil {
											starter.IdleLeaveAfter = func() time.Duration { return musicRegistry.IdleLeave(installation.ID) }
											device := id.ID{}
											if result.Installation.BotDevice != nil {
												device = *result.Installation.BotDevice
											}
											starter.Logger = logger.With("installation_id", installation.ID.String(), "device_id", device.String(), "channel_id", channel.String())
											starter.LogContext = func(logCtx context.Context) context.Context {
												group := id.ID{}
												if state, lookupErr := result.Session.ChannelCall(logCtx, channel); lookupErr == nil && state.GroupID != nil {
													group = *state.GroupID
												}
												return music.WithPlaybackGroup(logCtx, group)
											}
											notices := musicRegistry.SkipNotices(installation.ID)
											starter.OnSkipped = func(skipCtx context.Context, skips []music.SkippedTrack) { notices.Notify(skipCtx, channel, skips) }
											starter.OnCallEnded = func() { notices.Clear(channel) }
											if starter.SetCallEnded != nil {
												starter.SetCallEnded(starter.OnCallEnded)
											}
										}
										return starter, starterErr
									}
									runtimeOwnedNative, channelsErr = musicRegistry.ReconcileWithStarterFactory(ctx, installation.ID, installation.CommunityID, result.Installation.Capabilities, channels, nativeClient, starterFor)
								}
								if channelsErr != nil {
									musicDeferred = cleanupFailedMusicRestore(installation.ID, channelsErr, musicRegistry)
									logger.Error("bot music queue restore paused", "installation_id", installation.ID.String(), "error", channelsErr.Error())
									ensureErr = errors.Join(ensureErr, channelsErr)
								} else {
									var musicErr error
									musicInteractions, musicErr = newRuntimeMusicInteractions(installation.ID, installation.CommunityID, result.Installation.Capabilities, runtime, result.Session, musicRegistry, logger)
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
							interactionPoller, interactionErr := service.NewGroupEventPoller(nativeClient)
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
									var outbox *service.InteractionResponseOutbox
									outbox, interactionErr = service.NewInteractionResponseOutbox(nativeClient, coordinator, result.Session)
									if interactionErr == nil {
										var dispatcher *service.InteractionDispatcher
										var snapshots *service.InteractionRegistrationCoordinator
										snapshots, interactionErr = service.NewInteractionRegistrationCoordinator(cfg.InstanceID, installation.CommunityID, installation.ID, nativeClient, api, result.Session)
										if interactionErr == nil {
											_, replayThrough, replayReceivedThrough, caseSink, moderationErr := configureAutoModeration(ctx, autoModerationState, nativeClient, result.Session, installation.ID, installation.CommunityID, logger)
											if moderationErr != nil {
												logger.Warn("bot automatic moderation configuration unavailable; retaining the last active policy", "installation_id", installation.ID.String(), "error", moderationErr.Error())
											}
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
														autoModerationGrantRedeemer{Session: result.Session, InstallationID: installation.ID},
														autoModerationState.ConfigRuntime(nativeClient, installation.ID, installation.CommunityID, func(cfg moderation.Config) (service.ModerationActorAuthority, service.ModerationActionSink, error) {
															return prepareAutoModerationDependencies(nativeClient, result.Session, installation.CommunityID, cfg)
														}),
														musicProviderStatus,
													)
													for operation, handler := range runtimeRolePanelHandlers(result.Installation.Capabilities, service.RolePanelInteractions{
														Authority:   result.Session,
														Destination: service.WelcomeDestinationResolver{Session: result.Session, Native: nativeClient},
														Sender:      coordinator, Discovery: api, Session: result.Session, CommunityID: installation.CommunityID, Now: time.Now,
													}) {
														handlers[operation] = handler
													}
													// Jonas 2026-10-28: /play that starts music is announced publicly with the live player card.
													musicAnnouncements := service.MusicAnnouncements{
														Music: musicInteractions, Authority: result.Session, Channels: result.Session, Limiter: &service.MusicAnnounceLimiter{},
														Destination: service.WelcomeDestinationResolver{Session: result.Session, Native: nativeClient},
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
													dispatcher, interactionErr = newRuntimeUtilityDispatcher(nativeClient, handlers, utilities, result.Session,
														installation.CommunityID, installation.ID, botUser, result.Installation.Capabilities,
														func() uint64 { return unixSecondsOrZero(time.Now()) }, snapshots)
												}
											}
											if interactionErr == nil {
												eventHandler := newInteractionEventHandler(nativeClient, dispatcher, durableInteractionResponseSender{api: api, outbox: outbox})
												eventHandler = newGroupEventRouter(nativeClient, eventHandler, autoModerationState.EventHandler(installation.ID, installation.CommunityID), replayThrough, replayReceivedThrough, newMusicReplyRecovery(installation.CommunityID, nativeClient, result.Session, api, coordinator))
												eventRuntime := newInteractionEventRuntime(interactionPoller, eventHandler)
												if interactionErr = musicRegistry.RetainInteractions(installation.ID, installation.CommunityID, nativeClient); interactionErr == nil {
													runtimeOwnedNative = true
													reportUnrefreshed := unrefreshedSendersLogger(logger, installation.ID)
													intakeEvents := countSkipped{holdingEventDrainer: eventRuntime, reportSkipped: func(events []service.SkippedGroupEvent) {
														logSkippedCommandEvents(logger, installation.ID, events)
													}}
													gateways.SetTextPoller(installation.ID, newCommandIntakePoller(commandOwnedMembership{membership, nativeClient, gateways, installation.ID}, trust, result.Session,
														func(pollCtx context.Context) error {
															return gateways.SubscribeNativeGroups(pollCtx, installation.ID, nativeClient)
														}, queuedInteractionDrainer{gateways, installation.ID}, reportUnrefreshed))
													gateways.SetInteractionPoller(installation.ID, retainedInteractionPoller(musicRegistry, installation.ID, installation.CommunityID, nativeClient,
														newInteractionIntakePoller(membership, trust, result.Session, intakeEvents, reportUnrefreshed)))
													gateways.SetMaintenance(installation.ID, backgroundMaintenance{
														current: func() bool { return musicRegistry.Native(installation.ID, installation.CommunityID) == nativeClient },
														run: func(maintenanceCtx context.Context) error {
															var utilityTask func(context.Context) error
															if utilities != nil {
																utilityTask = utilities.maintain
															}
															return runBackgroundMaintenance(maintenanceCtx, scheduler.RunDue, utilityTask)
														},
													})
												}

												if interactionErr == nil {
													gateways.notifyBackgroundInteraction(installation.ID, id.ID{})
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
							caseSink, caseErr := service.NewModerationCaseSink(nativeClient, nil, time.Now)
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
		cleanupUnreadyInstallation(installation.ID, membershipReady, ensureErr, musicRegistry, autoModerationState)
		var closeErr error
		if !retainedNative && !runtimeOwnedNative {
			closeErr = nativeClient.Close()
		}
		if client.IsDeferred(ensureErr) && closeErr == nil {
			if gateways.TextPoller(installation.ID) == nil {
				gateways.SetTextPoller(installation.ID, oldTextPoller)
			}
			if gateways.InteractionPoller(installation.ID) == nil {
				gateways.SetInteractionPoller(installation.ID, oldInteractionPoller)
			}
			if refusal := reconciliationRateLimit(ensureErr); refusal != nil {
				logBotRateLimit(logger, installation.ID, refusal)
			}
			succeeded++
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
			var trustErr *service.TrustPhaseError
			if errors.As(ensureErr, &trustErr) {
				attrs = append(attrs, "trust_phase", trustErr.Phase.String())
				attrs = append(attrs, safeTrustErrorAttrs(trustErr)...)
			}
			logger.Error("bot installation reconciliation failed", attrs...)
			continue
		}
		succeeded++
		if !ready {
			incomplete = true
		}
		logger.Info("bot installation identity and membership reconciled", "installation_id", installation.ID.String(), "ready", ready)
	}
	var passErr error
	if succeeded != eligible || failed > 0 {
		passErr = fmt.Errorf("%d of %d eligible installations reconciled", succeeded, eligible)
	}
	if err := errors.Join(passErr, maintenanceErr); err != nil {
		return err
	}
	if incomplete {
		// An installation is joining (its required MLS joins are pending) or an interaction-group registration is in
		// flight; no event of the Bot's own reports either as done. The loop runs the pass again on a backoff until
		// both are settled (Bot v2 task 14).
		return errPassIncomplete
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

func safeTrustErrorAttrs(err error) []any {
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
		return []any{"trust_failure", "native_helper", "helper_status", name}
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		code := "other"
		switch apiErr.Code {
		case "E_BAD_INPUT", "E_CONFLICT", "E_FORBIDDEN", "E_INTERNAL", "E_NOT_FOUND", "E_PRUNED", "E_UNAVAILABLE":
			code = apiErr.Code
		}
		return []any{"trust_failure", "api", "api_status", apiErr.Status, "api_code", code}
	}
	return []any{"trust_failure", "other"}
}

// unrefreshedSendersLogger reports senders whose signed device-list history was stale in a
// command-intake trust pass. Their commands stay held (not dispatched) until a later pass makes
// their trust current; only the count is logged, never the user identities.
func unrefreshedSendersLogger(logger *slog.Logger, installation id.ID) func(service.TrustRefresh) {
	return func(refresh service.TrustRefresh) {
		logger.Warn("bot commands from senders with stale device-list history held",
			"installation_id", installation.String(), "trust_failure", "sender_history_stale", "senders", len(refresh.Unrefreshed))
	}
}

// withGatewayStartup joins a failed gateway start into the pass result. The joined error is never deferred
// (client.IsDeferred is false for a tree with a permanent cause), so the installation counts as failed even when
// every socket route it tried answered ErrNoGateway.
// interactionGroupsPending reports a private interaction group the helper holds that is not active yet (below state 2,
// the active one; 3 is gone): its participant's commands reach the Bot as hints only once it is subscribed as active. A read failure
// counts as pending, so the pass is run again rather than left waiting.
func interactionGroupsPending(ctx context.Context, n notificationGroupSource) bool {
	groups, err := n.InteractionGroups(ctx)
	if err != nil {
		return true
	}
	for _, g := range groups {
		if g.State < 2 {
			return true
		}
	}
	return false
}

func withGatewayStartup(ensureErr, gatewayErr error) error {
	if gatewayErr == nil {
		return ensureErr
	}
	return errors.Join(ensureErr, gatewayErr)
}
