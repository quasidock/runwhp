package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
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
// networks, volumes, secrets and configs. services are those up would
// run.
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
		for opt, val := range v.DriverOpts {
			switch {
			case opt != "shared":
				pr.refuse("volume %s: driver_opts %s is not supported; runwhp knows shared", key, opt)
			case !validBool(val):
				pr.refuse("volume %s: driver_opts shared=%q: give true or false", key, val)
			}
		}
	}
}

func validBool(s string) bool {
	_, err := strconv.ParseBool(s)
	return err == nil
}

// SharedVolumes returns the names, in the store, of the project's volumes
// that containers would use at once, which are to be shared: those two
// services mount, or a service with replicas, scale says how many, or
// whose driver_opts say shared. A service that waits for another to
// complete runs after it, not with it. services are those to consider,
// which the project's whole, rather than those up runs, so that a volume
// is shared before its first user starts, not after.
func SharedVolumes(p *types.Project, services []string, scale func(service string) int) []string {
	shared := map[string]bool{}
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
			if vc, ok := p.Volumes[v.Source]; ok {
				if vc.Name != "" {
					vol = vc.Name
				}
				if on, _ := strconv.ParseBool(vc.DriverOpts["shared"]); on {
					shared[vol] = true
				}
			}
			if seen[vol] {
				continue
			}
			seen[vol] = true
			users[vol] = append(users[vol], name)
			if scale(name) > 1 {
				shared[vol] = true
			}
		}
	}
	for vol, names := range users {
		for i, a := range names {
			for _, b := range names[i+1:] {
				if !completesBefore(p, a, b) && !completesBefore(p, b, a) {
					shared[vol] = true
				}
			}
		}
	}
	return sortedKeys(shared)
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
