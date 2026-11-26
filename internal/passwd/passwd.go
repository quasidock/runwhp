// Package passwd resolves a container user specification against the
// image's /etc/passwd and /etc/group, the way Docker and runc do.
package passwd

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// User is a resolved user.
type User struct {
	UID, GID uint32
	Groups   []uint32 // supplementary groups
	Home     string
}

type entry struct {
	name    string
	id      uint32
	gid     uint32 // passwd only
	home    string // passwd only
	members []string
}

func parse(data []byte, passwd bool) []entry {
	var out []entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if passwd && len(f) < 7 || !passwd && len(f) < 4 {
			continue
		}
		id, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			continue
		}
		e := entry{name: f[0], id: uint32(id)}
		if passwd {
			gid, err := strconv.ParseUint(f[3], 10, 32)
			if err != nil {
				continue
			}
			e.gid, e.home = uint32(gid), f[5]
		} else if f[3] != "" {
			e.members = strings.Split(f[3], ",")
		}
		out = append(out, e)
	}
	return out
}

// Names maps the user IDs in an /etc/passwd to their names, the first
// name for each, as ps shows them.
func Names(passwdData []byte) map[uint32]string {
	names := map[uint32]string{}
	for _, e := range parse(passwdData, true) {
		if _, ok := names[e.id]; !ok {
			names[e.id] = e.name
		}
	}
	return names
}

func numeric(s string) (uint32, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), err == nil
}

// Resolve turns spec (empty, "user", "uid", "user:group" or "uid:gid") into
// ids. passwd and group are the contents of the image's files and may be
// empty. A numeric user that is not in passwd is accepted with group 0 and
// home "/", as runc does; a name that is not found is an error.
func Resolve(spec string, passwdData, groupData []byte) (User, error) {
	users, groups := parse(passwdData, true), parse(groupData, false)
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" {
		userPart = "0"
	}

	u := User{Home: "/"}
	var found *entry
	for i := range users {
		e := &users[i]
		if e.name == userPart {
			found = e
			break
		}
	}
	if found == nil {
		if id, ok := numeric(userPart); ok {
			u.UID = id
			for i := range users {
				if users[i].id == id {
					found = &users[i]
					break
				}
			}
		} else {
			return u, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
	}
	if found != nil {
		u.UID, u.GID, u.Home = found.id, found.gid, found.home
		if u.Home == "" {
			u.Home = "/"
		}
	}

	if hasGroup {
		gid, ok := numeric(groupPart)
		if !ok {
			ok = false
			for _, g := range groups {
				if g.name == groupPart {
					gid, ok = g.id, true
					break
				}
			}
			if !ok {
				return u, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
			}
		}
		u.GID = gid
		return u, nil
	}

	// Without an explicit group, the user also gets the groups that list
	// them as a member.
	if found != nil {
		seen := map[uint32]bool{u.GID: true}
		for _, g := range groups {
			for _, m := range g.members {
				if m == found.name && !seen[g.id] {
					seen[g.id] = true
					u.Groups = append(u.Groups, g.id)
				}
			}
		}
	}
	return u, nil
}
