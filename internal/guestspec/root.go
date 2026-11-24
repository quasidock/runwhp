package guestspec

import (
	"io/fs"
	"time"
)

// RootPort is the vsock port on which init serves the container's files
// to the host, for runwhp cp, export, diff and commit: in a container's
// guest from the moment its root is ready, before its process starts, and
// in a guest booted with Spec.Serve set to ServeRoot for a stopped one.
//
// Each connection carries one request. The host sends a RootRequest as a
// JSON line, and init answers with a RootReply line, then, for RootGet,
// RootExport and RootDiff, a tar of the files, uncompressed, until it
// closes the connection. For RootPut, the host sends the tar after init's
// first reply says it may, then ends its side of the connection; init
// extracts it and sends a second reply. A tar init cannot finish ends
// early, so the host sees it cut short.
//
// Paths are the container's, resolved as Docker's daemon resolves them:
// symlinks in the directory part are followed within the container's
// root, mounts included, and the last element is left as it is.
const RootPort = 1026

// ServeRoot is Spec.Serve for a guest booted with a stopped container's
// storage: init mounts its root, volumes and binds as for the container,
// runs no process, serves RootPort, sends an Event with Ready, and waits
// for Control.Shutdown.
const ServeRoot = "root"

// Root operations.
const (
	RootStat    = "stat"    // describe Path, in Stat
	RootGet     = "get"     // a tar of Path, its name rebased as Docker's
	RootPut     = "put"     // extract a tar into the directory Path
	RootExport  = "export"  // a tar of the container's root file system, without its mounts
	RootChanges = "changes" // what the writable layer changed, in Changes
	RootDiff    = "diff"    // a tar of the writable layer, as an image layer
)

// RootRequest is the host's request.
type RootRequest struct {
	Op   string `json:"op"`
	Path string `json:"path,omitempty"`
	// NoOverwriteDirNonDir keeps a RootPut from replacing a directory
	// with a file or a file with a directory.
	NoOverwriteDirNonDir bool `json:"noOverwriteDirNonDir,omitempty"`
	// CopyUIDGID gives what a RootPut extracts the container's user's
	// owner, as docker cp -a does, rather than the tar's.
	CopyUIDGID bool `json:"copyUIDGID,omitempty"`
}

// RootReply is init's answer.
type RootReply struct {
	Error string `json:"error,omitempty"`
	// NotFound says Error is that Path does not exist.
	NotFound bool      `json:"notFound,omitempty"`
	Stat     *PathStat `json:"stat,omitempty"`
	Changes  []Change  `json:"changes,omitempty"`
}

// PathStat describes a file, as Docker's container path stat does.
type PathStat struct {
	Name  string      `json:"name"`
	Size  int64       `json:"size"`
	Mode  fs.FileMode `json:"mode"`
	Mtime time.Time   `json:"mtime"`
	// LinkTarget is where a symlink leads, resolved in the container.
	LinkTarget string `json:"linkTarget,omitempty"`
}

// Change kinds, Docker's numbers.
const (
	ChangeModify = 0
	ChangeAdd    = 1
	ChangeDelete = 2
)

// Change is one path the writable layer changed.
type Change struct {
	Path string `json:"path"`
	Kind int    `json:"kind"`
}
