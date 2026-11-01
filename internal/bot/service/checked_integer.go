package service

import "math"

// Checked integer conversions keep durable state values within the range of the destination
// type before changing signedness (the same helpers as internal/api).
func checkedUint64(value int64) (uint64, bool) {
	if value < 0 {
		return 0, false
	}
	return uint64(value), true
}

func checkedInt64(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}
