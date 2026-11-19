package passwd

import (
	"reflect"
	"testing"
)

const passwdFile = `root:x:0:0:root:/root:/bin/sh
# comment
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
app:x:1000:1000::/home/app:/bin/sh
broken line
`

const groupFile = `root:x:0:
wheel:x:10:root,app
audio:x:29:app
app:x:1000:
nogroup:x:65534:
`

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want User
	}{
		{"", User{UID: 0, GID: 0, Groups: []uint32{10}, Home: "/root"}},
		{"root", User{UID: 0, GID: 0, Groups: []uint32{10}, Home: "/root"}},
		{"nobody", User{UID: 65534, GID: 65534, Home: "/nonexistent"}},
		{"app", User{UID: 1000, GID: 1000, Groups: []uint32{10, 29}, Home: "/home/app"}},
		{"1000", User{UID: 1000, GID: 1000, Groups: []uint32{10, 29}, Home: "/home/app"}},
		{"app:audio", User{UID: 1000, GID: 29, Home: "/home/app"}},
		{"app:5", User{UID: 1000, GID: 5, Home: "/home/app"}},
		{"4242", User{UID: 4242, GID: 0, Home: "/"}},
		{"4242:4242", User{UID: 4242, GID: 4242, Home: "/"}},
	} {
		got, err := Resolve(tc.spec, []byte(passwdFile), []byte(groupFile))
		if err != nil {
			t.Errorf("%q: %v", tc.spec, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %+v, want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	for _, spec := range []string{"ghost", "app:ghosts"} {
		if _, err := Resolve(spec, []byte(passwdFile), []byte(groupFile)); err == nil {
			t.Errorf("%q resolved", spec)
		}
	}
	// An image without /etc/passwd can still run as root or a numeric id.
	if u, err := Resolve("", nil, nil); err != nil || u.UID != 0 || u.Home != "/" {
		t.Errorf("empty passwd: %+v, %v", u, err)
	}
	if u, err := Resolve("7:8", nil, nil); err != nil || u.UID != 7 || u.GID != 8 {
		t.Errorf("numeric without passwd: %+v, %v", u, err)
	}
}
