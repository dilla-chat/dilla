//go:build linux

package main

import "errors"

var errNativeLeaseUnavailable = errors.New("bot command native generation is unavailable")

type nativeLeaseUnavailableError struct{}

func (*nativeLeaseUnavailableError) Error() string        { return errNativeLeaseUnavailable.Error() }
func (*nativeLeaseUnavailableError) Is(target error) bool { return target == errNativeLeaseUnavailable }
