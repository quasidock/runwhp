// Package compose reads compose files, with compose-go, the library Docker
// Compose reads them with, and turns a project's services into the
// containers runwhp makes for them, named and labelled as Docker Compose
// names and labels its own, so that a project's containers, networks and
// volumes are found again by their labels. It builds on any OS; running
// what it describes is the command line's.
package compose

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"
)

// The labels Docker Compose puts on what it makes, and runwhp with it.
const (
	LabelProject     = "com.docker.compose.project"
	LabelService     = "com.docker.compose.service"
	LabelNumber      = "com.docker.compose.container-number"
	LabelOneoff      = "com.docker.compose.oneoff"
	LabelSlug        = "com.docker.compose.slug"
	LabelConfigHash  = "com.docker.compose.config-hash"
	LabelImage       = "com.docker.compose.image"
	LabelDependsOn   = "com.docker.compose.depends_on"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelVersion     = "com.docker.compose.version"
	LabelNetwork     = "com.docker.compose.network"
	LabelVolume      = "com.docker.compose.volume"
)

// Version is the version of compose-go runwhp reads compose files with,
// which runwhp gives as its compose version.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, m := range bi.Deps {
			if m.Path == "github.com/compose-spec/compose-go/v2" {
				return strings.TrimPrefix(m.Version, "v")
			}
		}
	}
	return "2"
}

// LoadOptions are compose's project flags.
type LoadOptions struct {
	Files      []string // -f; - reads stdin
	ProjectDir string   // --project-directory
	Name       string   // -p
	Profiles   []string // --profile
	EnvFiles   []string // --env-file
	// Warnings is where compose-go's warnings go, a line each.
	Warnings io.Writer
}

// Load reads a project as Docker Compose does: COMPOSE_FILE,
// COMPOSE_PROJECT_NAME, COMPOSE_PROFILES and the files and .env it finds,
// interpolated, merged, extended and normalized, with relative paths made
// absolute.
func Load(ctx context.Context, o LoadOptions) (*types.Project, error) {
	routeWarnings(o.Warnings)
	opts := []cli.ProjectOptionsFn{
		cli.WithOsEnv,
		cli.WithEnvFiles(o.EnvFiles...),
		cli.WithDotEnv,
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		cli.WithName(o.Name),
		cli.WithResolvedPaths(true),
	}
	if o.ProjectDir != "" {
		opts = append([]cli.ProjectOptionsFn{cli.WithWorkingDirectory(o.ProjectDir)}, opts...)
	}
	profiles := o.Profiles
	opts = append(opts, func(po *cli.ProjectOptions) error {
		// COMPOSE_PROFILES, as Docker Compose reads it, unless --profile.
		if len(profiles) == 0 {
			if p, ok := po.Environment["COMPOSE_PROFILES"]; ok && p != "" {
				profiles = strings.Split(p, ",")
			}
		}
		return cli.WithProfiles(profiles)(po)
	})
	po, err := cli.NewProjectOptions(o.Files, opts...)
	if err != nil {
		return nil, err
	}
	p, err := po.LoadProject(ctx)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// routeWarnings sends compose-go's warnings, which it logs with logrus, to
// w as runwhp's own.
func routeWarnings(w io.Writer) {
	if w == nil {
		w = io.Discard
	}
	logrus.SetOutput(w)
	logrus.SetLevel(logrus.WarnLevel)
	logrus.SetFormatter(warningFormatter{})
}

type warningFormatter struct{}

func (warningFormatter) Format(e *logrus.Entry) ([]byte, error) {
	return []byte("runwhp: compose: warning: " + e.Message + "\n"), nil
}

// ContainerName is the name of a service's container number n, as Docker
// Compose names it, unless the service names its container.
func ContainerName(project string, s types.ServiceConfig, n int) string {
	if s.ContainerName != "" {
		return s.ContainerName
	}
	return fmt.Sprintf("%s-%s-%d", project, s.Name, n)
}

// OneoffName is the name of a one-off container compose run makes, from
// its slug.
func OneoffName(project, service, slug string) string {
	return fmt.Sprintf("%s-%s-run-%s", project, service, slug[:12])
}

// ImageName is the image a service runs: its image, or the one compose
// builds for it.
func ImageName(project string, s types.ServiceConfig) string {
	if s.Image != "" {
		return s.Image
	}
	return project + "-" + s.Name
}

// DependsOnLabel is the depends_on label Docker Compose gives a
// container: each dependency as name:condition:restart, comma separated,
// in name order.
func DependsOnLabel(s types.ServiceConfig) string {
	var parts []string
	for _, name := range sortedKeys(s.DependsOn) {
		d := s.DependsOn[name]
		parts = append(parts, fmt.Sprintf("%s:%s:%t", name, d.Condition, d.Restart))
	}
	return strings.Join(parts, ",")
}
