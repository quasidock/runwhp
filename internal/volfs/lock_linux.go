package volfs

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"

	"github.com/quasidock/runwhp/internal/fuse"
)

// Locks.
//
// A guest's POSIX locks belong to a lock owner, its process's files, and
// go when that process closes any descriptor for the file. Each owner's
// locks on a file are kept as open file description locks on a descriptor
// of the owner's own, which FlushOwner closes: so they conflict with every
// other owner's, in this guest or another, as POSIX locks between
// processes do. flock(2) locks are kept on the open file's own descriptor,
// and go when it is released.
//
// A lock request that waits polls: this server makes every change to the
// volume's locks, so it wakes waiters when one is let go, and polls every
// lockPoll in case it missed one. A guest's kernel cannot interrupt the
// request: it waits until the lock is granted or the session ends.

const lockPoll = 50 * time.Millisecond

// ownerFd returns the descriptor holding owner's POSIX locks on n, opening
// one if create is set; -1 if there is none.
func (s *Session) ownerFd(n *node, owner uint64, create bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fd, ok := n.owners[owner]; ok {
		return fd, nil
	}
	if !create {
		return -1, nil
	}
	flags := unix.O_RDWR
	if n.mode != fuse.ModeFile {
		flags = unix.O_RDONLY
	}
	fd, err := unix.Open(procPath(n.fd), flags|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return -1, errno(err)
	}
	n.owners[owner] = fd
	return fd, nil
}

// flockOf is lk as fcntl takes it, which has a length, 0 for the rest of
// the file, where FUSE has the last byte.
func flockOf(lk fuse.Lock) unix.Flock_t {
	fl := unix.Flock_t{Type: int16(lk.Type), Whence: 0, Start: int64(min(lk.Start, fuse.LockToEnd))}
	if lk.End < fuse.LockToEnd && lk.End >= lk.Start {
		fl.Len = int64(lk.End - lk.Start + 1)
	}
	return fl
}

func (s *Session) Getlk(c fuse.Context, id, fh, owner uint64, lk fuse.Lock, flags uint32) (fuse.Lock, error) {
	n, err := s.node(id)
	if err != nil {
		return fuse.Lock{}, err
	}
	fd, err := s.ownerFd(n, owner, true)
	if err != nil {
		return fuse.Lock{}, err
	}
	fl := flockOf(lk)
	if err := unix.FcntlFlock(uintptr(fd), unix.F_OFD_GETLK, &fl); err != nil {
		return fuse.Lock{}, errno(err)
	}
	if fl.Type == unix.F_UNLCK {
		return fuse.Lock{Type: fuse.LockUnlock}, nil
	}
	got := fuse.Lock{Start: uint64(fl.Start), End: fuse.LockToEnd, Type: uint32(fl.Type)}
	if fl.Len > 0 {
		got.End = uint64(fl.Start + fl.Len - 1)
	}
	// Who holds it is not known: an open file description lock has no
	// process, and the holder may be in another guest.
	return got, nil
}

func (s *Session) Setlk(c fuse.Context, id, fh, owner uint64, lk fuse.Lock, flags uint32, wait bool) error {
	n, err := s.node(id)
	if err != nil {
		return err
	}
	var try func() error
	if flags&fuse.LockFlock != 0 {
		h, err := s.handle(fh)
		if err != nil {
			return err
		}
		how := unix.LOCK_UN
		switch lk.Type {
		case fuse.LockRead:
			how = unix.LOCK_SH
		case fuse.LockWrite:
			how = unix.LOCK_EX
		}
		try = func() error { return unix.Flock(h.fd, how|unix.LOCK_NB) }
	} else {
		fd, err := s.ownerFd(n, owner, lk.Type != fuse.LockUnlock)
		if err != nil {
			return err
		}
		if fd < 0 {
			return nil // nothing of owner's to unlock
		}
		fl := flockOf(lk)
		try = func() error { return unix.FcntlFlock(uintptr(fd), unix.F_OFD_SETLK, &fl) }
	}
	if lk.Type == fuse.LockUnlock {
		err := try()
		s.v.wake(n.ino)
		return errno(err)
	}
	for {
		changed := s.v.changed(n.ino)
		err := try()
		if !errors.Is(err, unix.EAGAIN) || !wait {
			if err == nil && lk.Type == fuse.LockRead {
				// A write lock taken down to a read lock lets readers in.
				s.v.wake(n.ino)
			}
			return errno(err)
		}
		select {
		case <-changed:
		case <-time.After(lockPoll):
		case <-s.closed:
			return fuse.EIO
		}
	}
}

func (s *Session) FlushOwner(id, fh, owner uint64) {
	s.mu.Lock()
	n, ok := s.nodes[id]
	fd := -1
	if ok {
		if f, held := n.owners[owner]; held {
			fd = f
			delete(n.owners, owner)
		}
	}
	s.mu.Unlock()
	if fd >= 0 {
		unix.Close(fd)
		s.v.wake(n.ino)
	}
}
