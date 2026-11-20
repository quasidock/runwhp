// Command push publishes a kernel build as runwhp's kernel artifact. CI runs
// it (see .github/workflows/kernel.yml) on the output of kernel/Dockerfile's
// release target:
//
//	go run ./kernel/push -dir out/release [-repo ghcr.io/quasidock/runwhp-kernel] [-dry-run]
//
// The artifact is tagged VERSION-REVISION from build-info.json. A tag is
// never moved: if it already names a different manifest, push fails and asks
// for a new KERNEL_REVISION; if it names the same one, there is nothing to
// do. The manifest depends only on the build, so a rebuild that reproduces
// the same bytes has the same digest.
//
// On success it prints tag=, digest= and ref= lines, the format of
// $GITHUB_OUTPUT; everything else goes to stderr.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/quasidock/runwhp/internal/kernel"
)

// buildInfo is build-info.json, written by kernel/build.sh.
type buildInfo struct {
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	Release      string `json:"release"`
	Created      string `json:"created"`
	Source       string `json:"source"`
	SourceSHA256 string `json:"sourceSHA256"`
	Builder      string `json:"builder"`
	Snapshot     string `json:"snapshot"`
}

func main() {
	dir := flag.String("dir", "out/release", "directory holding the release target's output")
	repo := flag.String("repo", "ghcr.io/quasidock/runwhp-kernel", "repository to push to")
	project := flag.String("project", "https://github.com/quasidock/runwhp", "URL of the project that builds the kernel")
	dryRun := flag.Bool("dry-run", false, "check the tag and print the digest, but push nothing")
	flag.Parse()
	if err := run(*dir, *repo, *project, *dryRun); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
}

func run(dir, repo, project string, dryRun bool) error {
	var info buildInfo
	if err := readJSON(filepath.Join(dir, "build-info.json"), &info); err != nil {
		return err
	}
	if info.Version == "" || info.Revision == "" || info.Release == "" {
		return fmt.Errorf("build-info.json lacks the version, revision or release")
	}
	a := &kernel.Artifact{SourceName: "linux-" + info.Version + ".tar.xz"}
	for _, f := range []struct {
		name string
		dst  *[]byte
	}{
		{"bzImage", &a.BzImage},
		{"config", &a.Config},
		{"build.tar", &a.Build},
		{a.SourceName, &a.Source},
	} {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return err
		}
		*f.dst = data
	}
	if sum := sha256.Sum256(a.Source); hex.EncodeToString(sum[:]) != info.SourceSHA256 {
		return fmt.Errorf("%s does not have the checksum build-info.json records", a.SourceName)
	}

	tagName := info.Version + "-" + info.Revision
	tag, err := name.NewTag(repo + ":" + tagName)
	if err != nil {
		return err
	}
	const ns = "io.github.quasidock.runwhp.kernel."
	a.Annotations = map[string]string{
		"org.opencontainers.image.title":       "runwhp kernel",
		"org.opencontainers.image.description": "Linux " + info.Release + ", the guest kernel runwhp boots",
		"org.opencontainers.image.version":     tagName,
		"org.opencontainers.image.created":     info.Created,
		"org.opencontainers.image.source":      project,
		"org.opencontainers.image.licenses":    "GPL-2.0-only",
		ns + "release":                         info.Release,
		ns + "source.url":                      info.Source,
		ns + "source.sha256":                   info.SourceSHA256,
		ns + "builder":                         info.Builder,
		ns + "snapshot":                        info.Snapshot,
	}
	digest, err := a.Digest()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "kernel %s: manifest %s, bzImage %d bytes\n", tagName, digest, len(a.BzImage))

	opts := []remote.Option{remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	existing, err := remote.Head(tag, opts...)
	switch {
	case err == nil && existing.Digest == digest:
		fmt.Fprintf(os.Stderr, "%s already names this build; nothing to push\n", tag)
		dryRun = true
	case err == nil:
		return fmt.Errorf("%s already names %s, a different build; raise KERNEL_REVISION in kernel/Dockerfile", tag, existing.Digest)
	case isStatus(err, http.StatusNotFound):
		// A new tag.
	case isStatus(err, http.StatusUnauthorized), isStatus(err, http.StatusForbidden):
		// ghcr.io refuses to say anything about a package that does not
		// exist yet, or that these credentials cannot read; credentials
		// that can push can also read, so the tag is new to them.
		fmt.Fprintf(os.Stderr, "%s is not visible (%v); taking the tag as new\n", tag, err)
	case dryRun:
		fmt.Fprintf(os.Stderr, "could not check %s (%v); it may be private or not exist yet\n", tag, err)
	default:
		return fmt.Errorf("checking %s: %w", tag, err)
	}

	if !dryRun {
		pushed, err := kernel.Push(tag, a, opts...)
		if err != nil {
			return fmt.Errorf("pushing %s: %w", tag, err)
		}
		if pushed != digest {
			return fmt.Errorf("pushed %s, expected %s", pushed, digest)
		}
		fmt.Fprintf(os.Stderr, "pushed %s@%s\n", tag, digest)
	}
	fmt.Printf("tag=%s\ndigest=%s\nref=%s@%s\n", tagName, digest, repo, digest)
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func isStatus(err error, code int) bool {
	var terr *transport.Error
	return errors.As(err, &terr) && terr.StatusCode == code
}
