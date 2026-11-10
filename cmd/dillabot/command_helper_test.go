//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dilla-chat/dilla/internal/bot/protocol"
)

// This subprocess speaks the helper protocol over fd3. The parent uses the REAL
// helper.Client and linuxExchange: cancellation really closes its socket and
// kills the process. Blocking occurs inside a protocol call, never fake HTTP.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--state-dir" {
		if mode, err := os.ReadFile(filepath.Join(os.Args[2], "worker-helper-fixture")); err == nil {
			if err := serveWorkerHelper(os.Args[2], string(mode)); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}
func serveWorkerHelper(dir, mode string) error {
	conn := os.NewFile(3, "worker-helper")
	defer conn.Close()
	blocked := false
	executions := 0
	result := []byte{0x80}
	digest := sha256.Sum256(result)
	transfer := make([]byte, 16)
	transfer[15] = 1
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(conn, prefix[:]); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(prefix[:])
		if size > 1<<20 {
			return errors.New("invalid helper frame")
		}
		frame := make([]byte, 4+int(size))
		copy(frame, prefix[:])
		if _, err := io.ReadFull(conn, frame[4:]); err != nil {
			return err
		}
		req, err := protocol.DecodeHelperRequestFrame(frame)
		if err != nil {
			return err
		}
		payload := []any{}
		shouldBlock := req.Operation == protocol.HelperStateGet && mode == "read"
		if req.Operation == protocol.HelperGroupTransfer {
			stage := req.Payload[0].(uint64)
			switch stage {
			case 0:
				payload = []any{transfer}
			case 1:
				payload = []any{req.Payload[2].(uint64) + uint64(len(req.Payload[3].([]byte)))}
			case 2:
				shouldBlock = mode == "apply"
				payload = []any{uint64(0), uint64(len(result)), digest[:]}
			case 3:
				payload = []any{result}
			}
		}
		if shouldBlock && !blocked {
			blocked = true
			if err = os.WriteFile(filepath.Join(dir, "entered"), []byte("1"), 0600); err != nil {
				return err
			}
			for {
				if _, err = os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		if req.Operation == protocol.HelperGroupTransfer && req.Payload[0] == uint64(2) {
			executions++
			if err = os.WriteFile(filepath.Join(dir, "executions"), []byte(strconv.Itoa(executions)), 0600); err != nil {
				return err
			}
		}
		response, err := protocol.EncodeHelperResponse(protocol.HelperResponse{RequestID: req.RequestID, Payload: payload})
		if err != nil {
			return err
		}
		for len(response) > 0 {
			n, e := conn.Write(response)
			if e != nil {
				return e
			}
			response = response[n:]
		}
		if req.Operation == protocol.HelperShutdown {
			return nil
		}
	}
}
