//go:build linux

package main

import (
	"syscall"
	"time"
)

// The guest's wall clock follows the host's: the host sends its time in
// Control.Time as init starts listening, every minute or so, and when the
// host wakes from sleep.
const (
	// clockIgnore is an offset too small to act on, about what the message
	// takes to arrive.
	clockIgnore = time.Millisecond
	// clockSlew is the largest offset slewed rather than stepped. The
	// kernel slews at 500 µs a second, so this much is made up in 40 s,
	// within the host's period.
	clockSlew = 20 * time.Millisecond
)

// clockAction decides what to do about the guest's clock being offset
// behind the host's (negative when it is ahead): nothing, slew it by
// offset, or step it by offset.
func clockAction(offset time.Duration) (step bool, ok bool) {
	abs := offset
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs < clockIgnore:
		return false, false
	case abs <= clockSlew:
		return false, true
	default:
		return true, true
	}
}

// adjtimex modes, from linux/timex.h.
const (
	adjSetOffset        = 0x0100
	adjOffsetSingleshot = 0x8001
)

// setClock brings the guest's clock to host, the host's time in nanoseconds
// since the epoch.
func setClock(host int64) {
	offset := time.Duration(host - time.Now().UnixNano())
	step, ok := clockAction(offset)
	if !ok {
		debugf("clock within %v of the host's", offset)
		return
	}
	var tx syscall.Timex
	if step {
		// ADJ_SETOFFSET adds the offset to the clock in one go; the
		// fraction must not be negative. It is in microseconds: ADJ_NANO
		// would set STA_NANO for good, and every later adjtimex would
		// report nanoseconds where programs expect microseconds.
		usec := offset.Microseconds()
		sec, frac := usec/1e6, usec%1e6
		if frac < 0 {
			sec, frac = sec-1, frac+1e6
		}
		tx.Modes = adjSetOffset
		tx.Time = syscall.Timeval{Sec: sec, Usec: frac}
	} else {
		tx.Modes = adjOffsetSingleshot
		tx.Offset = int64(offset / time.Microsecond)
	}
	if _, err := syscall.Adjtimex(&tx); err != nil {
		logf("setting the clock by %v: %v", offset, err)
		return
	}
	if step {
		debugf("stepped the clock by %v", offset)
	} else {
		debugf("slewing the clock by %v", offset)
	}
}
