//go:build !linux

package botsupervisor

func openPidfd(int) (childHandle, error) { return nil, errNotLinux }
