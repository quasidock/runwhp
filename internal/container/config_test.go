package container

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNetworkMode(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want string
	}{
		{Config{Network: true}, "user"}, // made before iteration 18
		{Config{}, "none"},
		{Config{Network: true, NetworkMode: "user"}, "user"},
		{Config{Network: true, Networks: []Attachment{{Name: "b"}, {Name: "c"}}, NetworkMode: "c"}, "c"},
		{Config{Networks: []Attachment{{Name: "b"}}}, "b"},
		{Config{NetworkMode: "gone"}, "gone"}, // off its last network
	} {
		if got := c.cfg.Mode(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.cfg, got, c.want)
		}
	}
	// An old configuration reads as it did.
	var old Config
	json.Unmarshal([]byte(`{"version":1,"id":"x","network":true,"hostname":"h"}`), &old)
	if old.Mode() != "user" || old.Networks != nil {
		t.Errorf("old config: %+v", old)
	}
}

func TestConnectDisconnect(t *testing.T) {
	id := strings.Repeat("a", 64)
	cfg := &Config{ID: id, Network: true, NetworkMode: "front", Networks: []Attachment{{Network: "f", Name: "front"}}}
	c, err := cfg.Connect(Attachment{Network: "b", Name: "back", Aliases: []string{"db"}})
	if err != nil || len(c.Networks) != 2 || c.Networks[0].Name != "back" || c.NetworkMode != "front" || !c.Network {
		t.Fatalf("connect: %+v %v", c, err)
	}
	if len(cfg.Networks) != 1 {
		t.Error("Connect changed the original")
	}
	if _, err := c.Connect(Attachment{Network: "b", Name: "back"}); err == nil || err.Error() != "endpoint with name aaaaaaaaaaaa already exists in network back" {
		t.Errorf("twice: %v", err)
	}
	d, err := c.Disconnect("f", "front")
	if err != nil || len(d.Networks) != 1 || d.Networks[0].Name != "back" || !d.Network {
		t.Fatalf("disconnect: %+v %v", d, err)
	}
	if _, err := d.Disconnect("f", "front"); err == nil || err.Error() != "container "+id+" is not connected to network front" {
		t.Errorf("not connected: %v", err)
	}
	e, err := d.Disconnect("b", "back")
	if err != nil || e.Network || len(e.Networks) != 0 || e.Mode() != "front" {
		t.Errorf("off the last: %+v %v", e, err)
	}
	if _, err := (&Config{ID: id}).Connect(Attachment{Network: "b", Name: "back"}); err == nil || !strings.Contains(err.Error(), "private (none) mode") {
		t.Errorf("none: %v", err)
	}
	if _, err := (&Config{ID: id, Network: true}).Connect(Attachment{Network: "b", Name: "back"}); err == nil || !strings.Contains(err.Error(), "default network") {
		t.Errorf("default network: %v", err)
	}
}
