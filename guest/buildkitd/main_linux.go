//go:build linux

// runwhp-buildkitd is BuildKit's daemon as runwhp runs it, in a builder
// container the Engine API starts: BuildKit's OCI worker, with runc from
// the moby/buildkit image, overlayfs on a volume, and the guest's own
// network, serving the docker CLI's /grpc and /session over vsock, with
// what Docker's own embedded BuildKit adds: a "moby" exporter, which
// loads images into runwhp's store, and the store's images as FROM's.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	ctdsnapshot "github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins/snapshots/overlay"
	"github.com/moby/buildkit/cache/remotecache"
	inlineremotecache "github.com/moby/buildkit/cache/remotecache/inline"
	localremotecache "github.com/moby/buildkit/cache/remotecache/local"
	registryremotecache "github.com/moby/buildkit/cache/remotecache/registry"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	"github.com/moby/buildkit/control"
	"github.com/moby/buildkit/executor/oci"
	"github.com/moby/buildkit/frontend"
	dockerfile "github.com/moby/buildkit/frontend/dockerfile/builder"
	"github.com/moby/buildkit/frontend/gateway"
	"github.com/moby/buildkit/frontend/gateway/forwarder"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/db/boltutil"
	"github.com/moby/buildkit/util/disk"
	_ "github.com/moby/buildkit/util/grpcutil/encoding/proto"
	"github.com/moby/buildkit/util/network/cniprovider"
	"github.com/moby/buildkit/util/network/netproviders"
	"github.com/moby/buildkit/util/resolver"
	"github.com/moby/buildkit/version"
	"github.com/moby/buildkit/worker"
	"github.com/moby/buildkit/worker/base"
	"github.com/moby/buildkit/worker/runc"
	copy "github.com/tonistiigi/fsutil/copy"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/quasidock/runwhp/internal/guestspec"
)

type options struct {
	root       string
	socket     string
	dockerSock string
	gateway    string
	// GC caps, in bytes: 0 leaves BuildKit's default for the disk.
	maxUsed, reserved, minFree int64
	debug                      bool
}

func main() {
	var o options
	flag.StringVar(&o.root, "root", "/var/lib/buildkit", "BuildKit's state")
	flag.StringVar(&o.socket, "socket", "/run/buildkit/buildkitd.sock", "a Unix socket to serve too, for buildctl")
	flag.StringVar(&o.dockerSock, "docker-socket", "/var/run/docker.sock", "runwhp's Engine API")
	flag.StringVar(&o.gateway, "host-gateway", "", "the address host-gateway means in --add-host")
	flag.Int64Var(&o.maxUsed, "gc-max-used", 0, "bytes the cache may use at most")
	flag.Int64Var(&o.reserved, "gc-reserved", 0, "bytes the cache keeps however full the disk")
	flag.Int64Var(&o.minFree, "gc-min-free", 0, "bytes to keep free")
	flag.BoolVar(&o.debug, "debug", false, "log at debug level")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "runwhp-buildkitd:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	syscall.Umask(0)
	copy.UmaskIsZero = true
	apicaps.ExportedProduct = "buildkit"
	setVersion()
	// The resource monitor moves this process into a cgroup of its own and
	// enables the controllers below the container's, so that runc's
	// cgroups for steps can be made there: the container's cgroup takes
	// no processes once it delegates.
	os.Setenv("BUILDKIT_SETUP_CGROUPV2_ROOT", "1")
	if o.debug {
		bklog.L.Logger.SetLevel(5) // logrus.DebugLevel
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := os.MkdirAll(o.root, 0o700); err != nil {
		return err
	}
	api := newDockerAPI(o.dockerSock)
	sm, err := session.NewManager()
	if err != nil {
		return err
	}
	controller, err := newController(ctx, o, api, sm)
	if err != nil {
		return err
	}
	defer controller.Close()

	server := grpc.NewServer(grpc.MaxRecvMsgSize(16<<20), grpc.MaxSendMsgSize(16<<20))
	healthv1.RegisterHealthServer(server, health.NewServer())
	controller.Register(server)

	vl, err := listenVsock(guestspec.BuildKitPort)
	if err != nil {
		return fmt.Errorf("listening on vsock port %d: %w", guestspec.BuildKitPort, err)
	}
	grpcConns := newConnListener(vl.Addr())
	go server.Serve(grpcConns)
	go serveHellos(ctx, vl, grpcConns, sm)

	if o.socket != "" {
		os.MkdirAll(filepath.Dir(o.socket), 0o755)
		os.Remove(o.socket)
		ul, err := net.Listen("unix", o.socket)
		if err != nil {
			return err
		}
		go server.Serve(ul)
	}
	bklog.L.Infof("runwhp-buildkitd, BuildKit %s, ready on vsock port %d", version.Version, guestspec.BuildKitPort)

	<-ctx.Done()
	vl.Close()
	server.GracefulStop()
	return nil
}

func newController(ctx context.Context, o options, api *dockerAPI, sm *session.Manager) (*control.Controller, error) {
	hosts := resolver.NewRegistryConfig(nil)

	snapshotter := runc.SnapshotterFactory{Name: "overlayfs", New: func(root string) (ctdsnapshot.Snapshotter, error) {
		return overlay.NewSnapshotter(root, overlay.AsynchronousRemove)
	}}
	labels := map[string]string{}
	if o.gateway != "" {
		// What Docker's embedded BuildKit labels its worker with, for
		// buildx to expand --add-host NAME:host-gateway.
		labels["org.mobyproject.buildkit.worker.moby.host-gateway-ip"] = o.gateway
	}
	nopt := netproviders.Opt{Mode: "host", CNI: cniprovider.Opt{Root: o.root}}
	wopt, err := runc.NewWorkerOpt(o.root, snapshotter, false, oci.ProcessSandbox, labels, nil, nopt, nil, "", "", false, nil, "", "", nil)
	if err != nil {
		return nil, err
	}
	wopt.GCPolicy = gcPolicy(o)
	wopt.BuildkitVersion = client.BuildkitVersion{Package: version.Package, Version: version.Version, Revision: version.Revision}
	wopt.RegistryHosts = hosts
	local := newLocalStore(api, wopt.ContentStore, wopt.LeaseManager)
	wopt.ImageStore = local
	bw, err := base.NewWorker(ctx, wopt)
	if err != nil {
		return nil, err
	}
	w := &runwhpWorker{Worker: bw, api: api}
	wc := &worker.Controller{}
	if err := wc.Add(w); err != nil {
		return nil, err
	}

	frontends := map[string]frontend.Frontend{
		"dockerfile.v0": forwarder.NewGatewayForwarder(wc.Infos(), dockerfile.Build),
	}
	gw, err := gateway.NewGatewayFrontend(wc.Infos(), nil)
	if err != nil {
		return nil, err
	}
	frontends["gateway.v0"] = gw

	cacheStorage, err := bboltcachestorage.NewStore(filepath.Join(o.root, "cache.db"))
	if err != nil {
		return nil, err
	}
	historyDB, err := boltutil.SafeOpen(filepath.Join(o.root, "history.db"), 0o600, &bolt.Options{
		FreelistType:   bolt.FreelistMapType,
		NoFreelistSync: true,
	})
	if err != nil {
		return nil, err
	}
	return control.NewController(control.Opt{
		SessionManager:   sm,
		WorkerController: wc,
		Frontends:        frontends,
		ResolveCacheExporterFuncs: map[string]remotecache.ResolveCacheExporterFunc{
			"registry": registryremotecache.ResolveCacheExporterFunc(sm, hosts),
			"local":    localremotecache.ResolveCacheExporterFunc(sm),
			"inline":   inlineremotecache.ResolveCacheExporterFunc(),
		},
		ResolveCacheImporterFuncs: map[string]remotecache.ResolveCacheImporterFunc{
			"registry": registryremotecache.ResolveCacheImporterFunc(sm, w.ContentStore(), hosts),
			"local":    localremotecache.ResolveCacheImporterFunc(sm),
		},
		CacheManager: solver.NewCacheManager(ctx, "local", cacheStorage, worker.NewCacheResultStorage(wc)),
		// As dockerd allows by default.
		Entitlements:   []string{"network.host"},
		HistoryDB:      historyDB,
		CacheStore:     cacheStorage,
		LeaseManager:   w.LeaseManager(),
		ContentStore:   w.ContentStore(),
		GarbageCollect: w.GarbageCollect,
		GracefulStop:   ctx.Done(),
	})
}

// setVersion gives BuildKit's version, which its own builds set with the
// linker, as this one links it: buildx shows it, and checks features by it.
func setVersion() {
	if version.Version != "v0.0.0+unknown" && version.Version != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, d := range info.Deps {
		if d.Path == "github.com/moby/buildkit" {
			version.Version = d.Version
			version.Revision = "runwhp"
			return
		}
	}
}

// gcPolicy is BuildKit's default policy, with the caps the API server
// gives from the host's disk: the volume's own size, sparse on the host,
// says nothing of the room there is.
func gcPolicy(o options) []client.PruneInfo {
	dstat, _ := disk.GetDiskStat(o.root)
	cfg := config.GCConfig{
		GCMaxUsedSpace:  config.DiskSpace{Bytes: o.maxUsed},
		GCReservedSpace: config.DiskSpace{Bytes: o.reserved},
		GCMinFreeSpace:  config.DiskSpace{Bytes: o.minFree},
	}
	var out []client.PruneInfo
	for _, rule := range config.DefaultGCPolicy(cfg, dstat) {
		out = append(out, client.PruneInfo{
			Filter:        rule.Filters,
			All:           rule.All,
			KeepDuration:  rule.KeepDuration.Duration,
			ReservedSpace: rule.ReservedSpace.AsBytes(dstat),
			MaxUsedSpace:  rule.MaxUsedSpace.AsBytes(dstat),
			MinFreeSpace:  rule.MinFreeSpace.AsBytes(dstat),
		})
	}
	return out
}
