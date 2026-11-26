// Package clients runs Docker's own Go clients against runwhp's Engine
// API: testcontainers-go and Docker's Go SDK. The e2e tests in test/e2e
// run them with RUNWHP_E2E_CLIENTS=1 and DOCKER_HOST pointing at a
// runwhp serving a store of their own, with the images they use pulled.
package clients

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	dnetwork "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	alpine = "alpine:3.20"
	nginx  = "nginx:alpine"
	redisI = "redis:7-alpine"
)

func need(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("RUNWHP_E2E_CLIENTS") != "1" {
		t.Skip("run by test/e2e with RUNWHP_E2E_CLIENTS=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestTestcontainersHTTP runs nginx as testcontainers' own tests do: a
// random host port for its exposed port, waited for over HTTP.
func TestTestcontainersHTTP(t *testing.T) {
	ctx := need(t)
	ctr, err := testcontainers.Run(ctx, nginx,
		testcontainers.WithExposedPorts("80/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/").WithPort("80/tcp").WithStartupTimeout(time.Minute)))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "80/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "nginx") {
		t.Errorf("GET %s: %s %q", endpoint, resp.Status, body)
	}
}

// TestTestcontainersRedis runs testcontainers' redis module, which waits
// for a log line, and speaks to redis through its mapped port.
func TestTestcontainersRedis(t *testing.T) {
	ctx := need(t)
	ctr, err := redis.Run(ctx, redisI)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(uri, "redis://"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	fmt.Fprint(conn, "SET greeting runwhp\r\nGET greeting\r\n")
	var lines []string
	for range 3 {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("after %q: %v", lines, err)
		}
		lines = append(lines, strings.TrimSpace(l))
	}
	if strings.Join(lines, ",") != "+OK,$6,runwhp" {
		t.Errorf("redis answered %q", lines)
	}
}

// TestTestcontainersFilesExecNetwork copies a file into a created
// container, waits for a port with testcontainers' exec probe, and has a
// second container reach the first by its alias on a network of the
// test's own.
func TestTestcontainersFilesExecNetwork(t *testing.T) {
	ctx := need(t)
	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupNetwork(t, nw)
	web, err := testcontainers.Run(ctx, alpine,
		testcontainers.WithFiles(testcontainers.ContainerFile{Reader: strings.NewReader("from the host\n"), ContainerFilePath: "/srv/hello.txt", FileMode: 0o644}),
		testcontainers.WithCmd("nc", "-lk", "-p", "8080", "-e", "cat", "/srv/hello.txt"),
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("8080/tcp").WithStartupTimeout(time.Minute)),
		network.WithNetwork([]string{"web"}, nw))
	testcontainers.CleanupContainer(t, web)
	if err != nil {
		t.Fatal(err)
	}
	code, out, err := web.Exec(ctx, []string{"cat", "/srv/hello.txt"}, tcexec.Multiplexed())
	if err != nil || code != 0 {
		t.Fatalf("exec: %d, %v", code, err)
	}
	if b, _ := io.ReadAll(out); string(b) != "from the host\n" {
		t.Errorf("the file copied in: %q", b)
	}
	client, err := testcontainers.Run(ctx, alpine,
		testcontainers.WithCmd("sleep", "600"),
		network.WithNetwork(nil, nw))
	testcontainers.CleanupContainer(t, client)
	if err != nil {
		t.Fatal(err)
	}
	code, out, err = client.Exec(ctx, []string{"nc", "web", "8080"}, tcexec.Multiplexed())
	b, _ := io.ReadAll(out)
	if err != nil || code != 0 || string(b) != "from the host\n" {
		t.Errorf("nc web 8080: %d, %v, %q", code, err, b)
	}
}

// TestGoSDK drives the API as Docker's Go SDK does: negotiation, a random
// host port, logs, an exec with input, top and events.
func TestGoSDK(t *testing.T) {
	ctx := need(t)
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	ping, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	if err != nil || ping.OSType != "linux" || ping.APIVersion == "" {
		t.Fatalf("ping: %+v, %v", ping, err)
	}
	since := fmt.Sprint(time.Now().Add(-time.Second).Unix())
	port, _ := dnetwork.ParsePort("8080/tcp")
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: alpine, Labels: map[string]string{"sdk": "yes"},
			Cmd:          []string{"sh", "-c", "echo hello from the sdk; echo oops >&2; exec nc -lk -p 8080 -e cat /etc/hostname"},
			ExposedPorts: dnetwork.PortSet{port: {}}},
		HostConfig: &container.HostConfig{PortBindings: dnetwork.PortMap{port: {{}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	insp, err := cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bindings := insp.Container.NetworkSettings.Ports[port]
	if len(bindings) == 0 || bindings[0].HostPort == "" || bindings[0].HostPort == "0" {
		t.Fatalf("ports %+v", insp.Container.NetworkSettings.Ports)
	}
	addr := net.JoinHostPort("127.0.0.1", bindings[0].HostPort)
	if got := tcpBody(addr); got == "" {
		t.Errorf("nothing served at %s", addr)
	}

	logs, err := cli.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, logs); err != nil {
		t.Fatal(err)
	}
	logs.Close()
	if stdout.String() != "hello from the sdk\n" || stderr.String() != "oops\n" {
		t.Errorf("logs %q, %q", stdout.String(), stderr.String())
	}

	ex, err := cli.ExecCreate(ctx, created.ID, client.ExecCreateOptions{Cmd: []string{"sh", "-c", "tr a-z A-Z; exit 3"},
		AttachStdin: true, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	att, err := cli.ExecAttach(ctx, ex.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	att.Conn.Write([]byte("shout\n"))
	att.CloseWrite()
	stdout.Reset()
	stdcopy.StdCopy(&stdout, io.Discard, att.Reader)
	att.Close()
	if stdout.String() != "SHOUT\n" {
		t.Errorf("exec output %q", stdout.String())
	}
	var exitCode int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		ei, err := cli.ExecInspect(ctx, ex.ID, client.ExecInspectOptions{})
		if err == nil && !ei.Running {
			exitCode = ei.ExitCode
			break
		}
	}
	if exitCode != 3 {
		t.Errorf("exec exit code %d", exitCode)
	}

	top, err := cli.ContainerTop(ctx, created.ID, client.ContainerTopOptions{})
	if err != nil || len(top.Titles) == 0 || !strings.Contains(fmt.Sprint(top.Processes), "nc -lk") {
		t.Errorf("top: %+v, %v", top, err)
	}

	cli.ContainerStop(ctx, created.ID, client.ContainerStopOptions{})
	evctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	evs := cli.Events(evctx, client.EventsListOptions{Since: since, Until: fmt.Sprint(time.Now().Add(time.Second).Unix()),
		Filters: make(client.Filters).Add("label", "sdk=yes").Add("type", "container")})
	var actions []string
	for done := false; !done; {
		select {
		case m, ok := <-evs.Messages:
			if !ok {
				done = true
				break
			}
			actions = append(actions, string(m.Action))
		case err := <-evs.Err:
			if err != nil && err != io.EOF {
				t.Errorf("events: %v", err)
			}
			done = true
		}
	}
	for _, want := range []string{"create", "start", "kill", "die", "stop"} {
		if !strings.Contains(","+strings.Join(actions, ",")+",", ","+want+",") {
			t.Errorf("events %q lack %s", actions, want)
		}
	}
}

// tcpBody reads what a server at addr sends, trying for 20 s while
// nothing answers.
func tcpBody(addr string) string {
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		c.Close()
		if len(b) > 0 {
			return string(b)
		}
	}
	return ""
}

// ryukChild is set in the process TestRyukReaps starts: it makes what
// Ryuk should remove, then exits without cleaning up.
const ryukChild = "RUNWHP_RYUK_CHILD"

// TestRyukReaps has a process of its own leave a container, its network
// and its volume behind and exit, as a test run killed midway would, and
// waits for Ryuk, which reaches the API through Docker's socket in its
// container, to remove them once the process's session has ended.
func TestRyukReaps(t *testing.T) {
	if tag := os.Getenv(ryukChild); tag != "" {
		ryukLeave(t, tag)
		return
	}
	ctx := need(t)
	if os.Getenv("TESTCONTAINERS_RYUK_DISABLED") == "true" {
		t.Skip("Ryuk is disabled")
	}
	tag := fmt.Sprintf("ryuk-%d", time.Now().UnixNano())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRyukReaps$", "-test.v")
	cmd.Env = append(os.Environ(), ryukChild+"="+tag, "TESTCONTAINERS_RYUK_RECONNECTION_TIMEOUT=1s")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the child: %v\n%s", err, out)
	}
	cli, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	left := func() []string {
		var names []string
		cs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", "runwhp.ryuk="+tag)})
		if err == nil {
			for _, c := range cs.Items {
				names = append(names, "container "+c.ID[:12])
			}
		}
		ns, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("label", "runwhp.ryuk="+tag)})
		if err == nil {
			for _, n := range ns.Items {
				names = append(names, "network "+n.Name)
			}
		}
		if _, err := cli.VolumeInspect(ctx, tag, client.VolumeInspectOptions{}); err == nil {
			names = append(names, "volume "+tag)
		}
		return names
	}
	if l := left(); len(l) != 3 {
		t.Fatalf("the child left %q, want a container, a network and a volume\n%s", l, out)
	}
	var l []string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if l = left(); len(l) == 0 {
			return
		}
	}
	t.Errorf("Ryuk left %q", l)
	// Not left for the next run.
	cs, _ := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: make(client.Filters).Add("label", "runwhp.ryuk="+tag)})
	for _, c := range cs.Items {
		cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})
	}
	ns, _ := cli.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("label", "runwhp.ryuk="+tag)})
	for _, n := range ns.Items {
		cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
	}
	cli.VolumeRemove(ctx, tag, client.VolumeRemoveOptions{Force: true})
}

// ryukLeave makes a container on a network, with a volume, and leaves them.
func ryukLeave(t *testing.T, tag string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	nw, err := network.New(ctx, network.WithLabels(map[string]string{"runwhp.ryuk": tag}))
	if err != nil {
		t.Fatal(err)
	}
	ctr, err := testcontainers.Run(ctx, alpine,
		testcontainers.WithCmd("sleep", "600"),
		testcontainers.WithLabels(map[string]string{"runwhp.ryuk": tag}),
		testcontainers.WithMounts(testcontainers.VolumeMount(tag, "/data")),
		network.WithNetwork(nil, nw))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("left container %s, network %s and volume %s", ctr.GetContainerID(), nw.Name, tag)
}
