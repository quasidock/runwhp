//go:build windows

package main

import (
	"strings"
	"testing"

	"github.com/quasidock/runwhp/internal/image"
)

func TestResolveSave(t *testing.T) {
	root, _, _, _ := imageStore(t)
	is := image.Open(root)
	refs := func(args ...string) string {
		t.Helper()
		groups, err := resolveSave(is, args)
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, g := range groups {
			var names []string
			for _, r := range g.refs {
				names = append(names, image.Familiar(r))
			}
			out = append(out, g.recs[0].ID()[7:11]+"="+strings.Join(names, ","))
		}
		return strings.Join(out, " ")
	}
	one, two := sha("one")[7:11], sha("two")[7:11]
	for args, want := range map[string]string{
		"app:1":                          one + "=app:1",
		"app":                            one + "=app:1,app:latest",
		"app:1 app:1":                    one + "=app:1",
		"app:1 other:1":                  one + "=app:1 " + two + "=other:1",
		sha("one")[7:19]:                 one + "=",
		"app:latest " + sha("one")[7:13]: one + "=app:latest",
		"docker.io/library/other":        two + "=other:1",
		"nothere:1":                      "error: no such image: nothere:1",
	} {
		if got := refs(strings.Fields(args)...); got != want {
			t.Errorf("%s: %s, want %s", args, got, want)
		}
	}
	// Every record of an image, named or not, serves to fetch it.
	groups, _ := resolveSave(is, []string{"app:1"})
	if len(groups[0].recs) != 2 {
		t.Errorf("records of app:1's image: %d", len(groups[0].recs))
	}
}

func TestRepositoryOnly(t *testing.T) {
	for arg, want := range map[string]string{
		"alpine":             "index.docker.io/library/alpine",
		"localhost:5000/app": "localhost:5000/app",
		"alpine:3.20":        "",
		"alpine@sha256:" + strings.Repeat("0", 64): "",
		"localhost:5000/app:1":                     "",
		"UPPER":                                    "",
	} {
		got, ok := repositoryOnly(arg)
		if got != want || ok != (want != "") {
			t.Errorf("repositoryOnly(%s) = %s, %v; want %s", arg, got, ok, want)
		}
	}
}

func TestSaveArgs(t *testing.T) {
	imageStore(t)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"save"}, 2},
		{[]string{"save", "-o"}, 2},
		{[]string{"save", "-o", t.TempDir() + `\x.tar`, "nothere:1"}, 1},
	} {
		if _, code := imageCmd("", c.args...); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
	}
}
