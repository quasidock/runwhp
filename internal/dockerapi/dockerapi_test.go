package dockerapi

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/jsonstream"
)

func TestSplitVersion(t *testing.T) {
	for _, tc := range []struct{ in, v, rest string }{
		{"/v1.56/containers/json", "1.56", "/containers/json"},
		{"/v1.40/_ping", "1.40", "/_ping"},
		{"/v1.41", "1.41", "/"},
		{"/containers/json", "", "/containers/json"},
		{"/volumes", "", "/volumes"},
		{"/vx.y/info", "", "/vx.y/info"},
		{"/version", "", "/version"},
	} {
		v, rest := SplitVersion(tc.in)
		if v != tc.v || rest != tc.rest {
			t.Errorf("SplitVersion(%q) = %q, %q; want %q, %q", tc.in, v, rest, tc.v, tc.rest)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"", "1.40", "1.44", "1.56"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	if err := CheckVersion("1.57"); err == nil || err.Error() != "client version 1.57 is too new. Maximum supported API version is 1.56" {
		t.Errorf("1.57: %v", err)
	}
	if err := CheckVersion("1.39"); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("1.39: %v", err)
	}
	if CompareVersions("1.9", "1.10") >= 0 {
		t.Error("1.9 sorted after 1.10")
	}
}

func TestDecodeFilters(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want map[string][]string
	}{
		{"", map[string][]string{}},
		{`{"label":["a=b","c"],"Status":["running"]}`, map[string][]string{"label": {"a=b", "c"}, "status": {"running"}}},
		{`{"label":{"c":true,"a=b":true,"off":false}}`, map[string][]string{"label": {"a=b", "c"}}},
	} {
		got, err := DecodeFilters(tc.in)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("DecodeFilters(%s) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	if _, err := DecodeFilters(`{"label":"x"}`); err == nil {
		t.Error("a string value was taken")
	}
	f, _ := DecodeFilters(`{"name":["x"],"bogus":["y"]}`)
	if err := CheckFilterKeys(f, "name", "label"); err == nil || err.Error() != "invalid filter 'bogus'" {
		t.Errorf("CheckFilterKeys: %v", err)
	}
}

func TestStream(t *testing.T) {
	var buf bytes.Buffer
	flushes := 0
	s := NewStream(&buf, func() { flushes++ })
	s.Status("abc", "Downloading", &jsonstream.Progress{Current: 1, Total: 2})
	s.Text("Step 1/2\n")
	s.Aux(map[string]string{"ID": "sha256:x"})
	s.Error(errors.New("no luck"))
	want := `{"status":"Downloading","progressDetail":{"current":1,"total":2},"id":"abc"}` + "\r\n" +
		`{"stream":"Step 1/2\n"}` + "\r\n" +
		`{"aux":{"ID":"sha256:x"}}` + "\r\n" +
		`{"errorDetail":{"message":"no luck"},"error":"no luck"}` + "\r\n"
	if buf.String() != want {
		t.Errorf("got\n%s\nwant\n%s", buf.String(), want)
	}
	if flushes != 4 || !s.Failed {
		t.Errorf("flushes %d, failed %v", flushes, s.Failed)
	}
}
