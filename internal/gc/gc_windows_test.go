package gc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quasidock/runwhp/internal/buildcache"
	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/disk"
	"github.com/quasidock/runwhp/internal/image"
	"github.com/quasidock/runwhp/internal/kernel"
	"github.com/quasidock/runwhp/internal/volume"
)

// testRoot is a new empty store, under RUNWHP_TEST_DIR when it is set, so
// that the tests can run on another file system, such as ReFS.
func testRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNWHP_TEST_DIR")
	if base == "" {
		return t.TempDir()
	}
	os.MkdirAll(base, 0o755)
	dir, err := os.MkdirTemp(base, "gc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fixture builds a store by hand: layer files of given sizes, image
// records listing them, and container records.
type fixture struct {
	t    *testing.T
	root string
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// layer writes a layer file of size bytes; large enough that the file
// system keeps it outside its own metadata.
func (f *fixture) layer(name string, size int) string {
	d := digestOf("layer " + name)
	p := filepath.Join(f.root, "layers", "sha256", strings.TrimPrefix(d, "sha256:")+".ext4")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return d
}

// image records ref, normalized as Docker Hub's, with the given ID and
// layers, and returns the record as a container made from it names it.
func (f *fixture) image(ref, id string, layers ...string) *image.Image {
	cfg, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux", "created": "2026-01-01T00:00:00Z",
		"rootfs": map[string]any{"type": "layers", "diff_ids": append([]string{}, layers...)},
	})
	img := &image.Image{Ref: "index.docker.io/library/" + ref, Digest: digestOf("manifest " + ref + id), ConfigDigest: digestOf(id), Config: cfg}
	if err := image.Open(f.root).Put(ref, img); err != nil {
		f.t.Fatal(err)
	}
	return img
}

// container records a container in the given status. A running one holds
// its lock until the test ends.
func (f *fixture) container(name string, status container.Status, edit func(*container.Config)) *container.Container {
	cfg := &container.Config{Version: container.ConfigVersion, ID: container.NewID(), Created: time.Now().UTC(), Args: []string{"true"}}
	edit(cfg)
	s := container.Open(f.root)
	c, err := s.Create(cfg, name)
	if err != nil {
		f.t.Fatal(err)
	}
	l, err := s.Lock(c)
	if err != nil {
		f.t.Fatal(err)
	}
	l.WriteState(container.State{Status: status, Pid: os.Getpid()})
	if status == container.Running || status == container.Created {
		f.t.Cleanup(func() { l.Close() })
	} else {
		l.Close()
	}
	return c
}

func from(img *image.Image) func(*container.Config) {
	return func(cfg *container.Config) {
		layers, _ := img.Layers()
		cfg.Image, cfg.ImageName, cfg.ImageDigest, cfg.Layers = img.Ref, img.Ref, img.Digest, layers
	}
}

func load(t *testing.T, root string) *Usage {
	t.Helper()
	u, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// store is the store most tests start from:
//
//	app:1, app:latest  ID one, layers A and B; c1 was made from app:1
//	other:1            ID two, layers A and C
//	inc:1              ID three, layer F, whose file is gone
//	D                  a layer nothing lists
//	E                  a layer only c2 lists, its image pulled again since
//	G                  a layer only c3 lists, given with --layer
//	junk.json          a record that cannot be read
type store struct {
	root                string
	a, b, c, d, e, f, g string
	app1, applatest     *image.Image
	c1, c2, c3          *container.Container
}

func newStore(t *testing.T) *store {
	f := &fixture{t, testRoot(t)}
	s := &store{root: f.root}
	s.a, s.b, s.c = f.layer("A", 64<<10), f.layer("B", 128<<10), f.layer("C", 256<<10)
	s.d, s.e, s.g = f.layer("D", 64<<10), f.layer("E", 64<<10), f.layer("G", 64<<10)
	s.f = digestOf("layer F")
	s.app1 = f.image("app:1", "one", s.a, s.b)
	s.applatest = f.image("app:latest", "one", s.a, s.b)
	f.image("other:1", "two", s.a, s.c)
	f.image("inc:1", "three", s.f)
	os.WriteFile(filepath.Join(f.root, "images", "junk.json"), []byte("not json"), 0o644)
	s.c1 = f.container("c1", container.Stopped, from(s.app1))
	s.c2 = f.container("c2", container.Stopped, func(cfg *container.Config) {
		cfg.Image, cfg.ImageName, cfg.ImageDigest = "old:1", "index.docker.io/library/old:1", digestOf("old manifest")
		cfg.Layers = []string{s.a, s.e}
	})
	s.c3 = f.container("c3", container.Stopped, func(cfg *container.Config) {
		cfg.LayerFiles = []string{filepath.Join(strings.ToUpper(f.root), "layers", "sha256", strings.TrimPrefix(s.g, "sha256:")+".ext4")}
	})
	return s
}

func (u *Usage) find(t *testing.T, ref string) *Image {
	t.Helper()
	for _, img := range u.Images {
		if img.Ref == "index.docker.io/library/"+ref {
			return img
		}
	}
	t.Fatalf("no image %s", ref)
	return nil
}

func diffIDs(ls []*Layer) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.DiffID)
	}
	sort.Strings(out)
	return out
}

func sorted(s ...string) []string {
	sort.Strings(s)
	return s
}

func refs(recs []*image.Record) []string {
	var out []string
	for _, r := range recs {
		if r.Image == nil {
			out = append(out, filepath.Base(r.Path))
		} else {
			out = append(out, image.Familiar(r.Ref))
		}
	}
	sort.Strings(out)
	return out
}

func TestUsage(t *testing.T) {
	s := newStore(t)
	u := load(t, s.root)

	if len(u.Images) != 4 || len(u.Junk) != 1 || len(u.Containers) != 3 || len(u.Layers) != 6 {
		t.Fatalf("%d images, %d junk, %d containers, %d layers", len(u.Images), len(u.Junk), len(u.Containers), len(u.Layers))
	}
	size := func(d string) int64 { return u.Layers[d].Size }
	if size(s.a) < 64<<10 || size(s.c) < 256<<10 {
		t.Errorf("layer sizes: A %d, C %d", size(s.a), size(s.c))
	}

	app1, applatest, other, inc := u.find(t, "app:1"), u.find(t, "app:latest"), u.find(t, "other:1"), u.find(t, "inc:1")
	if app1.Size != size(s.a)+size(s.b) || app1.Shared != size(s.a) {
		t.Errorf("app:1 size %d, shared %d", app1.Size, app1.Shared)
	}
	// Another reference to the same image shares nothing with it.
	if other.Size != size(s.a)+size(s.c) || other.Shared != size(s.a) {
		t.Errorf("other:1 size %d, shared %d", other.Size, other.Shared)
	}
	if len(app1.Containers) != 1 || app1.Containers[0].ID != s.c1.ID || len(applatest.Containers) != 0 {
		t.Errorf("containers of app:1 %v, of app:latest %v", app1.Containers, applatest.Containers)
	}
	if !app1.Complete || inc.Complete || inc.Size != 0 {
		t.Errorf("complete: app:1 %v, inc:1 %v (size %d)", app1.Complete, inc.Complete, inc.Size)
	}

	rooted := map[string]bool{}
	for d, l := range u.Layers {
		rooted[d] = l.Rooted()
	}
	if want := map[string]bool{s.a: true, s.b: true, s.c: true, s.d: false, s.e: true, s.g: true}; !reflect.DeepEqual(rooted, want) {
		t.Errorf("rooted layers %v, want %v", rooted, want)
	}
	if l := u.Layers[s.e]; len(l.Images) != 0 || len(l.Containers) != 1 || l.Containers[0].ID != s.c2.ID {
		t.Errorf("E: %+v", l)
	}
	if l := u.Layers[s.a]; len(l.Images) != 3 || len(l.Containers) != 2 {
		t.Errorf("A: %d images, %d containers", len(l.Images), len(l.Containers))
	}
}

func TestDangling(t *testing.T) {
	s := newStore(t)
	u := load(t, s.root)
	p := u.Dangling()
	if got := refs(p.Records); !reflect.DeepEqual(got, []string{"inc:1", "junk.json"}) {
		t.Errorf("records %v", got)
	}
	if got := diffIDs(p.Layers); !reflect.DeepEqual(got, []string{s.d}) {
		t.Errorf("layers %v, want D", got)
	}
	r := u.Apply(p)
	if len(r.Errors) > 0 || !reflect.DeepEqual(r.Untagged, []string{"inc:1"}) || !reflect.DeepEqual(r.Deleted, []string{s.d}) || r.Freed != u.Layers[s.d].Size {
		t.Errorf("report %+v", r)
	}
	after := load(t, s.root)
	if len(after.Images) != 3 || len(after.Junk) != 0 || len(after.Layers) != 5 || after.Layers[s.d] != nil {
		t.Errorf("after: %d images, %d junk, %d layers", len(after.Images), len(after.Junk), len(after.Layers))
	}
	if p := after.Dangling(); len(p.Records)+len(p.Layers) != 0 {
		t.Errorf("dangling again: %+v", p)
	}
}

func TestUnused(t *testing.T) {
	s := newStore(t)
	u := load(t, s.root)
	p := u.Unused()
	if got := refs(p.Records); !reflect.DeepEqual(got, []string{"app:latest", "inc:1", "junk.json", "other:1"}) {
		t.Errorf("records %v", got)
	}
	// A and B stay with app:1, which c1 uses; E and G with their containers.
	if got := diffIDs(p.Layers); !reflect.DeepEqual(got, sorted(s.c, s.d)) {
		t.Errorf("layers %v, want C and D", got)
	}
	u.Apply(p)
	after := load(t, s.root)
	if len(after.Images) != 1 || after.Images[0].Ref != s.app1.Ref || len(after.Layers) != 4 {
		t.Errorf("after: %d images, %d layers", len(after.Images), len(after.Layers))
	}
	// With c1 gone, app:1 goes too, but A stays with c2.
	if err := container.Open(s.root).Remove(s.c1); err != nil {
		t.Fatal(err)
	}
	after = load(t, s.root)
	p = after.Unused()
	if got := diffIDs(p.Layers); len(p.Records) != 1 || !reflect.DeepEqual(got, []string{s.b}) {
		t.Errorf("once c1 is gone: %d records, layers %v", len(p.Records), got)
	}
}

func TestRemoveImages(t *testing.T) {
	s := newStore(t)
	u := load(t, s.root)
	oneID := strings.TrimPrefix(digestOf("one"), "sha256:")

	// other:1's own layer goes, and D, which nothing lists, stays for a prune.
	p, errs := u.RemoveImages([]string{"other:1", "other:1"}, false)
	if len(errs) > 0 || !reflect.DeepEqual(refs(p.Records), []string{"other:1"}) || !reflect.DeepEqual(diffIDs(p.Layers), []string{s.c}) {
		t.Errorf("rmi other:1: %v, %v, %v", refs(p.Records), diffIDs(p.Layers), errs)
	}

	// An image a container was made from, only with force; its layers stay
	// with the container.
	if p, errs := u.RemoveImages([]string{"app:1"}, false); len(p.Records) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "used by container c1") {
		t.Errorf("rmi app:1: %v, %v", refs(p.Records), errs)
	}
	if p, errs := u.RemoveImages([]string{"app:1"}, true); len(errs) > 0 || !reflect.DeepEqual(refs(p.Records), []string{"app:1"}) || len(p.Layers) != 0 {
		t.Errorf("rmi -f app:1: %v, %v, %v", refs(p.Records), diffIDs(p.Layers), errs)
	}

	// An ID with two references, only with force.
	if p, errs := u.RemoveImages([]string{oneID[:12]}, false); len(p.Records) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "app:1, app:latest") && !strings.Contains(errs[0].Error(), "app:latest, app:1") {
		t.Errorf("rmi of a shared ID: %v, %v", refs(p.Records), errs)
	}
	if p, errs := u.RemoveImages([]string{oneID}, true); len(errs) > 0 || !reflect.DeepEqual(refs(p.Records), []string{"app:1", "app:latest"}) || len(p.Layers) != 0 {
		t.Errorf("rmi -f of a shared ID: %v, %v, %v", refs(p.Records), diffIDs(p.Layers), errs)
	}

	// app:latest alone is unused, but its layers are app:1's too.
	if p, errs := u.RemoveImages([]string{"app:latest"}, false); len(errs) > 0 || len(p.Records) != 1 || len(p.Layers) != 0 {
		t.Errorf("rmi app:latest: %v, %v, %v", refs(p.Records), diffIDs(p.Layers), errs)
	}

	// Errors for some arguments do not stop the others.
	p, errs = u.RemoveImages([]string{"nothere", "other:1"}, false)
	if len(errs) != 1 || !errors.Is(errs[0], image.ErrNoSuchImage) || len(p.Records) != 1 {
		t.Errorf("rmi nothere other:1: %v, %v", refs(p.Records), errs)
	}
	r := u.Apply(p)
	if !reflect.DeepEqual(r.Untagged, []string{"other:1"}) || !reflect.DeepEqual(r.Deleted, []string{s.c}) {
		t.Errorf("report %+v", r)
	}
}

// TestApplyInUse removes a layer a guest has open.
func TestApplyInUse(t *testing.T) {
	s := newStore(t)
	u := load(t, s.root)
	p := u.Dangling()
	path := filepath.Join(s.root, "layers", "sha256", strings.TrimPrefix(s.d, "sha256:")+".ext4")
	f, err := disk.Open(path, disk.ReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := u.Apply(p)
	if len(r.Errors) > 0 || !reflect.DeepEqual(r.InUse, []string{s.d}) || len(r.Deleted) != 0 || r.Freed != 0 || len(r.Untagged) != 1 {
		t.Errorf("report %+v", r)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the open layer: %v", err)
	}
}

func TestKernels(t *testing.T) {
	root := testRoot(t)
	ks := kernel.Open(root)
	put := func(digest string) string {
		dir, err := ks.Dir(digest)
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "bzImage"), make([]byte, 64<<10), 0o644)
		return dir
	}
	old, given := digestOf("old kernel"), digestOf("given kernel")
	put(kernel.Default.Digest)
	put(old)
	dir := put(given)
	f := &fixture{t, root}
	f.container("kc", container.Stopped, func(cfg *container.Config) { cfg.Kernel = filepath.Join(dir, "bzImage") })

	u := load(t, root)
	p := u.OldKernels()
	if len(p.Kernels) != 1 || p.Kernels[0].Digest != old || p.Kernels[0].Size < 64<<10 {
		t.Fatalf("old kernels %+v", p.Kernels)
	}
	if r := u.Apply(p); !reflect.DeepEqual(r.Kernels, []string{old}) || r.Freed != p.Kernels[0].Size {
		t.Errorf("report %+v", r)
	}
	if got, _ := ks.List(); len(got) != 2 {
		t.Errorf("kernels left %v", got)
	}
}

func TestUnusedVolumes(t *testing.T) {
	root := testRoot(t)
	vs := volume.Open(root)
	for _, name := range []string{"used", "free"} {
		if _, err := vs.Create(name, 64<<20); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t, root}
	f.container("vc", container.Stopped, func(cfg *container.Config) { cfg.Volumes = []container.Volume{{Name: "USED"}} })
	u := load(t, root)
	p := u.UnusedVolumes()
	if len(p.Volumes) != 1 || p.Volumes[0].Name != "free" || p.Volumes[0].Size == 0 {
		t.Fatalf("unused volumes %+v", p.Volumes)
	}
	if r := u.Apply(p); !reflect.DeepEqual(r.Volumes, []string{"free"}) || r.Freed != p.Volumes[0].Size {
		t.Errorf("report %+v", r)
	}
	if left, _ := vs.List(); len(left) != 1 || left[0].Name != "used" {
		t.Errorf("volumes left %v", left)
	}
}

func TestRemovable(t *testing.T) {
	root := testRoot(t)
	f := &fixture{t, root}
	for _, st := range []container.Status{container.Stopped, container.Created, container.Running} {
		f.container(string(st), st, func(*container.Config) {})
	}
	u := load(t, root)
	var got []string
	for _, c := range u.Containers {
		if Removable(c.Container) {
			got = append(got, c.Name)
		}
	}
	if sort.Strings(got); !reflect.DeepEqual(got, []string{"created", "stopped"}) {
		t.Errorf("removable: %v", got)
	}
}

func TestLock(t *testing.T) {
	root := testRoot(t)
	r1, err := Hold(root)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Hold(root)
	if err != nil {
		t.Fatal("a second shared hold:", err)
	}

	// An exclusive lock waits for both.
	var waited atomic.Int32
	got := make(chan func())
	go func() {
		release, err := Exclusive(root, func() { waited.Add(1) })
		if err != nil {
			t.Error(err)
		}
		got <- release
	}()
	r1()
	select {
	case <-got:
		t.Fatal("exclusive with a shared hold left")
	case <-time.After(200 * time.Millisecond):
	}
	r2()
	var release func()
	select {
	case release = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("exclusive not granted once the holds were released")
	}
	if waited.Load() != 1 {
		t.Errorf("waiting called %d times", waited.Load())
	}

	// A shared hold waits for the exclusive one.
	held := make(chan func())
	go func() {
		r, err := Hold(root)
		if err != nil {
			t.Error(err)
		}
		held <- r
	}()
	select {
	case <-held:
		t.Fatal("shared hold granted under an exclusive one")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case r := <-held:
		r()
	case <-time.After(5 * time.Second):
		t.Fatal("shared hold not granted once the exclusive one was released")
	}

	// Uncontended, waiting is not called.
	release, err = Exclusive(root, func() { t.Error("waiting called without a holder") })
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// TestSplitsAndBlobs counts a layer's tar metadata with it and removes it
// with it, and removes configuration blobs no record keeps.
func TestSplitsAndBlobs(t *testing.T) {
	s := newStore(t)
	is := image.Open(s.root)
	// D's tar metadata, and an orphan: metadata whose ext4 image is gone,
	// as a removal that stopped halfway leaves it.
	dsplit, _ := is.Layers.SplitPath(s.d)
	os.WriteFile(dsplit, make([]byte, 64<<10), 0o644)
	orphan := digestOf("layer H")
	hsplit, _ := is.Layers.SplitPath(orphan)
	os.WriteFile(hsplit, make([]byte, 64<<10), 0o644)
	// Blobs: app's and other's configurations, and one no record keeps.
	for _, id := range []string{"one", "two", "gone"} {
		data := []byte(`{"id":"` + id + `"}`)
		p := filepath.Join(s.root, "blobs", "sha256", strings.TrimPrefix(digestOf(id), "sha256:"))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
	}

	u := load(t, s.root)
	if d, a := u.Layers[s.d], u.Layers[s.a]; d.Size <= a.Size {
		t.Errorf("D with its metadata takes %d, no more than A alone, %d", d.Size, a.Size)
	}
	if u.Layers[orphan] == nil || u.Layers[orphan].Rooted() {
		t.Fatalf("orphan metadata: %+v", u.Layers[orphan])
	}
	r := u.Apply(u.Dangling())
	if len(r.Errors) > 0 {
		t.Fatal(r.Errors)
	}
	for _, p := range []string{dsplit, hsplit} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left", p)
		}
	}
	blobs, _ := is.Blobs()
	if strings.Join(blobs, " ") != strings.Join(sortedDigests("one", "two"), " ") {
		t.Errorf("blobs left: %v", blobs)
	}

	// Removing other:1 removes its blob; app's stays with app:latest.
	u = load(t, s.root)
	p, errs := u.RemoveImages([]string{"other:1", "app:1"}, true)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	u.Apply(p)
	if blobs, _ := is.Blobs(); strings.Join(blobs, " ") != digestOf("one") {
		t.Errorf("blobs left after rmi: %v", blobs)
	}
}

func sortedDigests(ids ...string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, digestOf(id))
	}
	sort.Strings(out)
	return out
}

// TestUntaggedDangling prunes an image loaded without a reference, unless
// a container uses it, as Docker's image prune does.
func TestUntaggedDangling(t *testing.T) {
	f := &fixture{t, testRoot(t)}
	a, b := f.layer("A", 64<<10), f.layer("B", 64<<10)
	cfg := func(layers ...string) []byte {
		b, _ := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux",
			"rootfs": map[string]any{"type": "layers", "diff_ids": layers}})
		return b
	}
	is := image.Open(f.root)
	unused, used := digestOf("unused"), digestOf("used")
	is.Put(unused, &image.Image{Ref: unused, ConfigDigest: unused, Config: cfg(a)})
	usedImg := &image.Image{Ref: used, ConfigDigest: used, Config: cfg(b)}
	is.Put(used, usedImg)
	f.container("c1", container.Stopped, from(usedImg))

	u := load(t, f.root)
	p := u.Dangling()
	if len(p.Records) != 1 || p.Records[0].Ref != unused {
		t.Errorf("Dangling records: %v", p.Records)
	}
	if got := diffIDs(p.Layers); len(got) != 1 || got[0] != a {
		t.Errorf("Dangling layers: %v", got)
	}
}

// TestContainerKeepsBlob keeps the configuration of a container's image,
// which commit builds on, after the image's records are gone.
func TestContainerKeepsBlob(t *testing.T) {
	s := newStore(t)
	is := image.Open(s.root)
	data := []byte(`{"id":"removed"}`)
	p := filepath.Join(s.root, "blobs", "sha256", strings.TrimPrefix(digestOf("removed"), "sha256:"))
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, data, 0o644)
	f := &fixture{t, s.root}
	f.container("c4", container.Stopped, func(cfg *container.Config) {
		cfg.Image, cfg.Layers, cfg.ImageID = "removed:1", []string{s.a}, digestOf("removed")
	})
	u := load(t, s.root)
	if r := u.Apply(u.Dangling()); len(r.Errors) > 0 {
		t.Fatal(r.Errors)
	}
	blobs, _ := is.Blobs()
	if !slices.Contains(blobs, digestOf("removed")) {
		t.Errorf("the container's image's configuration was removed: %v", blobs)
	}
}

// TestBuildCache keeps the layers build cache entries name, until a
// builder prune removes the entries: without -a, those whose layer no
// image uses; with it, all, and the cache mounts' volumes.
func TestBuildCache(t *testing.T) {
	f := &fixture{t, testRoot(t)}
	shared, only, gone := f.layer("shared", 64<<10), f.layer("cache only", 64<<10), f.layer("nothing", 64<<10)
	f.image("built:1", "built", shared)
	cache := buildcache.Open(f.root)
	key := func(s string) string { return strings.TrimPrefix(digestOf(s), "sha256:") }
	for _, e := range []buildcache.Entry{
		{Key: key("a"), DiffID: shared}, {Key: key("b"), DiffID: only}, {Key: key("c")},
	} {
		if err := cache.Put(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := volume.Open(cache.VolumeRoot()).Create("cache-x", 64<<20); err != nil {
		t.Fatal(err)
	}
	u := load(t, f.root)
	if len(u.Cache) != 3 || len(u.CacheVolumes) != 1 || !u.Layers[only].Rooted() || u.Layers[only].Used() {
		t.Fatalf("cache %d, volumes %d", len(u.Cache), len(u.CacheVolumes))
	}
	// An image prune leaves the cache's layers.
	if got := diffIDs(u.Unused().Layers); !slices.Equal(got, []string{gone}) {
		t.Errorf("image prune -a removes %q", got)
	}
	p := u.BuildCache(false)
	var keys []string
	for _, e := range p.Cache {
		keys = append(keys, e.Key)
	}
	if !slices.Equal(sorted(keys...), sorted(key("b"), key("c"))) || !slices.Equal(diffIDs(p.Layers), []string{only}) || len(p.CacheVolumes) != 0 {
		t.Errorf("builder prune: entries %q, layers %q", keys, diffIDs(p.Layers))
	}
	// system prune -a: the image and every entry go, and with them every
	// layer.
	all := u.AndCache(u.Unused(), true)
	if got := diffIDs(all.Layers); !slices.Equal(got, sorted(shared, only, gone)) || len(all.CacheVolumes) != 1 {
		t.Errorf("system prune -a removes %q, %d volumes", got, len(all.CacheVolumes))
	}
	r := u.Apply(u.BuildCache(true))
	if len(r.Cache) != 3 || len(r.Errors) > 0 {
		t.Errorf("report %+v", r)
	}
	u = load(t, f.root)
	if len(u.Cache) != 0 || len(u.CacheVolumes) != 0 || u.Layers[only] != nil || u.Layers[shared] == nil {
		t.Errorf("after builder prune -a: %d entries, %d volumes, layers %v", len(u.Cache), len(u.CacheVolumes), u.Layers)
	}
}
