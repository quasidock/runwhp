//go:build windows

package e2e

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type statsJSON struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	CPUPercent     float64 `json:"cpuPercent"`
	CPUs           int     `json:"cpus"`
	MemUsage       uint64  `json:"memUsage"`
	MemLimit       uint64  `json:"memLimit"`
	HostCommit     uint64  `json:"hostCommit"`
	HostWorkingSet uint64  `json:"hostWorkingSet"`
	NetRx          uint64  `json:"netRx"`
	NetTx          uint64  `json:"netTx"`
	BlockRead      uint64  `json:"blockRead"`
	PIDs           int     `json:"pids"`
	Error          string  `json:"error"`
}

// statsOf runs runwhp stats --no-stream --format json for the containers
// named, and returns a row for each.
func statsOf(t *testing.T, names ...string) map[string]statsJSON {
	t.Helper()
	r := cli(t, append([]string{"stats", "--no-stream", "--format", "json"}, names...)...)
	if r.code != 0 {
		t.Fatalf("stats: %+v", r)
	}
	rows := map[string]statsJSON{}
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	for dec.More() {
		var s statsJSON
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("stats output %q: %v", r.stdout, err)
		}
		rows[s.Name] = s
	}
	return rows
}

func TestStats(t *testing.T) {
	busy, idle := newName(t), newName(t)
	detached(t, "--name", busy, "--cpus", "2", alpine, "sh", "-c", "while :; do :; done & while :; do :; done")
	detached(t, "--name", idle, alpine, "sh", "-c", "sleep 300 & sleep 300 & sleep 300 & wait")
	time.Sleep(time.Second)

	rows := statsOf(t, busy, idle)
	b, i := rows[busy], rows[idle]
	if len(rows) != 2 || b.Error != "" || i.Error != "" {
		t.Fatalf("rows %+v", rows)
	}
	if b.CPUs != 2 || b.CPUPercent < 150 || b.CPUPercent > 210 {
		t.Errorf("two busy loops on 2 vCPUs: %.1f%% of %d", b.CPUPercent, b.CPUs)
	}
	if i.CPUPercent > 20 {
		t.Errorf("an idle container uses %.1f%%", i.CPUPercent)
	}
	// sh and its three sleeps.
	if i.PIDs != 4 {
		t.Errorf("PIDs %d, want 4", i.PIDs)
	}
	if i.MemLimit != defaultMemory() || i.MemUsage == 0 || i.MemUsage > i.MemLimit || i.HostCommit < 50<<20 || i.HostWorkingSet == 0 {
		t.Errorf("memory: %+v", i)
	}

	// Without names: every running container, these two among them.
	if all := statsOf(t); all[busy].ID != b.ID || all[idle].ID != i.ID {
		t.Errorf("stats without names: %+v", all)
	}

	// The table.
	r := cli(t, "stats", "--no-stream", busy)
	if r.code != 0 || !strings.HasPrefix(r.stdout, "CONTAINER ID   NAME") || !strings.Contains(r.stdout, busy) {
		t.Errorf("table: %+v", r)
	}
}

func TestStatsNetwork(t *testing.T) {
	parallel(t)
	port := hostServer(t, make([]byte, 20<<20))
	name := newName(t)
	detached(t, "--name", name, "--add-host", hostGateway, alpine, "sh", "-c",
		"wget -qO /dev/null http://host.docker.internal:"+port+"/ && sleep 300")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if s := statsOf(t, name)[name]; s.NetRx >= 20<<20 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("NET I/O never reached 20 MB: %+v", statsOf(t, name)[name])
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestStatsStopped(t *testing.T) {
	parallel(t)
	name := newName(t)
	if r := cli(t, keepArgs("--name", name, alpine, "true")...); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	if r := cli(t, "stats", "--no-stream", name); r.code != 1 || !strings.Contains(r.stderr, "not running") {
		t.Errorf("stats of a stopped container: %+v", r)
	}
}

// TestStatsForeground asks for the statistics of a container run in the
// foreground, whose runwhp is the run command itself.
func TestStatsForeground(t *testing.T) {
	parallel(t)
	name := newName(t)
	cmd := command(t, keepArgs("--name", name, alpine, "sleep", "30")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cli(t, "kill", name, "KILL")
		cmd.Wait()
	}()
	// The record appears once the run command has resolved the image.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var s stateJSON
		if r := cli(t, "state", name); r.code == 0 && json.Unmarshal([]byte(r.stdout), &s) == nil && s.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the foreground run never showed as running; stderr: %s", stderr.String())
		}
	}
	if s := statsOf(t, name)[name]; s.Error != "" || s.MemLimit == 0 {
		t.Errorf("stats of a foreground run: %+v", s)
	}
}
