package build

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// clock is a test's clock, a second further on at each reading.
func clock() func() time.Time {
	t := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func run(t *testing.T, w *fakeWorker, c Cache, o Options) (*Result, string) {
	t.Helper()
	var out bytes.Buffer
	o.Progress = NewProgress(&out, clock())
	o.Now = clock()
	res, err := Build(context.Background(), w, c, o)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out.String())
	}
	return res, out.String()
}

func dockerfile(s string) []byte { return []byte(strings.TrimLeft(s, "\n")) }

// TestHistoryAndConfig checks each instruction's history entry and
// configuration against Docker 29's (out/check17/df/meta.Dockerfile).
func TestHistoryAndConfig(t *testing.T) {
	w := newFakeWorker()
	res, out := run(t, w, newMemCache(), Options{Dockerfile: dockerfile(`
FROM alpine:3.20
ARG X=1
ENV A=b C=$X
LABEL l=v
WORKDIR /w
USER nobody
WORKDIR /w2
EXPOSE 80 443/udp
VOLUME /v
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=5s CMD true
SHELL ["/bin/sh", "-ec"]
ONBUILD RUN echo ob
MAINTAINER m
CMD ["x"]
ENTRYPOINT ["y"]
`)})
	img := built(t, res)
	want := []string{
		"ARG X=1", "ENV A=b C=1", "LABEL l=v", "WORKDIR /w", "USER nobody", "WORKDIR /w2 [layer]",
		"EXPOSE 443/udp 80/tcp", "VOLUME [/v]", "STOPSIGNAL SIGTERM",
		"HEALTHCHECK {Test:[CMD-SHELL true] Interval:5s Timeout:0s StartPeriod:0s StartInterval:0s Retries:0}",
		"SHELL [/bin/sh -ec]", "MAINTAINER m", `CMD ["x"]`, `ENTRYPOINT ["y"]`,
	}
	if got := createdBy(img); !slices.Equal(got, want) {
		t.Errorf("history:\n got %q\nwant %q", got, want)
	}
	c := img.Config
	if !slices.Equal(c.Env, []string{"PATH=" + DefaultPath, "A=b", "C=1"}) || c.User != "nobody" || c.WorkingDir != "/w2" ||
		c.Labels["l"] != "v" || c.StopSignal != "SIGTERM" || !slices.Equal(c.Shell, []string{"/bin/sh", "-ec"}) ||
		!slices.Equal(c.OnBuild, []string{"RUN echo ob"}) || !slices.Equal(c.Cmd, []string{"x"}) || !slices.Equal(c.Entrypoint, []string{"y"}) ||
		c.Healthcheck == nil || c.Healthcheck.Interval != 5*time.Second || len(c.ExposedPorts) != 2 || len(c.Volumes) != 1 || !c.ArgsEscaped {
		t.Errorf("config %+v", c)
	}
	for _, h := range img.History {
		if h.Comment != historyComment || h.Created == nil {
			t.Errorf("history entry %+v", h)
		}
	}
	if img.Created == nil || !img.Created.Equal(*img.History[len(img.History)-1].Created) {
		t.Errorf("created %v", img.Created)
	}
	// The two WORKDIRs' directories are made by one step at the end: /w as
	// root, /w2 as nobody.
	if w.count() != 1 || len(w.roots[0].puts) != 2 || w.roots[0].puts[0].opts.Chown != "" || w.roots[0].puts[1].opts.Chown != "nobody" {
		t.Errorf("steps %d, puts %+v", w.count(), w.roots)
	}
	if !strings.Contains(out, "[1/3] FROM alpine:3.20") || !strings.Contains(out, "[3/3] WORKDIR /w2") {
		t.Errorf("progress:\n%s", out)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.HasPrefix(s, "MaintainerDeprecated") }) {
		t.Errorf("warnings %q", res.Warnings)
	}
}

// TestCache builds twice: the second build runs nothing and makes the same
// image; a changed file reruns its COPY and what follows; a touched one
// does not.
func TestCache(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("one"), 0o644)
	ctxt, err := NewContext(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	df := dockerfile(`
FROM alpine:3.20
RUN echo a
COPY f /f
RUN echo b
`)
	w, cache := newFakeWorker(), newMemCache()
	first, _ := run(t, w, cache, Options{Dockerfile: df, Context: ctxt})
	if w.count() != 3 {
		t.Fatalf("first build ran %d steps", w.count())
	}
	second, out := run(t, w, cache, Options{Dockerfile: df, Context: ctxt})
	if w.count() != 3 || second.ID != first.ID || strings.Count(out, "CACHED") != 4 {
		t.Errorf("second build: %d steps, ID %s against %s\n%s", w.count(), second.ID, first.ID, out)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "f"), later, later)
	if third, _ := run(t, w, cache, Options{Dockerfile: df, Context: ctxt}); w.count() != 3 || third.ID != first.ID {
		t.Errorf("a touched file: %d steps, ID %s", w.count(), third.ID)
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("two"), 0o644)
	if fourth, _ := run(t, w, cache, Options{Dockerfile: df, Context: ctxt}); w.count() != 5 || fourth.ID == first.ID {
		t.Errorf("a changed file: %d steps, ID %s", w.count(), fourth.ID)
	}
	run(t, w, cache, Options{Dockerfile: df, Context: ctxt, NoCache: true})
	if w.count() != 8 {
		t.Errorf("--no-cache: %d steps", w.count())
	}
}

// TestArgs: global arguments in FROM, build arguments, the platform's,
// those no ARG declares, proxies kept out of history, and
// SOURCE_DATE_EPOCH.
func TestArgs(t *testing.T) {
	w := newFakeWorker()
	res, _ := run(t, w, newMemCache(), Options{Dockerfile: dockerfile(`
ARG BASE=alpine
FROM ${BASE}:3.20
ARG TARGETARCH
ARG V=default
ARG HTTP_PROXY
RUN echo $TARGETARCH $V
`), BuildArgs: map[string]string{"V": "given", "HTTP_PROXY": "http://p:1", "https_proxy": "http://q:2", "UNUSED": "x", "SOURCE_DATE_EPOCH": "1700000000"}})
	img := built(t, res)
	proc := w.steps[0].Run
	if want := []string{"/bin/sh", "-c", "echo $TARGETARCH $V"}; !slices.Equal(proc.Args, want) {
		t.Errorf("args %q", proc.Args)
	}
	for _, e := range []string{"TARGETARCH=amd64", "V=given", "HTTP_PROXY=http://p:1", "https_proxy=http://q:2", "HTTPS_PROXY=http://q:2"} {
		if !slices.Contains(proc.Env, e) {
			t.Errorf("env %q lacks %s", proc.Env, e)
		}
	}
	last := img.History[len(img.History)-1].CreatedBy
	if want := "RUN |3 TARGETARCH=amd64 V=given HTTP_PROXY=http://p:1 /bin/sh -c echo $TARGETARCH $V # buildkit"; last != want {
		t.Errorf("history %q, want %q", last, want)
	}
	if strings.Contains(string(res.Config), "q:2") {
		t.Error("an undeclared proxy argument is in the configuration")
	}
	epoch := time.Unix(1700000000, 0)
	for _, h := range img.History[2:] {
		if !h.Created.Equal(epoch) {
			t.Errorf("entry %q created %v", h.CreatedBy, h.Created)
		}
	}
	if base := time.Date(2026, 4, 16, 23, 53, 26, 0, time.UTC); !img.History[0].Created.Equal(base) {
		t.Errorf("the base's history was clamped: %v", img.History[0].Created)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, `"UNUSED"`) }) {
		t.Errorf("warnings %q", res.Warnings)
	}
}

// TestStages builds only what the target needs, and copies from stages.
func TestStages(t *testing.T) {
	w := newFakeWorker()
	src := newFakeRoot()
	src.files["/out"] = &fakeFile{dir: true, mode: 0o750, uid: 5, gid: 6}
	src.files["/out/bin"] = &fakeFile{mode: 0o755, uid: 5, gid: 6, data: "elf"}
	w.source = src
	df := dockerfile(`
FROM alpine:3.20 AS unused
RUN never
FROM alpine:3.20 AS build
RUN make
FROM alpine:3.20
COPY --from=build /out /app
COPY --from=1 /out/bin /bin/
`)
	res, out := run(t, w, newMemCache(), Options{Dockerfile: df})
	for _, s := range w.steps {
		if s.Run != nil && slices.Contains(s.Run.Args, "never") {
			t.Error("a stage the target does not need was built")
		}
	}
	if w.count() != 3 || !strings.Contains(out, "[build 2/2] RUN make") || !strings.Contains(out, "[stage-2 2/3] COPY --from=build /out /app") {
		t.Errorf("%d steps\n%s", w.count(), out)
	}
	puts := w.roots[1].puts
	if len(puts) != 1 || puts[0].dir != "/app/" || !slices.Equal(puts[0].entries, []string{". 750 0:0", "bin 755 5:6"}) {
		t.Errorf("COPY --from of a directory: %+v", puts)
	}
	puts = w.roots[2].puts
	if len(puts) != 1 || puts[0].dir != "/bin/" || !slices.Equal(puts[0].entries, []string{"bin 755 5:6"}) {
		t.Errorf("COPY --from of a file: %+v", puts)
	}
	if got := createdBy(built(t, res)); !slices.Equal(got, []string{"COPY /out /app # buildkit [layer]", "COPY /out/bin /bin/ # buildkit [layer]"}) {
		t.Errorf("history %q", got)
	}

	if _, err := Build(context.Background(), newFakeWorker(), newMemCache(), Options{Dockerfile: df, Target: "build", Now: clock()}); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), newFakeWorker(), newMemCache(), Options{Dockerfile: df, Target: "nope", Now: clock()}); err == nil {
		t.Error("an unknown target built")
	}
}

// TestCopy checks COPY's destinations and options against what BuildKit
// does (out/check17/df/copy.Dockerfile).
func TestCopy(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"f1": "a", "f2": "bb", "d/c": "c", "d/sub/x": "x", "ignored": "i"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
		os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644)
	}
	ctxt, err := NewContext(dir, []string{"ignored"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		line string
		dir  string
		want []string
		opts string
	}{
		{"COPY f1 /a/b/c/", "/a/b/c/", []string{"f1 755 0:0"}, ""},
		{"COPY f1 /m", "/", []string{"m 755 0:0"}, ""},
		{"COPY f1 /etc", "/etc/", []string{"f1 755 0:0"}, ""},
		{"COPY --chown=nobody:nogroup f1 /p/", "/p/", []string{"f1 755 0:0"}, "chown parents"},
		{"COPY --chmod=600 f2 /m1", "/", []string{"m1 600 0:0"}, ""},
		{"COPY --chmod=u-x,go-rx f2 /m2", "/", []string{"m2 600 0:0"}, ""},
		{"COPY d /dst", "/dst/", []string{". 755 0:0", "c 755 0:0", "sub 755 0:0", "sub/x 755 0:0"}, ""},
		{"COPY f* /wild/", "/wild/", []string{"f1 755 0:0"}, ""},
		{"COPY f1 f2 /notdir", "/notdir/", []string{"f1 755 0:0"}, ""},
		{"COPY --exclude=sub d /ex/", "/ex/", []string{". 755 0:0", "c 755 0:0"}, ""},
		{"COPY <<EOF /hdoc\nhello\nEOF", "/", []string{"hdoc 644 0:0"}, ""},
	} {
		w := newFakeWorker()
		_, err := Build(context.Background(), w, newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\n" + c.line + "\n"), Context: ctxt, Now: clock()})
		if err != nil {
			t.Errorf("%s: %v", c.line, err)
			continue
		}
		puts := w.roots[0].puts
		if len(puts) == 0 || puts[0].dir != c.dir || !slices.Equal(puts[0].entries, c.want) {
			t.Errorf("%s: puts %+v, want %s %q", c.line, puts, c.dir, c.want)
			continue
		}
		o := puts[0].opts
		if !o.MakeDir || !o.Merge || strings.Contains(c.opts, "parents") != o.ChownParents ||
			(strings.Contains(c.opts, "chown") && o.Chown != "nobody:nogroup") {
			t.Errorf("%s: options %+v", c.line, o)
		}
	}
	for _, c := range []struct{ line, want string }{
		{"COPY missing /x", `"/missing": not found`},
		{"COPY ignored /x", `"/ignored": not found`},
		{"COPY --link --chown=nobody f1 /x", "user and group names can't be resolved when used with --link"},
		{"ADD --checksum=sha256:00 f1 /x", "checksum requires HTTP(S) or Git sources"},
	} {
		_, err := Build(context.Background(), newFakeWorker(), newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\n" + c.line + "\n"), Context: ctxt, Now: clock()})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.line, err, c.want)
		}
	}
}

// TestRun checks RUN's arguments, heredocs, mounts and failures.
func TestRun(t *testing.T) {
	for _, c := range []struct {
		line    string
		args    []string
		history string
	}{
		{"RUN echo hi", []string{"/bin/sh", "-c", "echo hi"}, "RUN /bin/sh -c echo hi # buildkit"},
		{`RUN ["echo", "hi"]`, []string{"echo", "hi"}, "RUN echo hi # buildkit"},
		{"RUN <<EOF\necho one\necho two\nEOF", []string{"/bin/sh", "-c", "echo one\necho two\n"}, "RUN /bin/sh -c echo one\necho two\n # buildkit"},
		{"RUN <<EOF\n#!/bin/sh\necho x\nEOF", []string{"/bin/sh", "-c", "/dev/pipes/EOF"}, "RUN /bin/sh -c /dev/pipes/EOF # buildkit"},
		{"RUN <<A cat > /a && <<B cat > /b\naaa\nA\nbbb\nB", []string{"/bin/sh", "-c", "<<A cat > /a && <<B cat > /b\naaa\nA\nbbb\nB"}, "RUN /bin/sh -c <<A cat > /a && <<B cat > /b\naaa\nA\nbbb\nB # buildkit"},
	} {
		w := newFakeWorker()
		res, _ := run(t, w, newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\n" + c.line + "\n")})
		if got := w.steps[0].Run.Args; !slices.Equal(got, c.args) {
			t.Errorf("%q: args %q, want %q", c.line, got, c.args)
		}
		img := built(t, res)
		if got := img.History[len(img.History)-1].CreatedBy; got != c.history {
			t.Errorf("%q: history %q, want %q", c.line, got, c.history)
		}
		if proc := w.steps[0].Run; proc.Hostname != Hostname || proc.Cwd != "/" || !proc.Network {
			t.Errorf("%q: process %+v", c.line, proc)
		}
	}
	// The shebang heredoc is put in place to run.
	w := newFakeWorker()
	run(t, w, newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\nRUN <<EOF\n#!/bin/sh\necho x\nEOF\n")})
	if puts := w.roots[0].puts; len(puts) != 1 || puts[0].dir != "/dev/pipes" || !slices.Equal(puts[0].entries, []string{"EOF 755 0:0"}) {
		t.Errorf("the heredoc's file: %+v", puts)
	}

	secret := filepath.Join(t.TempDir(), "s")
	os.WriteFile(secret, []byte("hush"), 0o600)
	w = newFakeWorker()
	run(t, w, newMemCache(), Options{
		Dockerfile: dockerfile(`
FROM alpine:3.20
RUN --mount=type=cache,target=/root/.cache,uid=1000 --mount=type=tmpfs,target=/t --mount=type=secret,id=tok --mount=type=secret,id=tok,env=TOKEN,target=/s --network=none true
`),
		Secrets: map[string]Secret{"tok": {Path: secret}},
	})
	p := w.steps[0].Run
	if len(p.Mounts) != 4 || p.Mounts[0].Kind != "cache" || p.Mounts[0].ID != CacheVolume("/root/.cache") || *p.Mounts[0].UID != 1000 ||
		p.Mounts[1].Kind != "tmpfs" || p.Mounts[2].Target != "/run/secrets/tok" || p.Mounts[3].Target != "/s" || p.Network {
		t.Errorf("mounts %+v, network %v", p.Mounts, p.Network)
	}
	if !slices.Contains(p.Env, "TOKEN=hush") {
		t.Errorf("env %q", p.Env)
	}
	for _, c := range []struct{ line, want string }{
		{"RUN --mount=type=ssh true", "ssh is not supported"},
		{"RUN --mount=type=secret,id=nope,required true", "secret nope: not found"},
		{"RUN --mount=type=bind,source=.,target=/x true", "needs a build context"},
		{"RUN --mount=type=bind,from=x,target=/x true", "from= is not supported"},
	} {
		_, err := Build(context.Background(), newFakeWorker(), newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\n" + c.line + "\n"), Now: clock()})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.line, err, c.want)
		}
	}

	w = newFakeWorker()
	w.fail = "exit 3"
	_, err := Build(context.Background(), w, newMemCache(), Options{Dockerfile: []byte("FROM alpine:3.20\nRUN echo out; exit 3\n"), Now: clock()})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 3 || err.Error() != `process "/bin/sh -c echo out; exit 3" did not complete successfully: exit code: 3` {
		t.Errorf("a failing RUN: %v", err)
	}
	var le *LocatedError
	if !errors.As(err, &le) || le.Line != 2 || !strings.Contains(le.Excerpt(), ">>> RUN echo out; exit 3") {
		t.Errorf("located: %+v", le)
	}
}

// TestOnBuild runs a base image's triggers, and clears them.
func TestOnBuild(t *testing.T) {
	w := newFakeWorker()
	base := w.images["alpine:3.20"]
	base.Config.OnBuild = []string{"RUN echo ob"}
	base.Config.Shell = []string{"/bin/sh", "-ec"}
	w.images["parent"] = base
	res, out := run(t, w, newMemCache(), Options{Dockerfile: dockerfile("FROM parent\nLABEL child=1\n")})
	img := built(t, res)
	if img.Config.OnBuild != nil || !slices.Equal(createdBy(img), []string{"RUN /bin/sh -ec echo ob # buildkit [layer]", "LABEL child=1"}) {
		t.Errorf("onbuild %q, history %q", img.Config.OnBuild, createdBy(img))
	}
	if !strings.Contains(out, "[2/2] ONBUILD RUN echo ob") {
		t.Errorf("progress:\n%s", out)
	}
}

func TestChmod(t *testing.T) {
	for _, c := range []struct {
		spec      string
		mode, out int64
		dir       bool
	}{
		{"600", 0o755, 0o600, false},
		{"u+x,go-r", 0o640, 0o700, false},
		{"a=rX", 0o700, 0o555, true},
		{"a=rX", 0o600, 0o444, false},
		{"g+s,o-rwx", 0o755, 0o2750, true},
	} {
		f, err := parseChmod(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		if got := f(c.mode, c.dir); got != c.out {
			t.Errorf("%s on %o: %o, want %o", c.spec, c.mode, got, c.out)
		}
	}
	for _, bad := range []string{"10000", "999", "u+q", "x"} {
		if _, err := parseChmod(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestNormalizeHistory(t *testing.T) {
	at := func(s int) *time.Time {
		t := time.Unix(int64(s), 0)
		return &t
	}
	h := []ocispecs.History{{Created: at(1)}, {}, {}, {Created: at(5)}, {}, {Created: at(9)}, {}}
	got := normalizeHistory(h, 1, nil)
	want := []int64{1, 5, 5, 5, 5, 9, 9}
	for i, e := range got {
		if e.Created.Unix() != want[i] {
			t.Errorf("entry %d: %v, want %d", i, e.Created.Unix(), want[i])
		}
	}
	got = normalizeHistory(h, 1, at(3))
	for i, e := range got {
		if w := map[bool]int64{true: 1, false: 3}[i == 0]; e.Created.Unix() != w {
			t.Errorf("clamped entry %d: %v", i, e.Created.Unix())
		}
	}
}
