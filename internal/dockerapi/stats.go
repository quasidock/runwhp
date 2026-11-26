package dockerapi

import (
	"time"

	dcontainer "github.com/moby/moby/api/types/container"

	"github.com/quasidock/runwhp/internal/container"
)

// Stats is what a container uses, cur, as Docker's stats endpoint gives
// it, with prev, the sample before, if any, as its previous figures.
//
// runwhp knows how long the container's vCPUs ran, not the host's time;
// system_cpu_usage is the time since the container's runwhp started its
// clock times its vCPUs, so that the docker CLI's percentage, its CPU
// time over the system's, times the CPUs, comes out as the share of one
// vCPU the container used, as runwhp's stats shows it.
func Stats(id, name string, cur, prev *container.Stats) dcontainer.StatsResponse {
	resp := dcontainer.StatsResponse{ID: id, Name: name, OSType: "linux", Read: cur.Time}
	resp.CPUStats = cpuStats(cur)
	if prev != nil {
		resp.PreRead = prev.Time
		resp.PreCPUStats = cpuStats(prev)
	}
	mem := dcontainer.MemoryStats{Limit: cur.MemLimit, Commit: cur.HostCommit, PrivateWorkingSet: cur.HostWorkingSet}
	if g := cur.Guest; g != nil {
		mem.Usage = g.MemTotal - min(g.MemAvailable, g.MemTotal)
		mem.Stats = map[string]uint64{"file": g.Cached, "inactive_file": 0}
		resp.PidsStats = dcontainer.PidsStats{Current: uint64(g.Tasks)}
		resp.NumProcs = 0
	}
	resp.MemoryStats = mem
	resp.Networks = map[string]dcontainer.NetworkStats{"eth0": {RxBytes: cur.NetRx, TxBytes: cur.NetTx}}
	resp.BlkioStats = dcontainer.BlkioStats{IoServiceBytesRecursive: []dcontainer.BlkioStatEntry{
		{Major: 254, Minor: 0, Op: "read", Value: cur.BlockRead},
		{Major: 254, Minor: 0, Op: "write", Value: cur.BlockWrite},
	}}
	return resp
}

// clockOrigin is the time system_cpu_usage counts from; any fixed time
// does, as clients look at its differences only.
var clockOrigin = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func cpuStats(s *container.Stats) dcontainer.CPUStats {
	cpus := uint32(max(s.CPUs, 1))
	wall := uint64(max(s.Time.Sub(clockOrigin), 0))
	return dcontainer.CPUStats{
		CPUUsage:    dcontainer.CPUUsage{TotalUsage: uint64(max(s.CPUTime, 0))},
		SystemUsage: wall * uint64(cpus),
		OnlineCPUs:  cpus,
	}
}

// CPUPercent is the docker CLI's CPU percentage of a stats response, for
// tests: its formula, as it computes it for Linux containers.
func CPUPercent(s dcontainer.StatsResponse) float64 {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	online := float64(s.CPUStats.OnlineCPUs)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0
	}
	return cpuDelta / sysDelta * online * 100
}
