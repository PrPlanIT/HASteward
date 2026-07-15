package common

import "time"

// sleepFn backs Sleep; tests replace it (typically with a no-op) so poll/retry
// loops run without real delays.
var sleepFn = time.Sleep

// Sleep pauses for d. Production code in poll loops should call this instead of
// time.Sleep so flow tests can drive the loops instantly.
func Sleep(d time.Duration) { sleepFn(d) }

// SetSleepForTest replaces Sleep and returns a restore func. TEST-ONLY.
func SetSleepForTest(fn func(time.Duration)) (restore func()) {
	prev := sleepFn
	sleepFn = fn
	return func() { sleepFn = prev }
}

// DisableSleepForTest makes Sleep a no-op (poll loops run instantly) and returns
// a restore func. TEST-ONLY.
func DisableSleepForTest() (restore func()) {
	return SetSleepForTest(func(time.Duration) {})
}
