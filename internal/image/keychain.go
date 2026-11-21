package image

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// Keychain resolves registry credentials from Docker's config.json (or
// Podman's auth.json) and any credential helper it names, falling back to
// inline auths or anonymous access when a configured helper binary is not
// installed.
var Keychain authn.Keychain = keychain{}

type keychain struct{}

type dockerConfig struct {
	Auths       map[string]authn.AuthConfig `json:"auths"`
	CredsStore  string                      `json:"credsStore"`
	CredHelpers map[string]string           `json:"credHelpers"`
}

func (keychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return keychain{}.ResolveContext(context.Background(), target)
}

func (keychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	path := configFilePath()
	if path == "" {
		return authn.Anonymous, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return authn.Anonymous, nil
	} else if err != nil {
		return nil, err
	}
	var cfg dockerConfig
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	keys := []string{target.String(), target.RegistryStr()}
	if keys[0] == keys[1] {
		keys = keys[:1]
	}
	for _, key := range keys {
		if helper, serverURL := findHelper(cfg.CredHelpers, key); helper != "" {
			if ac, ok := helperAuth(ctx, helper, serverURL); ok {
				return authn.FromConfig(ac), nil
			}
			if alt := authServerURL(key); alt != serverURL {
				if ac, ok := helperAuth(ctx, helper, alt); ok {
					return authn.FromConfig(ac), nil
				}
			}
		} else if key == target.RegistryStr() && cfg.CredsStore != "" {
			if ac, ok := helperAuth(ctx, cfg.CredsStore, authServerURL(key)); ok {
				return authn.FromConfig(ac), nil
			}
		}
		if ac, ok := findAuth(cfg.Auths, key); ok {
			return authn.FromConfig(ac), nil
		}
	}
	return authn.Anonymous, nil
}

func configFilePath() string {
	home, _ := os.UserHomeDir()
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		if p := filepath.Join(dir, "config.json"); isRegularFile(p) {
			return p
		}
	} else if home != "" {
		if p := filepath.Join(home, ".docker", "config.json"); isRegularFile(p) {
			return p
		}
	}
	if p := os.Getenv("REGISTRY_AUTH_FILE"); p != "" && isRegularFile(p) {
		return p
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if p := filepath.Join(dir, "containers", "auth.json"); isRegularFile(p) {
			return p
		}
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" && home != "" {
		configDir = filepath.Join(home, ".config")
	}
	if configDir != "" {
		if p := filepath.Join(configDir, "containers", "auth.json"); isRegularFile(p) {
			return p
		}
	}
	return ""
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func findHelper(helpers map[string]string, key string) (helper, serverURL string) {
	if len(helpers) == 0 {
		return "", ""
	}
	for _, exact := range []string{key, authServerURL(key)} {
		if h := helpers[exact]; h != "" {
			return h, exact
		}
	}
	want := cleanKey(key)
	for k, h := range helpers {
		if h != "" && cleanKey(k) == want {
			return h, k
		}
	}
	return "", ""
}

func findAuth(auths map[string]authn.AuthConfig, key string) (authn.AuthConfig, bool) {
	if len(auths) == 0 {
		return authn.AuthConfig{}, false
	}
	var empty authn.AuthConfig
	for _, exact := range []string{key, authServerURL(key)} {
		if ac := auths[exact]; ac != empty {
			return ac, true
		}
	}
	want := cleanKey(key)
	for k, ac := range auths {
		if ac != empty && cleanKey(k) == want {
			return ac, true
		}
	}
	return authn.AuthConfig{}, false
}

func authServerURL(key string) string {
	if cleanKey(key) == name.DefaultRegistry {
		return authn.DefaultAuthKey
	}
	return key
}

func cleanKey(k string) string {
	k = strings.TrimSpace(k)
	if _, rest, hasScheme := strings.Cut(k, "://"); hasScheme {
		k = strings.TrimRight(rest, "/")
		if base, ok := strings.CutSuffix(k, "/v1"); ok {
			k = base
		} else if base, ok := strings.CutSuffix(k, "/v2"); ok {
			k = base
		}
	}
	k = strings.TrimRight(k, "/")
	host, path, hasPath := strings.Cut(k, "/")
	host = strings.ToLower(host)
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		host = name.DefaultRegistry
	}
	if hasPath {
		return host + "/" + path
	}
	return host
}

func helperAuth(ctx context.Context, helper, serverURL string) (authn.AuthConfig, bool) {
	path, err := exec.LookPath("docker-credential-" + helper)
	if err != nil {
		return authn.AuthConfig{}, false
	}
	cmd := exec.CommandContext(ctx, path, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	out, err := cmd.Output()
	if err != nil {
		return authn.AuthConfig{}, false
	}
	var creds struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out, &creds); err != nil {
		return authn.AuthConfig{}, false
	}
	if creds.Username == "" && creds.Secret == "" {
		return authn.AuthConfig{}, false
	}
	if creds.Username == "<token>" {
		return authn.AuthConfig{Username: creds.Username, IdentityToken: creds.Secret}, true
	}
	return authn.AuthConfig{Username: creds.Username, Password: creds.Secret}, true
}
