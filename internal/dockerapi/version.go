// Package dockerapi holds the parts of runwhp's Docker Engine API server
// that need no Windows: API versions, Docker's filters, its JSON message
// streams, and translations between Docker's types and runwhp's. Builds on
// any OS.
package dockerapi

import (
	"fmt"
	"strconv"
	"strings"
)

// APIVersion is the newest Engine API version runwhp serves: that of the
// github.com/moby/moby/api module whose types it answers with.
// MinAPIVersion is the oldest it takes, as Docker 29's daemon.
const (
	APIVersion    = "1.56"
	MinAPIVersion = "1.40"
)

// SplitVersion takes a request path's /vX.Y prefix off it, returning the
// version, empty if there was none, and the rest of the path.
func SplitVersion(path string) (version, rest string) {
	if !strings.HasPrefix(path, "/v") {
		return "", path
	}
	end := strings.IndexByte(path[1:], '/')
	if end < 0 {
		end = len(path) - 1
	}
	v := path[2 : 1+end]
	if _, _, ok := parseVersion(v); !ok {
		return "", path
	}
	rest = path[1+end:]
	if rest == "" {
		rest = "/"
	}
	return v, rest
}

func parseVersion(v string) (major, minor int, ok bool) {
	a, b, found := strings.Cut(v, ".")
	if !found {
		return 0, 0, false
	}
	var err1, err2 error
	major, err1 = strconv.Atoi(a)
	minor, err2 = strconv.Atoi(b)
	return major, minor, err1 == nil && err2 == nil && major >= 0 && minor >= 0
}

// CompareVersions compares two API versions as numbers: -1, 0 or 1.
func CompareVersions(a, b string) int {
	am, an, _ := parseVersion(a)
	bm, bn, _ := parseVersion(b)
	switch {
	case am != bm:
		return cmp(am, bm)
	default:
		return cmp(an, bn)
	}
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CheckVersion refuses a version runwhp does not serve, in Docker's words.
func CheckVersion(v string) error {
	if v == "" {
		return nil
	}
	if CompareVersions(v, APIVersion) > 0 {
		return fmt.Errorf("client version %s is too new. Maximum supported API version is %s", v, APIVersion)
	}
	if CompareVersions(v, MinAPIVersion) < 0 {
		return fmt.Errorf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", v, MinAPIVersion)
	}
	return nil
}
