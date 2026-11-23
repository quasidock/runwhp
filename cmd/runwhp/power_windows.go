//go:build windows

package main

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Resume notifications, for the guest clock: a guest whose host slept may
// wake with its clock behind.
//
// PowerRegisterSuspendResumeNotification with a callback works in a process
// with no window and no console, as a container's background runwhp is. The
// callback runs on a thread of the system's; registering once per process
// keeps to the one callback windows.NewCallback has to make.

var (
	powrprof                                   = windows.NewLazySystemDLL("powrprof.dll")
	procPowerRegisterSuspendResumeNotification = powrprof.NewProc("PowerRegisterSuspendResumeNotification")
)

const (
	deviceNotifyCallback  = 2
	pbtAPMResumeSuspend   = 0x7
	pbtAPMResumeAutomatic = 0x12
)

// deviceNotifySubscribeParameters is DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS.
type deviceNotifySubscribeParameters struct {
	callback uintptr
	context  uintptr
}

var resume struct {
	once   sync.Once
	err    error
	params deviceNotifySubscribeParameters // kept alive while registered
	handle uintptr

	mu   sync.Mutex
	next int
	fns  map[int]func()
}

// onResume calls f, on a thread of its own, each time Windows resumes from
// sleep, until stop is called.
func onResume(f func()) (stop func(), err error) {
	resume.once.Do(func() {
		if err := procPowerRegisterSuspendResumeNotification.Find(); err != nil {
			resume.err = err
			return
		}
		resume.fns = map[int]func(){}
		resume.params.callback = windows.NewCallback(func(ctx, typ, setting uintptr) uintptr {
			if typ == pbtAPMResumeAutomatic || typ == pbtAPMResumeSuspend {
				resume.mu.Lock()
				fns := make([]func(), 0, len(resume.fns))
				for _, f := range resume.fns {
					fns = append(fns, f)
				}
				resume.mu.Unlock()
				for _, f := range fns {
					go f()
				}
			}
			return 0
		})
		r, _, _ := procPowerRegisterSuspendResumeNotification.Call(deviceNotifyCallback,
			uintptr(unsafe.Pointer(&resume.params)), uintptr(unsafe.Pointer(&resume.handle)))
		if r != 0 {
			resume.err = fmt.Errorf("registering for resume notifications: %w", windows.Errno(r))
		}
	})
	if resume.err != nil {
		return func() {}, resume.err
	}
	resume.mu.Lock()
	id := resume.next
	resume.next++
	resume.fns[id] = f
	resume.mu.Unlock()
	return func() {
		resume.mu.Lock()
		delete(resume.fns, id)
		resume.mu.Unlock()
	}, nil
}
