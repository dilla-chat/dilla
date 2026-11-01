package api

import "math"

// Checked integer conversions keep protocol and persistence values within the
// range of the destination type before narrowing or changing signedness.
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

func checkedUint16(value uint64) (uint16, bool) {
	if value > math.MaxUint16 {
		return 0, false
	}
	return uint16(value), true
}

func checkedInt32(value uint64) (int32, bool) {
	if value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}
