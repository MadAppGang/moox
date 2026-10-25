package saver

import "time"

// Clock is the saver's time source: a consumer-side port, faked in tests
// (§5.2). Every timer it hands out is a one-shot (tt §9.3a).
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now is time.Now.
func (SystemClock) Now() time.Time { return time.Now() }

// NewTimer is time.NewTimer: a one-shot, armed by the event that asks for
// it, which is exactly what the timer rule permits (audit.go:650-653).
func (SystemClock) NewTimer(d time.Duration) Timer { return sysTimer{time.NewTimer(d)} }

type sysTimer struct{ t *time.Timer }

func (s sysTimer) C() <-chan time.Time { return s.t.C }
func (s sysTimer) Stop() bool          { return s.t.Stop() }
