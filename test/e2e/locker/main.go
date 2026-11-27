//go:build linux

// Command locker takes POSIX locks and sets extended attributes for the
// end-to-end tests, which alpine's busybox cannot.
//
//	locker posix PATH SECONDS [MARK]  wait for a write lock on the whole file,
//	                                  make MARK once it holds it, hold it
//	                                  SECONDS, and print "locked"
//	locker try PATH                   print "locked" if a write lock can be
//	                                  had at once, else "busy"
//	locker setx PATH NAME VALUE       set an extended attribute
//	locker getx PATH NAME             print an extended attribute
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 3 {
		fail("usage: locker posix|try|setx|getx PATH ...")
	}
	path := os.Args[2]
	switch os.Args[1] {
	case "posix", "try":
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			fail("%v", err)
		}
		lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: 0}
		cmd := unix.F_SETLKW
		if os.Args[1] == "try" {
			cmd = unix.F_SETLK
		}
		if err := unix.FcntlFlock(f.Fd(), cmd, &lk); err != nil {
			if err == unix.EAGAIN || err == unix.EACCES {
				fmt.Println("busy")
				return
			}
			fail("lock: %v", err)
		}
		if os.Args[1] == "posix" && len(os.Args) > 3 {
			secs, _ := strconv.Atoi(os.Args[3])
			if len(os.Args) > 4 {
				os.WriteFile(os.Args[4], nil, 0o644)
			}
			time.Sleep(time.Duration(secs) * time.Second)
		}
		fmt.Println("locked")
	case "setx":
		if err := syscall.Setxattr(path, os.Args[3], []byte(os.Args[4]), 0); err != nil {
			fail("setxattr: %v", err)
		}
	case "getx":
		buf := make([]byte, 256)
		n, err := syscall.Getxattr(path, os.Args[3], buf)
		if err != nil {
			fail("getxattr: %v", err)
		}
		fmt.Println(string(buf[:n]))
	default:
		fail("unknown command %s", os.Args[1])
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "locker: "+format+"\n", a...)
	os.Exit(1)
}
