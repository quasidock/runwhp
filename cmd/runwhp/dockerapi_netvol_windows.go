//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/build"
	dcontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dimage "github.com/moby/moby/api/types/image"
	dnetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/system"
	dvolume "github.com/moby/moby/api/types/volume"

	"github.com/quasidock/runwhp/internal/container"
	"github.com/quasidock/runwhp/internal/dockerapi"
	"github.com/quasidock/runwhp/internal/gc"
	"github.com/quasidock/runwhp/internal/network"
	"github.com/quasidock/runwhp/internal/volume"
)

func (s *apiServer) networkRoutes() {
	s.handle("GET /networks", s.getNetworks)
	s.handle("POST /networks/create", s.postNetworkCreate)
	s.handle("POST /networks/prune", s.postNetworksPrune)
	s.handle("GET /networks/{id}", s.getNetwork)
	s.handle("DELETE /networks/{id}", s.deleteNetwork)
	s.handle("POST /networks/{id}/connect", s.postNetworkConnect)
	s.handle("POST /networks/{id}/disconnect", s.postNetworkDisconnect)
}

func (s *apiServer) volumeRoutes() {
	s.handle("GET /volumes", s.getVolumes)
	s.handle("POST /volumes/create", s.postVolumeCreate)
	s.handle("POST /volumes/prune", s.postVolumesPrune)
	s.handle("GET /volumes/{name}", s.getVolume)
	s.handle("DELETE /volumes/{name}", s.deleteVolume)
	s.handle("PUT /volumes/{name}", func(http.ResponseWriter, *http.Request) error {
		return badRequest("runwhp's volumes cannot be updated: only cluster volumes can")
	})
}

// filtersFrom checks a request's filters against the keys valid, as the
// CLI's --filter would be.
func filtersFrom(raw string, valid ...string) (filters, error) {
	m, err := dockerapi.DecodeFilters(raw)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if err := dockerapi.CheckFilterKeys(m, valid...); err != nil {
		return nil, badRequest("%v", err)
	}
	return filters(m), nil
}

// dockerNetwork is a network as Docker's API shows it: runwhp's default
// network is Docker's bridge.
func (s *apiServer) dockerNetwork(store *network.Store, n *network.Network, withContainers bool) networkJSON {
	j := inspectNetwork(s.root, store, n)
	if n.Name == network.User {
		j.Name, j.Driver = "bridge", "bridge"
	}
	if !withContainers {
		j.Containers = map[string]networkEndpoint{}
	}
	return j
}

func (s *apiServer) resolveNetwork(store *network.Store, ref string) (*network.Network, error) {
	n, err := store.Resolve(ref)
	if errors.Is(err, network.ErrNotFound) {
		return nil, notFound("network %s not found", ref)
	}
	return n, err
}

func (s *apiServer) getNetworks(w http.ResponseWriter, r *http.Request) error {
	f, err := filtersFrom(r.URL.Query().Get("filters"), "name", "id", "label", "driver", "type", "scope", "dangling")
	if err != nil {
		return err
	}
	store := network.Open(s.root)
	nets, err := store.List()
	if err != nil {
		return err
	}
	out := []networkJSON{}
	for _, n := range nets {
		shown := *n
		if n.Name == network.User {
			shown.Name, shown.Driver = "bridge", "bridge"
		}
		if !f.matchNetwork(&shown) && !f.matchNetwork(n) {
			continue
		}
		if sc := f["scope"]; len(sc) > 0 && !containsString(sc, "local") {
			continue
		}
		out = append(out, s.dockerNetwork(store, n, false))
	}
	writeJSON(w, out)
	return nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (s *apiServer) getNetwork(w http.ResponseWriter, r *http.Request) error {
	store := network.Open(s.root)
	n, err := s.resolveNetwork(store, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, s.dockerNetwork(store, n, true))
	return nil
}

func (s *apiServer) postNetworkCreate(w http.ResponseWriter, r *http.Request) error {
	var req dnetwork.CreateRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Name == "" {
		return badRequest("network name is required")
	}
	var refused []string
	if req.Driver != "" && req.Driver != "bridge" {
		refused = append(refused, "--driver "+req.Driver)
	}
	if req.IPAM != nil && (len(req.IPAM.Config) > 0 || req.IPAM.Driver != "" && req.IPAM.Driver != "default") {
		refused = append(refused, "--subnet, --gateway and --ip-range")
	}
	if req.Internal {
		refused = append(refused, "--internal")
	}
	if req.EnableIPv6 != nil && *req.EnableIPv6 {
		refused = append(refused, "--ipv6")
	}
	if req.ConfigOnly || req.ConfigFrom != nil {
		refused = append(refused, "--config-only and --config-from")
	}
	if req.Scope != "" && req.Scope != "local" {
		refused = append(refused, "--scope "+req.Scope)
	}
	if len(req.Options) > 0 {
		refused = append(refused, "-o")
	}
	if len(refused) > 0 {
		return badRequest("runwhp cannot make this network as asked: %s %s not supported; runwhp chooses each network's subnet itself",
			strings.Join(refused, ", "), pluralIs(len(refused)))
	}
	store := network.Open(s.root)
	n, err := store.CreateWith(req.Name, req.Labels)
	switch {
	case errors.Is(err, network.ErrExists):
		return conflict("network with name %s already exists", req.Name)
	case errors.Is(err, network.ErrPredefined):
		return &apiError{http.StatusForbidden, "operation is not permitted on predefined " + req.Name + " network"}
	case err != nil:
		return err
	}
	s.emit(events.NetworkEventType, events.ActionCreate, n.ID, map[string]string{"name": n.Name, "type": "bridge"})
	writeJSONStatus(w, http.StatusCreated, dnetwork.CreateResponse{ID: n.ID})
	return nil
}

func (s *apiServer) deleteNetwork(w http.ResponseWriter, r *http.Request) error {
	store := network.Open(s.root)
	n, err := s.resolveNetwork(store, r.PathValue("id"))
	if err != nil {
		return err
	}
	if n.IsBuiltin() {
		return &apiError{http.StatusForbidden, n.Name + " is a pre-defined network and cannot be removed"}
	}
	if _, err := store.Remove(n.ID); err != nil {
		if errors.Is(err, network.ErrActive) {
			return &apiError{http.StatusForbidden, "error while removing network: network " + n.Name + " id " + n.ID + " has active endpoints"}
		}
		return err
	}
	s.emit(events.NetworkEventType, events.ActionDestroy, n.ID, map[string]string{"name": n.Name, "type": "bridge"})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postNetworksPrune(w http.ResponseWriter, r *http.Request) error {
	f, err := filtersFrom(r.URL.Query().Get("filters"), "label", "label!", "until")
	if err != nil {
		return err
	}
	if len(f) > 0 {
		return badRequest("runwhp's network prune takes no filters yet")
	}
	removed, err := pruneNetworks(network.Open(s.root))
	if err != nil {
		s.logf("network prune: %v", err)
	}
	if removed == nil {
		removed = []string{}
	}
	writeJSON(w, dnetwork.PruneReport{NetworksDeleted: removed})
	return nil
}

func (s *apiServer) connectArgs(r *http.Request, req any) (*network.Store, *network.Network, *container.Container, error) {
	if err := readJSON(r, req); err != nil {
		return nil, nil, nil, err
	}
	store := network.Open(s.root)
	n, err := s.resolveNetwork(store, r.PathValue("id"))
	if err != nil {
		return nil, nil, nil, err
	}
	var ref string
	switch x := req.(type) {
	case *dnetwork.ConnectRequest:
		ref = x.Container
	case *dnetwork.DisconnectRequest:
		ref = x.Container
	}
	c, err := s.resolve(ref)
	if err != nil {
		return nil, nil, nil, err
	}
	return store, n, c, nil
}

// networkError gives a refusal to connect or disconnect its status.
func networkError(err error) error {
	if errors.Is(err, container.ErrNetworksRunning) {
		return conflict("%v", err)
	}
	if httpStatus(err) == http.StatusInternalServerError {
		return &apiError{http.StatusForbidden, err.Error()}
	}
	return err
}

func (s *apiServer) postNetworkConnect(w http.ResponseWriter, r *http.Request) error {
	var req dnetwork.ConnectRequest
	store, n, c, err := s.connectArgs(r, &req)
	if err != nil {
		return err
	}
	var aliases []string
	ip := ""
	if e := req.EndpointConfig; e != nil {
		aliases = e.Aliases
		if e.IPAMConfig != nil && e.IPAMConfig.IPv4Address.IsValid() {
			ip = e.IPAMConfig.IPv4Address.String()
		}
		if e.IPAMConfig != nil && e.IPAMConfig.IPv6Address.IsValid() {
			return badRequest("--ip6 is not supported: the network is IPv4 only")
		}
	}
	if err := networkConnect(s.root, store, n.ID, c.ID, aliases, ip); err != nil {
		return networkError(err)
	}
	s.emit(events.NetworkEventType, events.ActionConnect, n.ID, map[string]string{"name": n.Name, "container": c.ID, "type": "bridge"})
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *apiServer) postNetworkDisconnect(w http.ResponseWriter, r *http.Request) error {
	var req dnetwork.DisconnectRequest
	store, n, c, err := s.connectArgs(r, &req)
	if err != nil {
		return err
	}
	if err := networkDisconnect(s.root, store, n.ID, c.ID); err != nil {
		return networkError(err)
	}
	s.emit(events.NetworkEventType, events.ActionDisconnect, n.ID, map[string]string{"name": n.Name, "container": c.ID, "type": "bridge"})
	w.WriteHeader(http.StatusOK)
	return nil
}

// dockerVolume is a volume as Docker's API shows it.
func dockerVolume(v *volume.Volume) dvolume.Volume {
	dv := dvolume.Volume{
		Name: v.Name, Driver: "local", Mountpoint: v.Disk(), Scope: "local",
		CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339), Labels: withLabels(map[string]string{}, v.Labels),
		Options: map[string]string{"size": humanSize(v.Size)},
	}
	return dv
}

func (s *apiServer) getVolumes(w http.ResponseWriter, r *http.Request) error {
	f, err := filtersFrom(r.URL.Query().Get("filters"), "name", "label", "driver", "dangling")
	if err != nil {
		return err
	}
	vs, err := volume.Open(s.root).List()
	if err != nil {
		return err
	}
	resp := dvolume.ListResponse{Volumes: []dvolume.Volume{}, Warnings: []string{}}
	for _, v := range vs {
		used := volumeUsers(s.root, v.Name) != ""
		if !f.matchVolume(v, used) {
			continue
		}
		resp.Volumes = append(resp.Volumes, dockerVolume(v))
	}
	writeJSON(w, resp)
	return nil
}

func (s *apiServer) getVolume(w http.ResponseWriter, r *http.Request) error {
	v, err := volume.Open(s.root).Get(r.PathValue("name"))
	if errors.Is(err, volume.ErrNotFound) {
		return notFound("get %s: no such volume", r.PathValue("name"))
	}
	if err != nil {
		return err
	}
	writeJSON(w, dockerVolume(v))
	return nil
}

func (s *apiServer) postVolumeCreate(w http.ResponseWriter, r *http.Request) error {
	var req dvolume.CreateRequest
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Driver != "" && req.Driver != "local" {
		return badRequest("volume driver %s is not supported: runwhp's volumes are local", req.Driver)
	}
	if req.ClusterVolumeSpec != nil {
		return badRequest("cluster volumes are not supported")
	}
	size := int64(defaultDiskSize)
	for k, v := range req.DriverOpts {
		if k != "size" {
			return badRequest("volume option %s is not supported; runwhp knows size", k)
		}
		var err error
		if size, err = parseDiskSize(v); err != nil {
			return badRequest("%v", err)
		}
	}
	name := req.Name
	if name == "" {
		var b [32]byte
		rand.Read(b[:])
		name = hex.EncodeToString(b[:])
	}
	if err := volume.CheckName(name); err != nil {
		return badRequest("%v", err)
	}
	store := volume.Open(s.root)
	v, created, err := store.EnsureWith(name, size, req.Labels)
	if err != nil {
		return err
	}
	if created {
		s.emit(events.VolumeEventType, events.ActionCreate, v.Name, map[string]string{"driver": "local"})
	}
	writeJSONStatus(w, http.StatusCreated, dockerVolume(v))
	return nil
}

func (s *apiServer) deleteVolume(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	store := volume.Open(s.root)
	if _, err := store.Get(name); errors.Is(err, volume.ErrNotFound) {
		return notFound("get %s: no such volume", name)
	}
	if users := volumeUsers(s.root, name); users != "" {
		return conflict("remove %s: volume is in use - [%s]", name, users)
	}
	if err := store.Remove(name); err != nil {
		return err
	}
	s.emit(events.VolumeEventType, events.ActionDestroy, name, map[string]string{"driver": "local"})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *apiServer) postVolumesPrune(w http.ResponseWriter, r *http.Request) error {
	f, err := filtersFrom(r.URL.Query().Get("filters"), "label", "label!", "all")
	if err != nil {
		return err
	}
	resp := dvolume.PruneReport{VolumesDeleted: []string{}}
	// Docker removes only anonymous volumes unless all is given; runwhp's
	// anonymous volumes live in their containers' writable layers.
	if a := f["all"]; len(a) == 0 || (a[0] != "true" && a[0] != "1") {
		writeJSON(w, resp)
		return nil
	}
	release, err := gc.Exclusive(s.root, func() {})
	if err != nil {
		return err
	}
	defer release()
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	p := u.UnusedVolumes()
	var keep []*gc.Volume
	for _, v := range p.Volumes {
		if (filters{"label": f["label"]}).matchLabels(v.Labels) && notLabels(f["label!"], v.Labels) {
			keep = append(keep, v)
		}
	}
	p.Volumes = keep
	rep := u.Apply(p)
	resp.VolumesDeleted = append(resp.VolumesDeleted, rep.Volumes...)
	resp.SpaceReclaimed = uint64(max(rep.Freed, 0))
	for _, v := range rep.Volumes {
		s.emit(events.VolumeEventType, events.ActionDestroy, v, map[string]string{"driver": "local"})
	}
	writeJSON(w, resp)
	return nil
}

// getSystemDF answers system df: what images, containers, volumes and the
// build cache take, as runwhp's system df works it out.
func (s *apiServer) getSystemDF(w http.ResponseWriter, r *http.Request) error {
	u, err := gc.Load(s.root)
	if err != nil {
		return err
	}
	rows := summarize(u)
	row := func(typ string) dfRow {
		for _, r := range rows {
			if r.Type == typ {
				return r
			}
		}
		return dfRow{}
	}
	want := map[string]bool{}
	for _, t := range r.URL.Query()["type"] {
		want[t] = true
	}
	all := len(want) == 0
	var resp system.DiskUsage
	if all || want["image"] {
		ir := row("Images")
		du := &dimage.DiskUsage{TotalCount: int64(ir.Total), ActiveCount: int64(ir.Active), TotalSize: ir.Size, Reclaimable: ir.Reclaimable, Items: []dimage.Summary{}}
		for _, g := range imageGroups(u) {
			du.Items = append(du.Items, g.summary())
		}
		resp.ImageUsage = du
	}
	if all || want["container"] {
		cr := row("Containers")
		du := &dcontainer.DiskUsage{TotalCount: int64(cr.Total), ActiveCount: int64(cr.Active), TotalSize: cr.Size, Reclaimable: cr.Reclaimable, Items: []dcontainer.Summary{}}
		for _, c := range u.Containers {
			sum := s.summaryOf(c.Container, false)
			sum.SizeRw = c.Size
			du.Items = append(du.Items, sum)
		}
		resp.ContainerUsage = du
	}
	if all || want["volume"] {
		vr := row("Local Volumes")
		du := &dvolume.DiskUsage{TotalCount: int64(vr.Total), ActiveCount: int64(vr.Active), TotalSize: vr.Size, Reclaimable: vr.Reclaimable, Items: []dvolume.Volume{}}
		for _, v := range u.Volumes {
			dv := dockerVolume(v.Volume)
			dv.UsageData = &dvolume.UsageData{RefCount: int64(len(v.Users)), Size: v.Size}
			du.Items = append(du.Items, dv)
		}
		resp.VolumeUsage = du
	}
	if all || want["build-cache"] {
		br := row("Build Cache")
		du := &build.DiskUsage{TotalCount: int64(br.Total), ActiveCount: int64(br.Active), TotalSize: br.Size, Reclaimable: br.Reclaimable, Items: []build.CacheRecord{}}
		for _, e := range u.Cache {
			rec := build.CacheRecord{ID: e.Key, Type: "regular", Description: e.CreatedBy, InUse: !e.Dangling(), CreatedAt: e.Created}
			if e.Layer != nil {
				rec.Size = e.Layer.Size
			}
			du.Items = append(du.Items, rec)
		}
		sort.Slice(du.Items, func(i, j int) bool { return du.Items[i].ID < du.Items[j].ID })
		resp.BuildCacheUsage = du
	}
	writeJSON(w, resp)
	return nil
}
