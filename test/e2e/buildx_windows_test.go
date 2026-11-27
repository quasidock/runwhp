//go:build windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildxAPI is the test's Engine API with buildx installed, whose builder
// comes from the e2e mirror, so that each store does not pull BuildKit's
// image from Docker Hub.
func buildxAPI(t *testing.T) *dockerAPI {
	t.Helper()
	plugin := os.Getenv("RUNWHP_E2E_BUILDX")
	if plugin == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_BUILDX to buildx's docker-buildx.exe to run BuildKit's tests")
	}
	setEnv(t, "RUNWHP_BUILDKIT_IMAGE", mirrored(t, "moby/buildkit:v0.35.0"))
	d := newDockerAPI(t)
	installPlugin(t, d, plugin, "docker-buildx.exe")
	d.image(alpine)
	return d
}

// installPlugin puts a docker CLI plugin where the test's docker CLI finds
// its plugins.
func installPlugin(t *testing.T, d *dockerAPI, plugin, name string) {
	t.Helper()
	dir := filepath.Join(d.config, "cli-plugins")
	os.MkdirAll(dir, 0o755)
	data, err := os.ReadFile(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestBuildx builds with buildx's docker driver, through the API's /grpc
// and /session to the builder's BuildKit: images loaded into runwhp's
// store and run, docker build forwarded to buildx, BuildKit's mounts and
// stages, FROM an image only runwhp's store has, outputs to the client,
// a push and its registry cache, two builds at once, and the cache kept
// across a restart of the server.
func TestBuildx(t *testing.T) {
	parallel(t)
	d := buildxAPI(t)
	if ls := d.dok("buildx", "ls"); !strings.Contains(ls, "docker") || !strings.Contains(ls, "v0.35.0") {
		t.Errorf("buildx ls does not show the docker driver with BuildKit v0.35.0:\n%s", ls)
	}

	t.Run("load", func(t *testing.T) {
		dir := buildDir(t, map[string]string{
			"Dockerfile": `FROM alpine:3.20 AS base
ARG GREETING=hello
RUN echo "$GREETING from buildkit" > /greeting
FROM base AS final
COPY file.txt /file.txt
RUN --network=none cat /file.txt >> /greeting
CMD ["cat", "/greeting"]
FROM base AS other
RUN false
`,
			"file.txt": "copied\n",
		})
		iid := filepath.Join(t.TempDir(), "iid")
		d.dok("buildx", "build", "--target", "final", "--build-arg", "GREETING=hi", "--iidfile", iid, "-t", "bx-load:1", dir)
		if out := d.dok("run", "--rm", "bx-load:1"); out != "hi from buildkit\ncopied\n" {
			t.Errorf("run: %q", out)
		}
		id, _ := os.ReadFile(iid)
		if inspected := d.dok("image", "inspect", "--format", "{{.Id}}", "bx-load:1"); strings.TrimSpace(inspected) != strings.TrimSpace(string(id)) {
			t.Errorf("--iidfile %q, inspect %q", id, inspected)
		}
		if r := d.cli("", "run", "--rm", "bx-load:1"); r.stdout != "hi from buildkit\ncopied\n" {
			t.Errorf("runwhp run: %+v", r)
		}
		// docker build goes to buildx when it is installed.
		r := d.docker1("build", "-q", "--target", "final", "-t", "bx-load:2", dir)
		if r.code != 0 || strings.Contains(r.stderr, "legacy builder") {
			t.Errorf("docker build: %+v", r)
		}
	})

	t.Run("local FROM", func(t *testing.T) {
		// An image only runwhp's store has, built by runwhp's own builder.
		base := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo local > /base\n"})
		d.ok("build", "-q", "-t", "bx-localbase:1", base)
		dir := buildDir(t, map[string]string{"Dockerfile": "FROM bx-localbase:1\nRUN cat /base > /seen\nCMD [\"cat\", \"/seen\"]\n"})
		d.dok("buildx", "build", "-t", "bx-local:1", dir)
		if out := d.dok("run", "--rm", "bx-local:1"); out != "local\n" {
			t.Errorf("run: %q", out)
		}
		if r := d.docker1("buildx", "build", "--pull", "-t", "bx-local:2", dir); r.code == 0 {
			t.Errorf("--pull found an image only the store has: %+v", r)
		}
	})

	t.Run("mounts", func(t *testing.T) {
		dir := buildDir(t, map[string]string{
			"Dockerfile": `FROM alpine:3.20 AS tools
RUN echo tool > /tool
FROM alpine:3.20
RUN --mount=type=secret,id=token cat /run/secrets/token > /token-seen
RUN --mount=type=cache,target=/cache echo cached >> /cache/log && cp /cache/log /cache-log
RUN --mount=type=bind,from=tools,source=/tool,target=/mnt/tool cp /mnt/tool /tool-seen
CMD ["sh", "-c", "cat /token-seen /cache-log /tool-seen"]
`,
			"token.txt": "s3cret",
		})
		d.dok("buildx", "build", "--secret", "id=token,src="+filepath.Join(dir, "token.txt"), "-t", "bx-mounts:1", dir)
		if out := d.dok("run", "--rm", "bx-mounts:1"); out != "s3cretcached\ntool\n" {
			t.Errorf("run: %q", out)
		}
		if h := d.dok("history", "--no-trunc", "bx-mounts:1"); strings.Contains(h, "s3cret") {
			t.Errorf("the secret is in the history:\n%s", h)
		}
	})

	t.Run("outputs", func(t *testing.T) {
		dir := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo out > /out.txt\n"})
		local := t.TempDir()
		d.dok("buildx", "build", "--output", "type=local,dest="+local, dir)
		if data, err := os.ReadFile(filepath.Join(local, "out.txt")); err != nil || string(data) != "out\n" {
			t.Errorf("type=local: %q, %v", data, err)
		}
		for _, typ := range []string{"tar", "oci", "docker"} {
			f := filepath.Join(t.TempDir(), typ+".tar")
			d.dok("buildx", "build", "--output", "type="+typ+",dest="+f, dir)
			if fi, err := os.Stat(f); err != nil || fi.Size() < 1024 {
				t.Errorf("type=%s: %v", typ, err)
			}
		}
		// A docker archive loads back, as docker load takes it.
		f := filepath.Join(t.TempDir(), "docker.tar")
		d.dok("buildx", "build", "--output", "type=docker,name=bx-out:1,dest="+f, dir)
		d.dok("load", "-i", f)
		if out := d.dok("run", "--rm", "bx-out:1", "cat", "/out.txt"); out != "out\n" {
			t.Errorf("loaded: %q", out)
		}
	})

	t.Run("push and registry cache", func(t *testing.T) {
		// The builder reaches the test's registry on the host's loopback
		// as host.docker.internal; buildx's own mirror host would be the
		// builder's own loopback.
		reg := strings.Replace(mirrored(t, alpine), "localhost", "host.docker.internal", 1)
		reg = reg[:strings.Index(reg, "/")]
		dir := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo pushed > /p\n"})
		ref := reg + "/bx/pushed:1"
		d.dok("buildx", "build", "--output", "type=image,name="+ref+",push=true,registry.insecure=true",
			"--cache-to", "type=registry,ref="+reg+"/bx/cache:1,registry.insecure=true", dir)
		pulled := strings.Replace(ref, "host.docker.internal", "localhost", 1)
		if out := d.dok("run", "--rm", pulled, "cat", "/p"); out != "pushed\n" {
			t.Errorf("the pushed image: %q", out)
		}
		d.dok("buildx", "prune", "-af")
		r := d.docker1("buildx", "build", "--progress", "plain", "--cache-from", "type=registry,ref="+reg+"/bx/cache:1,registry.insecure=true", "-t", "bx-cached:1", dir)
		out := r.stderr
		if r.code != 0 || !strings.Contains(out, "CACHED") {
			t.Errorf("the registry cache was not used:\n%s", out)
		}
	})

	t.Run("parallel", func(t *testing.T) {
		var wg sync.WaitGroup
		for _, n := range []string{"a", "b"} {
			dir := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo " + n + " > /n && sleep 1\n"})
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r := d.docker1("buildx", "build", "-t", "bx-par:"+n, dir); r.code != 0 {
					t.Errorf("build %s: %+v", n, r)
				}
			}()
		}
		wg.Wait()
		for _, n := range []string{"a", "b"} {
			if out := d.dok("run", "--rm", "bx-par:"+n, "cat", "/n"); out != n+"\n" {
				t.Errorf("bx-par:%s: %q", n, out)
			}
		}
	})

	t.Run("cache across restarts", func(t *testing.T) {
		dir := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo slow > /s\n"})
		d.dok("buildx", "build", "-t", "bx-restart:1", dir)
		d.ok("docker-api", "stop", "--pipe", d.pipe)
		d.ok("docker-api", "start", "--pipe", d.pipe)
		r := d.docker1("buildx", "build", "--progress", "plain", "-t", "bx-restart:1", dir)
		out := r.stderr
		if r.code != 0 || !strings.Contains(out, "CACHED") {
			t.Errorf("no cache after the server restarted:\n%s", out)
		}
		if du := d.dok("buildx", "du"); !strings.Contains(du, "Total") {
			t.Errorf("buildx du:\n%s", du)
		}
	})

	t.Run("hidden", func(t *testing.T) {
		if ps := d.dok("ps", "-a", "--format", "{{.Names}}"); strings.Contains(ps, "runwhp_buildkit") {
			t.Errorf("docker ps lists the builder:\n%s", ps)
		}
		if r := d.cli("", "list"); strings.Contains(r.stdout, "runwhp_buildkit") {
			t.Errorf("runwhp list lists the builder:\n%s", r.stdout)
		}
		d.dok("container", "prune", "-f")
		if r := d.cli("", "state", "runwhp_buildkit"); r.code != 0 {
			t.Errorf("container prune removed the builder: %+v", r)
		}
	})
}

// TestBuildxCompose builds a project with Docker Compose through buildx
// bake: a service built FROM another's image, which only runwhp's store
// has, then brought up.
func TestBuildxCompose(t *testing.T) {
	parallel(t)
	plugin := os.Getenv("RUNWHP_E2E_COMPOSE_PLUGIN")
	if plugin == "" {
		runwhp(t)
		t.Skip("set RUNWHP_E2E_COMPOSE_PLUGIN to Docker Compose's docker-compose.exe")
	}
	d := buildxAPI(t)
	installPlugin(t, d, plugin, "docker-compose.exe")
	dir := buildDir(t, map[string]string{
		"compose.yaml": `name: bxcompose
services:
  base:
    build: ./base
    image: bxcompose-base:1
    command: ["true"]
  app:
    build: ./app
    depends_on: [base]
    command: ["sh", "-c", "cat /base /app && sleep 1000"]
`,
		"base/Dockerfile": "FROM alpine:3.20\nRUN echo base > /base\n",
		"app/Dockerfile":  "FROM bxcompose-base:1\nRUN echo app > /app\n",
	})
	compose := func(args ...string) result {
		cmd := d.cmd(nil, append([]string{"compose", "--project-directory", dir}, args...)...)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return result{stdout.String(), stderr.String(), exitCode(err)}
	}
	// base first, then app FROM it: bake builds them at once otherwise.
	if r := compose("build", "base"); r.code != 0 {
		t.Fatalf("compose build base: %+v", r)
	}
	if r := compose("build", "app"); r.code != 0 || strings.Contains(r.stderr, "classic builder") {
		t.Fatalf("compose build app: %+v", r)
	}
	if r := compose("up", "-d", "app"); r.code != 0 {
		t.Fatalf("compose up: %+v", r)
	}
	t.Cleanup(func() { compose("down", "-t", "0") })
	waitFor(t, time.Minute, func() bool {
		return strings.Contains(compose("logs", "app").stdout, "base\n") && strings.Contains(compose("logs", "app").stdout, "app\n")
	})
}

// TestBuildxContainerDriver creates a builder of buildx's own, with the
// docker-container driver: a privileged moby/buildkit container that buildx
// reaches with docker exec, and loads its images through docker load.
func TestBuildxContainerDriver(t *testing.T) {
	parallel(t)
	d := buildxAPI(t)
	img := mirrored(t, "moby/buildkit:v0.35.0")
	d.dok("buildx", "create", "--name", "bxdc", "--driver", "docker-container", "--driver-opt", "image="+img)
	t.Cleanup(func() { d.docker1("buildx", "rm", "-f", "bxdc") })
	dir := buildDir(t, map[string]string{"Dockerfile": "FROM alpine:3.20\nRUN echo container-driver > /c\n"})
	d.dok("buildx", "build", "--builder", "bxdc", "--load", "-t", "bx-dc:1", dir)
	if out := d.dok("run", "--rm", "bx-dc:1", "cat", "/c"); out != "container-driver\n" {
		t.Errorf("run: %q", out)
	}
}

// TestDockerInDocker runs docker:dind privileged, as Docker's own image
// for Docker in Docker: its daemon runs containers and builds.
func TestDockerInDocker(t *testing.T) {
	parallel(t)
	d := newDockerAPI(t)
	dind := d.image("docker:29-dind")
	d.image(alpine)
	d.dok("run", "-d", "--privileged", "--name", "dind", "-v", "dindstate:/var/lib/docker", dind)
	waitFor(t, time.Minute, func() bool { return d.docker1("exec", "dind", "docker", "info").code == 0 })
	r := d.docker1("exec", "dind", "docker", "info", "--format", "{{.CgroupVersion}} {{.Driver}}")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "2 ") {
		t.Errorf("docker info in dind: %+v", r)
	}
	build := "printf 'FROM scratch\\nCOPY f /f\\n' > /tmp/Dockerfile && echo built > /tmp/f && docker build -q -t inner /tmp && docker image ls -q inner"
	if r := d.docker1("exec", "dind", "sh", "-c", build); r.code != 0 || strings.TrimSpace(r.stdout) == "" {
		t.Errorf("a build in dind: %+v", r)
	}
}
