package kernel

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
)

// Pin names one published build of runwhp's kernel. runwhp fetches it by
// digest alone; the tag is there for people, and for the publishing job to
// refuse to move it.
type Pin struct {
	Repository string // such as ghcr.io/quasidock/runwhp-kernel
	Tag        string // upstream version and build revision, such as 6.18.64-1
	Digest     string // the manifest digest
}

// Default is the kernel runwhp boots unless --kernel or RUNWHP_KERNEL names
// another. "Kernel" in README.md describes how to publish a new build and
// move this to it.
var Default = Pin{
	Repository: "ghcr.io/quasidock/runwhp-kernel",
	Tag:        "6.18.64-1",
	Digest:     "",
}

func (p Pin) String() string { return p.Tag }

// Reference is where the pinned build is fetched from.
func (p Pin) Reference() (name.Digest, error) {
	if p.Digest == "" {
		return name.Digest{}, fmt.Errorf("this runwhp was built without a kernel digest; pass --kernel PATH or set RUNWHP_KERNEL")
	}
	d, err := name.NewDigest(p.Repository + "@" + p.Digest)
	if err != nil {
		return name.Digest{}, fmt.Errorf("kernel pin %s: %w", p.Tag, err)
	}
	return d, nil
}
