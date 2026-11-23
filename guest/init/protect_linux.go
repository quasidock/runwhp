//go:build linux

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// protectInit keeps the container from ending its VM behind init's back.
// The container runs in init's PID namespace, as root with every
// capability, so it can reach init and the kernel's reboot call.
//
// CAP_SYS_BOOT leaves init's bounding set, on every thread, so no process
// init starts gets it back when it execs: reboot -f, halt -f and poweroff
// -f fail with EPERM in the container, as in Docker, instead of resetting
// the VM without a status or leaving it halted. Init keeps the capability
// in its own permitted set, for the reset that ends a run.
//
// The signals that tools such as busybox's reboot and halt send PID 1 are
// taken and dropped: unhandled, SIGTERM would end init, and the kernel
// would panic. They are handled rather than ignored, because an ignored
// signal stays ignored across exec, in the container.
func protectInit() {
	const prCapbsetDrop, capSysBoot = 24, 22
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prCapbsetDrop, capSysBoot, 0); errno != 0 {
		logf("dropping CAP_SYS_BOOT from what init starts: %v", errno)
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGPWR)
	go func() {
		for sig := range sigs {
			debugf("ignored %v sent to init", sig)
		}
	}()
}
