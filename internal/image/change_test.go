package image

import (
	"encoding/json"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestApplyChanges(t *testing.T) {
	cfg := v1.Config{Env: []string{"PATH=/bin", "X=0"}, WorkingDir: "/srv"}
	err := ApplyChanges(&cfg, []string{
		`CMD ["echo","hi"]`,
		`entrypoint /bin/echo hi`,
		`ENV A=b C="d e" X=1`,
		`ENV LEGACY value with spaces`,
		`EXPOSE 80 53/UDP`,
		`VOLUME /data`,
		`VOLUME ["/a", "/b"]`,
		`LABEL a=b c="d e" 'f g'=h`,
		`USER nobody`,
		`WORKDIR app`,
		`STOPSIGNAL SIGINT`,
		`ONBUILD RUN true`,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := v1.Config{
		Cmd:          []string{"echo", "hi"},
		Entrypoint:   []string{"/bin/sh", "-c", "/bin/echo hi"},
		Env:          []string{"PATH=/bin", "X=1", "A=b", "C=d e", "LEGACY=value with spaces"},
		ExposedPorts: map[string]struct{}{"80/tcp": {}, "53/udp": {}},
		Volumes:      map[string]struct{}{"/data": {}, "/a": {}, "/b": {}},
		Labels:       map[string]string{"a": "b", "c": "d e", "f g": "h"},
		User:         "nobody",
		WorkingDir:   "/srv/app",
		StopSignal:   "SIGINT",
		OnBuild:      []string{"RUN true"},
	}
	got, _ := json.Marshal(cfg)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("config\n got %s\nwant %s", got, exp)
	}
	for _, bad := range []string{"RUN echo", "FROM alpine", "COPY a b", "ENV", "LABEL x", `ENV A="b`, "WORKDIR", "EXPOSE"} {
		if err := ApplyChanges(&v1.Config{}, []string{bad}); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if err := ApplyChanges(&v1.Config{}, []string{"RUN echo"}); err == nil || err.Error() != "RUN is not a valid change command" {
		t.Errorf("RUN: %v", err)
	}
}
