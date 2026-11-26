package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// ConfigHash is the hash a container's config-hash label records, from
// which a later up tells whether the service changed: what Docker Compose
// hashes, the service less its build, pull policy, scale, profiles and
// dependencies, which change nothing in its containers.
func ConfigHash(s types.ServiceConfig) string {
	s.Build = nil
	s.PullPolicy = ""
	s.Scale = nil
	if s.Deploy != nil {
		d := *s.Deploy
		d.Replicas = nil
		s.Deploy = &d
	}
	s.Profiles = nil
	s.DependsOn = nil
	return hashOf(s)
}

// ResourceHash is the config-hash label of a network or a volume.
func ResourceHash(v any) string { return hashOf(v) }

func hashOf(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Check looks over a project for what runwhp cannot do as it asks: its
// networks, volumes, secrets and configs, and volumes services would use
// at once, which runwhp cannot share. services are those up would run.
func Check(p *types.Project, services []string, pr *Problems) {
	if len(p.Jobs) > 0 {
		pr.refuse("jobs are not supported")
	}
	for _, key := range sortedKeys(p.Networks) {
		n := p.Networks[key]
		if n.External {
			continue
		}
		switch {
		case n.Driver != "" && n.Driver != "bridge":
			pr.refuse("network %s: driver %s is not supported: runwhp's networks are bridges", key, n.Driver)
		case len(n.Ipam.Config) > 0 || n.Ipam.Driver != "" && n.Ipam.Driver != "default":
			pr.refuse("network %s: ipam is not supported: runwhp chooses each network's subnet itself", key)
		case n.Internal:
			pr.refuse("network %s: internal networks are not supported", key)
		case n.EnableIPv6 != nil && *n.EnableIPv6:
			pr.refuse("network %s: IPv6 is not supported", key)
		}
		if len(n.DriverOpts) > 0 {
			pr.warn("network %s: driver_opts are ignored", key)
		}
	}
	for _, key := range sortedKeys(p.Volumes) {
		v := p.Volumes[key]
		if v.External {
			continue
		}
		if v.Driver != "" && v.Driver != "local" {
			pr.refuse("volume %s: driver %s is not supported: runwhp's volumes are disk images in its store", key, v.Driver)
		}
		if len(v.DriverOpts) > 0 {
			pr.refuse("volume %s: driver_opts are not supported", key)
		}
	}
	sharedVolumes(p, services, pr)
}

// sharedVolumes refuses a named volume that two of services, or two
// replicas of one, would mount at once: a runwhp volume is a disk only one
// running VM can have. A service that waits for another to complete runs
// after it, not with it.
func sharedVolumes(p *types.Project, services []string, pr *Problems) {
	users := map[string][]string{} // a volume's name in the store, and the services mounting it
	for _, name := range services {
		s, ok := p.Services[name]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, v := range s.Volumes {
			if v.Type != types.VolumeTypeVolume || v.Source == "" {
				continue
			}
			vol := v.Source
			if vc, ok := p.Volumes[v.Source]; ok && vc.Name != "" {
				vol = vc.Name
			}
			if seen[vol] {
				continue
			}
			seen[vol] = true
			users[vol] = append(users[vol], name)
			if s.GetScale() > 1 {
				pr.refuse("service %q mounts volume %s and has %d replicas: a runwhp volume is used by one running container at a time; share files through a bind mount", name, vol, s.GetScale())
			}
		}
	}
	for _, vol := range sortedKeys(users) {
		names := users[vol]
		for i, a := range names {
			for _, b := range names[i+1:] {
				if completesBefore(p, a, b) || completesBefore(p, b, a) {
					continue
				}
				pr.refuse("services %q and %q both mount volume %s: a runwhp volume is used by one running container at a time; share files through a bind mount", a, b, vol)
			}
		}
	}
}

// completesBefore reports whether service b waits for a to complete.
func completesBefore(p *types.Project, a, b string) bool {
	d, ok := p.Services[b].DependsOn[a]
	return ok && d.Condition == types.ServiceConditionCompletedSuccessfully
}

// ConfigFiles is the config_files label: the project's files, comma
// separated, as Docker Compose records them.
func ConfigFiles(p *types.Project) string { return strings.Join(p.ComposeFiles, ",") }

// ServicesOf are the services named, or all the project's, in name order.
func ServicesOf(p *types.Project, names []string) []string {
	if len(names) == 0 {
		return sortedKeys(p.Services)
	}
	return names
}
