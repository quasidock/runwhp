//go:build linux

package main

import (
	"testing"

	"github.com/quasidock/runwhp/internal/guestspec"
)

func TestHostsFile(t *testing.T) {
	// The default network: the hostname at 127.0.0.1, as before.
	got := hostsFile(&guestspec.Spec{Hostname: "h", Hosts: []string{"10.0.2.2\thg"}})
	if want := "127.0.0.1\tlocalhost h\n::1\tlocalhost ip6-localhost ip6-loopback\n10.0.2.2\thg\n"; got != want {
		t.Errorf("default network:\n%s\nwant\n%s", got, want)
	}
	// A user-defined network: Docker's file, with the hostname at each of
	// the container's addresses after the extra hosts.
	got = hostsFile(&guestspec.Spec{Hostname: "h", Hosts: []string{"10.0.2.2\thg"}, HostnameAddrs: []string{"172.18.0.2", "172.19.0.2"}})
	want := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n" +
		"fe00::\tip6-localnet\nff00::\tip6-mcastprefix\nff02::1\tip6-allnodes\nff02::2\tip6-allrouters\n" +
		"10.0.2.2\thg\n172.18.0.2\th\n172.19.0.2\th\n"
	if got != want {
		t.Errorf("user-defined network:\n%s\nwant\n%s", got, want)
	}
}

func TestResolvFile(t *testing.T) {
	if got := resolvFile(&guestspec.Resolv{Nameservers: []string{"10.0.2.3"}}, false); got != "nameserver 10.0.2.3\n" {
		t.Errorf("default network: %q", got)
	}
	user := &guestspec.Resolv{Nameservers: []string{"172.18.0.1"}, Embedded: "172.18.0.1", Options: []string{"ndots:0"}}
	if got := resolvFile(user, true); got != "nameserver 127.0.0.11\noptions ndots:0\n" {
		t.Errorf("user-defined network: %q", got)
	}
	if got := resolvFile(user, false); got != "nameserver 172.18.0.1\noptions ndots:0\n" {
		t.Errorf("user-defined network, no embedded server: %q", got)
	}
	given := &guestspec.Resolv{Nameservers: []string{"192.0.2.53", "192.0.2.54"},
		Search: []string{"corp.test", "example.test"}, Options: []string{"ndots:2", "timeout:1"}}
	if got := resolvFile(given, false); got != "nameserver 192.0.2.53\nnameserver 192.0.2.54\nsearch corp.test example.test\noptions ndots:2 timeout:1\n" {
		t.Errorf("--dns, --dns-search, --dns-option: %q", got)
	}
}
