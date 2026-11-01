package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/jonasthim/dilla/internal/desktoplaunch"
)

type launchProcess func(*selection, []string, bool) (uint32, error)
type pendingUpdate struct {
	nonce   string
	journal *desktoplaunch.Journal
	old     *selection
}

func activeNonce(l layout) (string, error) {
	raw, e := readMetadata(filepath.Join(l.updates, "active.json"), 256)
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	return desktoplaunch.ParseActiveRecord(raw)
}
func readJournal(l layout, nonce string) (*desktoplaunch.Journal, error) {
	raw, e := readMetadata(filepath.Join(l.updates, "startup-"+nonce, "journal.json"), 1024)
	if os.IsNotExist(e) {
		return &desktoplaunch.Journal{Format: 1, Nonce: nonce, Phase: "prepared"}, nil
	}
	if e != nil {
		return nil, e
	}
	return desktoplaunch.ParseJournal(raw, nonce)
}
func loadPending(l layout, cfg buildConfig, now time.Time) (*pendingUpdate, error) {
	nonce, e := activeNonce(l)
	if e != nil || nonce == "" {
		return nil, e
	}
	journal, e := readJournal(l, nonce)
	if e != nil {
		return nil, e
	}
	raw, e := readMetadata(filepath.Join(l.updates, "startup-"+nonce, "old-slot.json"), 1_048_576)
	if e != nil {
		return nil, e
	}
	old, e := selectSlot(raw, l, cfg, now)
	if e != nil {
		return nil, e
	}
	return &pendingUpdate{nonce: nonce, journal: journal, old: old}, nil
}

func describe(cfg buildConfig, out io.Writer) error {
	keys, e := json.Marshal(cfg.PublicKeys)
	if e != nil {
		return e
	}
	hash := sha256.Sum256(keys)
	return json.NewEncoder(out).Encode(map[string]any{"format": 1, "appId": cfg.AppID, "productName": cfg.ProductName, "channel": cfg.Channel, "arch": cfg.Arch, "initialVersion": cfg.InitialVersion, "initialManifestSha512": cfg.InitialManifestSHA512, "trustHash": hex.EncodeToString(hash[:])})
}
func reportState(out io.Writer, s *selection, status string) error {
	return json.NewEncoder(out).Encode(struct {
		*desktoplaunch.Slot
		Status string `json:"status"`
	}{s.slot, status})
}

func runLauncher(l layout, cfg buildConfig, args arguments, out io.Writer, launch launchProcess) (int, error) {
	if args.mode == "describe" {
		return 0, describe(cfg, out)
	}
	for _, root := range []string{l.stableRoot, l.userData, l.updates} {
		guard, e := desktoplaunch.PinOwnedDirectory(root)
		if e != nil {
			return 1, e
		}
		defer func() { _ = guard.Close() }()
	}
	now := time.Now()
	pending, e := loadPending(l, cfg, now)
	if e != nil {
		return 1, e
	}
	if pending != nil {
		defer func() { _ = pending.old.Close() }()
	}
	var selected *selection
	defer func() {
		if selected != nil {
			_ = selected.Close()
		}
	}()
	if args.mode == "state" {
		status := "committed"
		if pending != nil {
			status = "pending"
			selected = pending.old
			if pending.journal.Phase == "complete" {
				selected, e = selectCommitted(l, cfg, now)
				if e != nil {
					return 1, e
				}
				if selected.slot.Version != pending.journal.Version {
					return 1, errors.New("complete journal does not match committed slot")
				}
			}
			if pending.journal.Phase == "failed" {
				current, err := selectCommitted(l, cfg, now)
				if err != nil {
					return 1, err
				}
				defer func() { _ = current.Close() }()
				if !reflect.DeepEqual(current.slot, pending.old.slot) {
					return 1, errors.New("failed journal does not match previous committed slot")
				}
			}
		} else {
			selected, e = selectCommitted(l, cfg, now)
			if e != nil {
				return 1, e
			}
		}
		return 0, reportState(out, selected, status)
	}
	if args.mode != "launch" {
		return 1, errors.New("invalid launcher mode")
	}
	if pending != nil {
		code, err := launch(pending.old, []string{"--update-bootstrap=" + pending.nonce}, true)
		if err != nil {
			return 1, err
		}
		if code == 20 || code == 21 {
			return 0, nil
		}
		if code != 22 {
			return 1, errors.New("verified bootstrap did not clear pending startup")
		}
		nonce, err := activeNonce(l)
		if err != nil {
			return 1, err
		}
		if nonce != pending.nonce {
			return 1, errors.New("update changed while bootstrap was running")
		}
		journal, err := readJournal(l, nonce)
		if err != nil {
			return 1, err
		}
		if journal.Phase != "complete" && journal.Phase != "failed" {
			return 1, errors.New("update remains unresolved after bootstrap")
		}
		selected, e = selectCommitted(l, cfg, time.Now())
		if e != nil {
			return 1, e
		}
		if journal.Phase == "complete" && selected.slot.Version != journal.Version {
			return 1, errors.New("committed release does not match completion")
		}
		if journal.Phase == "failed" && !reflect.DeepEqual(selected.slot, pending.old.slot) {
			return 1, errors.New("failed update did not retain previous committed version")
		}
	} else {
		selected, e = selectCommitted(l, cfg, now)
		if e != nil {
			return 1, e
		}
	}
	// Source main repeats this check before ordinary Electron boot as well; the
	// launcher never forwards user intent after observing a different transaction.
	expectedNonce := ""
	if pending != nil {
		expectedNonce = pending.nonce
	}
	latestNonce, e := activeNonce(l)
	if e != nil {
		return 1, e
	}
	if latestNonce != expectedNonce {
		return 1, errors.New("active update changed before ordinary launch")
	}
	_, e = launch(selected, args.forwarded, false)
	if e != nil {
		return 1, e
	}
	return 0, nil
}
