//go:build windows && compose

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/rootfs"
)

// apiSocketConfigDir is where a service with use_api_socket finds the
// docker CLI's credentials, as Docker Compose puts them there.
const apiSocketConfigDir = "/run/secrets/docker"

// apiSocketCredentials is the docker CLI configuration Docker Compose
// gives a service with use_api_socket: the credentials the user's docker
// CLI has stored, its helpers' included, and nothing else. It is nil when
// there are none, and Docker Compose then gives the service nothing.
func apiSocketCredentials() ([]byte, error) {
	cf, err := config.Load(config.Dir())
	if err != nil {
		return nil, err
	}
	creds, err := cf.GetAllCredentials()
	if err != nil {
		return nil, fmt.Errorf("reading the docker CLI's credentials: %w", err)
	}
	if len(creds) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := (&configfile.ConfigFile{AuthConfigs: creds}).SaveToWriter(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// withAPISocketConfig points the docker CLI in a service with
// use_api_socket at the credentials, if there are any.
func withAPISocketConfig(spec *runSpec, creds []byte) {
	if creds != nil {
		spec.env = append(spec.env, "DOCKER_CONFIG="+apiSocketConfigDir)
	}
}

// putAPISocketConfig writes the credentials into a created container, as
// Docker Compose copies them in before it starts it.
func putAPISocketConfig(ctx context.Context, root string, c *container.Container, creds []byte) error {
	if creds == nil {
		return nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	now := time.Now()
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "run/", Mode: 0o755, ModTime: now},
		{Typeflag: tar.TypeDir, Name: "run/secrets/", Mode: 0o755, ModTime: now},
		{Typeflag: tar.TypeDir, Name: "run/secrets/docker/", Mode: 0o755, ModTime: now},
		{Typeflag: tar.TypeReg, Name: "run/secrets/docker/config.json", Mode: 0o444, Size: int64(len(creds)), ModTime: now},
	} {
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
	}
	tw.Write(creds)
	if err := tw.Close(); err != nil {
		return err
	}
	rc := pipeRoot(root, c, c.ID)
	if err := rc.Put(ctx, "/", &buf, rootfs.PutOptions{NoOverwriteDirNonDir: true}); err != nil {
		return fmt.Errorf("saving the credentials for the API socket: %w", err)
	}
	return nil
}
