//go:build linux

package main

import (
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/worker/base"
)

// mobyExporterName is the exporter buildx's docker driver asks for, for an
// image in the daemon's store: Docker's embedded BuildKit's own.
const mobyExporterName = "moby"

// runwhpWorker is BuildKit's OCI worker with the moby exporter.
type runwhpWorker struct {
	*base.Worker
	api *dockerAPI
}

func (w *runwhpWorker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	if name != mobyExporterName {
		return w.Worker.Exporter(name, sm)
	}
	image, err := w.Worker.Exporter(client.ExporterImage, sm)
	if err != nil {
		return nil, err
	}
	return &mobyExporter{image: image, content: w.ContentStore(), api: w.api}, nil
}
