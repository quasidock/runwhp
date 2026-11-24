//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/quasidock/runwhp/internal/image"
)

const loadUsage = `Usage: runwhp load [-i FILE] [-q]

Load images from an archive, as docker save and runwhp save write it, from
FILE or stdin: Docker's manifest.json, an OCI image layout, or both, with
layers plain or compressed with gzip or zstd. Images keep their IDs, and
their layers the diffIDs they were saved with. Only linux/amd64 images
are taken.

  -i, --input FILE   read FILE instead of stdin
  -q, --quiet        print only what was loaded
`

// cmdLoad runs runwhp load.
func cmdLoad(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := newFlags("load", loadUsage)
	var input string
	var quiet bool
	fs.StringVar(&input, "i", "", "")
	fs.StringVar(&input, "input", "", "")
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	root, err := storeRoot()
	if err != nil {
		errorf("%v", err)
		return 1
	}
	var f *os.File
	if input != "" {
		if f, err = os.Open(input); err != nil {
			errorf("load: %v", err)
			return 1
		}
		defer f.Close()
	} else {
		// The archive is read out of order, so stdin is kept in the store's
		// tmp directory first, as Docker keeps it.
		if sf, ok := stdin.(*os.File); ok && isTerminal(sf) {
			errorf("load: requested load from stdin, but stdin is a console: use -i or redirect")
			return 1
		}
		if f, err = spool(root, stdin); err != nil {
			errorf("load: %v", err)
			return 1
		}
		defer os.Remove(f.Name())
		defer f.Close()
	}
	a, err := image.OpenArchive(f)
	if err != nil {
		errorf("load: %v", err)
		return 1
	}
	_, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	is := image.Open(root)
	if !quiet {
		is.Progress = os.Stderr
	}
	loaded, err := is.Load(a)
	for _, l := range loaded {
		for _, ref := range l.Refs {
			fmt.Fprintf(stdout, "Loaded image: %s\n", image.Familiar(ref))
		}
		if len(l.Refs) == 0 {
			fmt.Fprintf(stdout, "Loaded image ID: %s\n", l.ID)
		}
	}
	if err != nil {
		errorf("load: %v", err)
		return 1
	}
	return 0
}

// spool copies r to a temporary file in the store, open for reading.
func spool(root string, r io.Reader) (*os.File, error) {
	dir := filepath.Join(root, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "load-*.tar")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

const importUsage = `Usage: runwhp import [-c CHANGE]... [-m MESSAGE] FILE|- [REPOSITORY[:TAG]]

Make an image of one layer from a tar of a root file system, from FILE or,
with -, stdin: plain, or compressed with gzip, zstd or bzip2. It is
linux/amd64, and prints its ID. Without REPOSITORY it has no name, and
image prune removes it unless a container uses it.

  -c, --change CHANGE    a Dockerfile instruction for its configuration:
                         CMD, ENTRYPOINT, ENV, EXPOSE, LABEL, ONBUILD,
                         STOPSIGNAL, USER, VOLUME or WORKDIR; repeatable
  -m, --message MESSAGE  the comment its history gives, "Imported from -"
                         unless this says otherwise
  --platform linux/amd64 the only platform runwhp runs
`

// cmdImport runs runwhp import.
func cmdImport(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := newFlags("import", importUsage)
	var changes listFlag
	var message, platform string
	fs.Var(&changes, "c", "")
	fs.Var(&changes, "change", "")
	fs.StringVar(&message, "m", "", "")
	fs.StringVar(&message, "message", "", "")
	fs.StringVar(&platform, "platform", "", "")
	if fs.Parse(args) != nil || fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	if platform != "" && platform != image.Platform.OS+"/"+image.Platform.Architecture {
		errorf("import: runwhp runs only %s/%s images, not %s", image.Platform.OS, image.Platform.Architecture, platform)
		return 1
	}
	src, ref := fs.Arg(0), fs.Arg(1)
	var r io.Reader
	switch {
	case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://"):
		errorf("import: importing from a URL is not supported: download the file, then import it")
		return 1
	case src == "-":
		if sf, ok := stdin.(*os.File); ok && isTerminal(sf) {
			errorf("import: stdin is a console: redirect a file to it, or name the file")
			return 1
		}
		r = stdin
	default:
		f, err := os.Open(src)
		if err != nil {
			errorf("import: %v", err)
			return 1
		}
		defer f.Close()
		r = f
	}
	tarStream, err := image.Decompress(r)
	if err != nil {
		errorf("import: %v", err)
		return 1
	}
	defer tarStream.Close()
	root, release, ok := hold()
	if !ok {
		return 1
	}
	defer release()
	img, err := image.Open(root).Import(tarStream, ref, changes, message, time.Now())
	if err != nil {
		errorf("import: %v", err)
		return 1
	}
	fmt.Fprintln(stdout, img.ID())
	return 0
}
