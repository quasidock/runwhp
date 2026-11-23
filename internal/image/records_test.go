package image

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRecords pulls three references, two of them to one image, and lists,
// finds and removes their records.
func TestRecords(t *testing.T) {
	host, _ := testRegistry(t)
	base := tarLayer(t, entry{name: "etc/", dir: true}, entry{name: "etc/os-release", body: "NAME=test\n"})
	one := push(t, host+"/test/app:1", "amd64", base, tarLayer(t, entry{name: "one", body: "1"}))
	push(t, host+"/test/app:latest", "amd64", base, tarLayer(t, entry{name: "one", body: "1"}))
	two := push(t, host+"/test/other:2", "amd64", base, tarLayer(t, entry{name: "two", body: "2"}))

	s := Open(t.TempDir())
	ctx := context.Background()
	for _, ref := range []string{host + "/test/app:1", host + "/test/app:latest", host + "/test/other:2"} {
		if _, err := s.Pull(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	oneID, _ := one.ConfigName()
	twoID, _ := two.ConfigName()

	img, err := s.Get(host + "/test/app:1")
	if err != nil {
		t.Fatal(err)
	}
	if img.ConfigDigest != oneID.String() || img.ID() != oneID.String() {
		t.Errorf("ConfigDigest %s, ID %s; want %s", img.ConfigDigest, img.ID(), oneID)
	}
	// A record from before iteration 12 has its ID worked out from the
	// configuration it holds, which the record keeps re-indented.
	if !strings.Contains(string(img.Config), "\n") {
		t.Fatal("the record's configuration is not re-indented; the fallback below proves nothing")
	}
	old := *img
	old.ConfigDigest = ""
	if old.ID() != oneID.String() {
		t.Errorf("ID without ConfigDigest = %s, want %s", old.ID(), oneID)
	}

	// Records being written, and junk, are not images.
	images := filepath.Join(s.Root, "images")
	os.WriteFile(filepath.Join(images, "abc.json.tmp"), []byte("{"), 0o644)
	os.WriteFile(filepath.Join(images, "junk.json"), []byte("not json"), 0o644)
	recs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("List returned %d records, want 3 and the junk", len(recs))
	}
	for _, r := range recs[:3] {
		if r.Image == nil || !r.Complete || r.Pulled.IsZero() || r.Err != nil {
			t.Errorf("record %s: %+v", r.Path, r)
		}
	}
	if junk := recs[3]; junk.Image != nil || junk.Err == nil || junk.Complete || filepath.Base(junk.Path) != "junk.json" {
		t.Errorf("junk record: %+v", junk)
	}

	// By reference. TestFindAmbiguous finds Docker Hub's by short names.
	if found, err := s.Find(host + "/test/other:2"); err != nil || len(found) != 1 || found[0].ID() != twoID.String() {
		t.Errorf("Find(other:2) = %v, %v", found, err)
	}
	// By ID, whole or a prefix, with or without its algorithm: both of the
	// first image's references.
	for _, arg := range []string{oneID.String(), oneID.Hex, oneID.Hex[:12], "sha256:" + oneID.Hex[:5]} {
		found, err := s.Find(arg)
		if err != nil || len(found) != 2 || found[0].ID() != oneID.String() || found[1].ID() != oneID.String() {
			t.Errorf("Find(%s) = %d records, %v", arg, len(found), err)
		}
	}
	for _, arg := range []string{"nothere", "zz12", "", strings.Repeat("a", 65)} {
		if _, err := s.Find(arg); !errors.Is(err, ErrNoSuchImage) {
			t.Errorf("Find(%q): %v", arg, err)
		}
	}

	// Removing a record leaves its layers.
	found, _ := s.Find(host + "/test/app:1")
	if err := s.Remove(found[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(found[0]); err != nil {
		t.Errorf("removing it again: %v", err)
	}
	if _, err := s.Get(host + "/test/app:1"); !errors.Is(err, ErrNotPresent) {
		t.Errorf("Get after Remove: %v", err)
	}
	if _, err := s.Get(host + "/test/app:latest"); err != nil {
		t.Errorf("the other reference to the image: %v", err)
	}

	// A record whose layer is gone is incomplete.
	layers, _ := img.Layers()
	p, _ := s.Layers.Path(layers[1])
	os.Remove(p)
	found, _ = s.Find(host + "/test/app:latest")
	if len(found) != 1 || found[0].Complete {
		t.Errorf("a record missing a layer: %+v", found)
	}
}

// TestFindAmbiguous finds images by ID prefixes that two IDs share.
func TestFindAmbiguous(t *testing.T) {
	s := Open(t.TempDir())
	cfg := json.RawMessage(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	put := func(ref, id string) {
		t.Helper()
		if err := s.Put(ref, &Image{Ref: "index.docker.io/library/" + ref, Digest: "sha256:" + strings.Repeat("0", 64), ConfigDigest: id, Config: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	a := "sha256:abcd1" + strings.Repeat("0", 59)
	b := "sha256:abcd2" + strings.Repeat("0", 59)
	put("a:1", a)
	put("b:1", b)
	if _, err := s.Find("abcd"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("Find(abcd): %v", err)
	}
	if found, err := s.Find("abcd2"); err != nil || len(found) != 1 || found[0].Ref != "index.docker.io/library/b:1" {
		t.Errorf("Find(abcd2) = %v, %v", found, err)
	}
	if found, err := s.Find("a:1"); err != nil || len(found) != 1 || found[0].ID() != a {
		t.Errorf("Find(a:1) = %v, %v", found, err)
	}
}

func TestListOrder(t *testing.T) {
	s := Open(t.TempDir())
	for i, ref := range []string{"old:1", "new:1", "mid:1"} {
		created := time.Date(2026, 1, 1+[]int{0, 2, 1}[i], 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		cfg := json.RawMessage(`{"created":"` + created + `","architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
		if err := s.Put(ref, &Image{Ref: "index.docker.io/library/" + ref, Config: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, Familiar(r.Ref))
	}
	if strings.Join(got, " ") != "new:1 mid:1 old:1" {
		t.Errorf("List order: %v", got)
	}
	if recs, err := Open(t.TempDir()).List(); err != nil || len(recs) != 0 {
		t.Errorf("List of an empty store: %v, %v", recs, err)
	}
}

func TestDisplay(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	for _, c := range []struct{ ref, familiar, repo, tag string }{
		{"index.docker.io/library/alpine:3.20", "alpine:3.20", "alpine", "3.20"},
		{"index.docker.io/bitnami/redis:7", "bitnami/redis:7", "bitnami/redis", "7"},
		{"ghcr.io/quasidock/runwhp-kernel:6.18", "ghcr.io/quasidock/runwhp-kernel:6.18", "ghcr.io/quasidock/runwhp-kernel", "6.18"},
		{"localhost:5000/e2e/app:1", "localhost:5000/e2e/app:1", "localhost:5000/e2e/app", "1"},
		{"index.docker.io/library/alpine@" + digest, "alpine@" + digest, "alpine", "<none>"},
	} {
		if got := Familiar(c.ref); got != c.familiar {
			t.Errorf("Familiar(%s) = %s, want %s", c.ref, got, c.familiar)
		}
		if repo, tag := Display(c.ref); repo != c.repo || tag != c.tag {
			t.Errorf("Display(%s) = %s %s, want %s %s", c.ref, repo, tag, c.repo, c.tag)
		}
	}
}

// TestPutAtOnce records one reference from many goroutines, as runs pulling
// the same image at once do, while others list the records.
func TestPutAtOnce(t *testing.T) {
	s := Open(t.TempDir())
	cfg := json.RawMessage(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := s.Put("alpine:3.20", &Image{Ref: "index.docker.io/library/alpine:3.20", Config: cfg}); err != nil {
					t.Error(err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := s.Get("alpine:3.20"); err != nil && !errors.Is(err, ErrNotPresent) {
					t.Error(err)
				}
				recs, err := s.List()
				for _, r := range recs {
					if r.Err != nil {
						t.Error(r.Err)
					}
				}
				if err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if recs, _ := s.List(); len(recs) != 1 {
		t.Errorf("%d records", len(recs))
	}
}
