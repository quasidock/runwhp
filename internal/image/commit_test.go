package image

import (
	"archive/tar"
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestCommit(t *testing.T) {
	s := Open(t.TempDir())
	base, _ := v1.NewHash("sha256:" + strings.Repeat("ab", 32))
	parent := &v1.ConfigFile{
		Architecture: "amd64", OS: "linux",
		Config:  v1.Config{Cmd: []string{"/bin/sh"}, Env: []string{"PATH=/bin"}, Labels: map[string]string{"from": "image"}},
		RootFS:  v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{base}},
		History: []v1.History{{CreatedBy: "ADD rootfs /"}},
	}
	now := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	cfg := parent.Config
	cfg.Cmd, cfg.Env = []string{"sleep", "1"}, []string{"PATH=/bin", "FOO=bar"}
	c := Commit{Parent: parent, Config: cfg, Container: "abc", Changes: []string{"ENV BAZ=1", "LABEL x=y"},
		Author: "Me <me@x>", Message: "msg", CreatedBy: "sleep 1", Now: now}

	diff, _ := tarLayer(t, entry{name: "added", body: "x"}).Uncompressed()
	img, err := s.Commit(c, diff, "mine:1")
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.RootFS.DiffIDs) != 2 || cf.RootFS.DiffIDs[0] != base {
		t.Errorf("diff IDs %v", cf.RootFS.DiffIDs)
	}
	if !s.Layers.Has(cf.RootFS.DiffIDs[1].String()) || !s.Layers.HasSplit(cf.RootFS.DiffIDs[1].String()) {
		t.Error("the new layer is not stored with its metadata")
	}
	if want := []string{"PATH=/bin", "FOO=bar", "BAZ=1"}; !slices.Equal(cf.Config.Env, want) {
		t.Errorf("env %q, want %q", cf.Config.Env, want)
	}
	if cf.Config.Labels["x"] != "y" || cf.Config.Labels["from"] != "image" || !slices.Equal(cf.Config.Cmd, []string{"sleep", "1"}) {
		t.Errorf("config %+v", cf.Config)
	}
	if cf.Author != "Me <me@x>" || cf.Container != "abc" || !cf.Created.Time.Equal(now) {
		t.Errorf("author %q, container %q, created %v", cf.Author, cf.Container, cf.Created)
	}
	last := cf.History[len(cf.History)-1]
	if len(cf.History) != 2 || last.CreatedBy != "sleep 1" || last.Comment != "msg" || last.Author != "Me <me@x>" || last.EmptyLayer {
		t.Errorf("history %+v", cf.History)
	}
	if parent.Config.Labels["x"] != "" || len(parent.History) != 1 {
		t.Error("the parent's configuration was changed")
	}
	if recs, err := s.Find("mine:1"); err != nil || len(recs) != 1 || recs[0].ID() != img.ID() || recs[0].Digest != "" {
		t.Errorf("record: %v", err)
	}
	if raw, err := s.RawConfig(img); err != nil || digestOf(raw) != img.ID() {
		t.Errorf("blob: %v", err)
	}

	// Nothing changed: no layer, an empty one in history, and untagged.
	var empty bytes.Buffer
	tar.NewWriter(&empty).Close()
	img, err = s.Commit(c, &empty, "")
	if err != nil {
		t.Fatal(err)
	}
	cf, _ = img.ConfigFile()
	if len(cf.RootFS.DiffIDs) != 1 || !cf.History[len(cf.History)-1].EmptyLayer {
		t.Errorf("empty commit: %v, %+v", cf.RootFS.DiffIDs, cf.History)
	}
	if img.Ref != img.ConfigDigest || !Untagged(img.Ref) {
		t.Errorf("untagged commit recorded as %q", img.Ref)
	}

	// No parent: the container's layers alone.
	c.Parent, c.Layers = nil, []string{base.String()}
	img, err = s.Commit(c, bytes.NewReader(empty.Bytes()), "")
	if err != nil {
		t.Fatal(err)
	}
	cf, _ = img.ConfigFile()
	if len(cf.RootFS.DiffIDs) != 1 || cf.RootFS.DiffIDs[0] != base || len(cf.History) != 1 || cf.OS != "linux" {
		t.Errorf("commit without a parent: %+v", cf)
	}

	// A change Docker refuses.
	c.Changes = []string{"RUN true"}
	if _, err := s.Commit(c, bytes.NewReader(empty.Bytes()), ""); err == nil {
		t.Error("RUN taken as a change")
	}
}

// TestImportDiffAndRecord stores layers and configurations apart from
// records, as a build does for the states between its steps.
func TestImportDiffAndRecord(t *testing.T) {
	s := Open(t.TempDir())
	if d, empty, err := s.ImportDiff(strings.NewReader("")); err != nil || !empty || d != "" {
		t.Errorf("an empty diff: %q, %v, %v", d, empty, err)
	}
	if d, empty, err := s.ImportDiff(bytes.NewReader(make([]byte, 1024))); err != nil || !empty || d != "" {
		t.Errorf("a tar of no entries: %q, %v, %v", d, empty, err)
	}
	diff, _ := tarLayer(t, entry{name: "f", body: "x"}).Uncompressed()
	d, empty, err := s.ImportDiff(diff)
	if err != nil || empty || !s.Layers.Has(d) || !s.Layers.HasSplit(d) {
		t.Fatalf("a diff: %q, %v, %v", d, empty, err)
	}
	if recs, _ := s.List(); len(recs) != 0 {
		t.Errorf("importing a diff recorded %d images", len(recs))
	}

	raw := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["` + d + `"]}}`)
	id, err := s.PutConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Blob(id); err != nil || !bytes.Equal(got, raw) {
		t.Errorf("blob %s: %q, %v", id, got, err)
	}
	if recs, _ := s.List(); len(recs) != 0 {
		t.Errorf("storing a configuration recorded %d images", len(recs))
	}
	img, err := s.Record("", raw)
	if err != nil || img.Ref != id || img.ID() != id {
		t.Fatalf("untagged record: %+v, %v", img, err)
	}
	n, _ := normalize("built:1")
	if img, err = s.Record(n.Name(), raw); err != nil || img.ID() != id {
		t.Fatalf("tagged record: %+v, %v", img, err)
	}
	if recs, err := s.Find("built:1"); err != nil || len(recs) != 1 || recs[0].ID() != id {
		t.Errorf("find built:1: %v, %v", recs, err)
	}
}
