//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jonasthim/dilla/internal/bot/client"
	"github.com/jonasthim/dilla/internal/bot/helper"
	"github.com/jonasthim/dilla/internal/bot/native"
	"github.com/jonasthim/dilla/internal/bot/service"
	"github.com/jonasthim/dilla/internal/id"
)

var interactionRecoveryCommandRunner = runInteractionRecovery

func runInteractionRecoveryCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dillabot interaction-recovery", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to the bot service TOML config")
	recoverText := flags.String("recover", "", "resolve one group by ID after its exact server receipt is confirmed")
	readmitText := flags.String("readmit", "", "submit an exact staged group again with fresh admission after a lost registration response")
	installationText := flags.String("installation", "", "installation ID that owns the group selected with --recover or --readmit")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "dillabot interaction-recovery: unexpected argument")
		}
		return 2
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, "dillabot interaction-recovery: configuration or protected runtime credential is invalid")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var recoverGroup *id.ID
	var recoverInstallation *id.ID
	if *recoverText != "" && *readmitText != "" {
		fmt.Fprintln(stderr, "dillabot interaction-recovery: --recover and --readmit are mutually exclusive")
		return 2
	}
	selected := *recoverText
	if *readmitText != "" {
		selected = *readmitText
	}
	if selected != "" {
		group, parseErr := id.Parse(selected)
		if parseErr != nil || group.IsZero() {
			fmt.Fprintln(stderr, "dillabot interaction-recovery: group must be a non-zero lowercase 32-character ID")
			return 2
		}
		recoverGroup = &group
		installation, installErr := id.Parse(*installationText)
		if installErr != nil || installation.IsZero() {
			fmt.Fprintln(stderr, "dillabot interaction-recovery: --recover and --readmit require a non-zero lowercase --installation ID")
			return 2
		}
		recoverInstallation = &installation
	} else if *installationText != "" {
		fmt.Fprintln(stderr, "dillabot interaction-recovery: --installation requires --recover or --readmit")
		return 2
	}
	if err := interactionRecoveryCommandRunner(ctx, cfg, stdout, recoverGroup, recoverInstallation, *readmitText != ""); err != nil {
		fmt.Fprintf(stderr, "dillabot interaction-recovery: %v\n", err)
		return 1
	}
	return 0
}

func runInteractionRecovery(ctx context.Context, cfg config, stdout io.Writer, recoverGroup, recoverInstallation *id.ID, readmit bool) error {
	credential, err := readCredential(cfg.RuntimeCredentialFile)
	if err != nil {
		return fmt.Errorf("runtime credential unavailable")
	}
	defer clear(credential)
	api, err := client.New(client.Config{BaseURL: cfg.BaseURL, InstanceID: cfg.InstanceID, AllowLoopbackHTTP: cfg.AllowLoopbackHTTP, Timeout: 30 * time.Second, RateLimits: cfg.RateLimits})
	if err != nil {
		return fmt.Errorf("bot HTTP client configuration is invalid")
	}
	discovery, err := api.Discover(ctx)
	if err != nil || discovery.InstanceID != cfg.InstanceID {
		return fmt.Errorf("configured Dilla instance discovery failed")
	}
	runtime, err := api.Runtime(string(credential))
	if err != nil {
		return fmt.Errorf("runtime credential could not be bound to the client")
	}
	installations, err := runtime.ListInstallations(ctx)
	if err != nil {
		return fmt.Errorf("installation discovery failed")
	}
	var diagnostics []service.InteractionRegistrationRecoveryDiagnostic
	var recovered bool
	for _, installation := range installations {
		if recoverInstallation != nil && installation.ID != *recoverInstallation {
			continue
		}
		if installation.State >= 4 {
			continue
		}
		key, exists, err := openExistingInteractionStateKey(cfg.StateDir, installation.ID.String())
		if err != nil {
			return fmt.Errorf("installation %s native state key unavailable", installation.ID)
		}
		if !exists {
			continue
		}
		nativeClient, err := native.Start(ctx, helper.Options{Executable: cfg.HelperExecutable, StateDir: filepath.Join(cfg.StateDir, installation.ID.String())}, native.Binding{Instance: cfg.InstanceID, Community: installation.CommunityID, Installation: installation.ID}, key)
		clear(key)
		if err != nil {
			return fmt.Errorf("installation %s native helper unavailable; stop the dillabot service first if it holds the helper lock", installation.ID)
		}
		identity, identityErr := nativeClient.Identity(ctx)
		stored, sessionErr := nativeClient.Session(ctx)
		if identityErr != nil || identity.Phase != 2 || sessionErr != nil || stored.Token() == "" {
			_ = nativeClient.Close()
			continue
		}
		if identity.Device == nil {
			_ = nativeClient.Close()
			continue
		}
		session, err := api.SessionForDevice(stored.Token(), *identity.Device)
		if err != nil {
			_ = nativeClient.Close()
			return fmt.Errorf("installation %s device session is invalid", installation.ID)
		}
		coordinator, err := service.NewInteractionRegistrationCoordinator(cfg.InstanceID, installation.CommunityID, installation.ID, nativeClient, api, session)
		if err != nil {
			_ = nativeClient.Close()
			return fmt.Errorf("installation %s recovery coordinator unavailable", installation.ID)
		}
		rows, inspectErr := coordinator.InspectInteractionRegistrationRecovery(ctx)
		if inspectErr == nil && recoverGroup != nil {
			for _, row := range rows {
				if row.Group != *recoverGroup {
					continue
				}
				var result service.InteractionRegistrationResult
				var recoveryErr error
				if readmit {
					result, recoveryErr = coordinator.ReadmitInteractionRegistration(ctx, *recoverGroup)
				} else {
					result, recoveryErr = coordinator.RecoverInteractionRegistration(ctx, *recoverGroup)
				}
				if recoveryErr != nil {
					_ = nativeClient.Close()
					return fmt.Errorf("group %s recovery refused: %w", *recoverGroup, recoveryErr)
				}
				if _, writeErr := fmt.Fprintf(stdout, "resolved installation=%s operation=%s group=%s accepted_generation=%d phase=2 usable=false\n", installation.ID, result.Receipt.OperationID, result.Receipt.GroupID, result.Receipt.AcceptedGeneration); writeErr != nil {
					_ = nativeClient.Close()
					return writeErr
				}
				recovered = true
			}
		} else if inspectErr == nil && recoverGroup == nil {
			diagnostics = append(diagnostics, rows...)
		}
		closeErr := nativeClient.Close()
		if inspectErr != nil {
			return fmt.Errorf("installation %s recovery inspection failed: %w", installation.ID, inspectErr)
		}
		if closeErr != nil {
			return fmt.Errorf("installation %s native helper close failed", installation.ID)
		}
		if recoverGroup != nil && recovered {
			break
		}
	}
	if recoverGroup != nil {
		if !recovered {
			return fmt.Errorf("group %s has no manual-recovery interaction registration", *recoverGroup)
		}
		return nil
	}
	return formatInteractionRecoveryDiagnostics(stdout, diagnostics)
}

// openExistingInteractionStateKey only opens already initialized state. The
// diagnostic command must never create directories, database keys, or state.
func openExistingInteractionStateKey(root, installation string) ([]byte, bool, error) {
	if !filepath.IsAbs(root) || len(installation) != 32 {
		return nil, false, os.ErrInvalid
	}
	installDir := filepath.Join(root, installation)
	info, err := os.Lstat(installDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int64(st.Uid) != int64(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return nil, false, os.ErrPermission
	}
	stateInfo, err := os.Lstat(filepath.Join(installDir, "state.db"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !stateInfo.Mode().IsRegular() || stateInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, os.ErrInvalid
	}
	path := filepath.Join(installDir, "database.key")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, false, os.ErrPermission
	}
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), "database-key")
	defer func() { _ = f.Close() }()
	keyInfo, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	keyStat, ok := keyInfo.Sys().(*syscall.Stat_t)
	if !keyInfo.Mode().IsRegular() || !ok || int64(keyStat.Uid) != int64(os.Geteuid()) || keyInfo.Mode().Perm() != 0o600 {
		return nil, false, os.ErrPermission
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(key) != 32 {
		clear(key)
		return nil, false, os.ErrInvalid
	}
	return key, true, nil
}

func formatInteractionRecoveryDiagnostics(stdout io.Writer, diagnostics []service.InteractionRegistrationRecoveryDiagnostic) error {
	if len(diagnostics) == 0 {
		_, err := fmt.Fprintln(stdout, "No manual-recovery interaction registrations.")
		return err
	}
	for _, row := range diagnostics {
		if _, err := fmt.Fprintf(stdout, "installation=%s operation=%s group=%s generation=%d phase=%d registration_sha256=%x receipt=%s group_status=%s\n", row.Installation, row.Operation, row.Group, row.Generation, row.Phase, row.RegistrationSHA256, row.ReceiptStatus, row.GroupStatus); err != nil {
			return err
		}
	}
	return nil
}
