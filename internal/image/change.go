package image

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// ApplyChanges applies Dockerfile instructions to an image configuration,
// as `docker import -c` and `docker commit -c` take them: CMD, ENTRYPOINT,
// ENV, EXPOSE, LABEL, ONBUILD, STOPSIGNAL, USER, VOLUME and WORKDIR. Any
// other instruction is refused, as Docker refuses it.
func ApplyChanges(cfg *v1.Config, changes []string) error {
	for _, change := range changes {
		instr, args, _ := strings.Cut(strings.TrimSpace(change), " ")
		args = strings.TrimSpace(args)
		if err := applyChange(cfg, strings.ToUpper(instr), args); err != nil {
			return err
		}
	}
	return nil
}

func applyChange(cfg *v1.Config, instr, args string) error {
	switch instr {
	case "CMD":
		cfg.Cmd = command(args)
	case "ENTRYPOINT":
		cfg.Entrypoint = command(args)
	case "ENV":
		pairs, err := keyValues(instr, args)
		if err != nil {
			return err
		}
		for _, kv := range pairs {
			cfg.Env = setEnv(cfg.Env, kv[0], kv[1])
		}
	case "LABEL":
		pairs, err := keyValues(instr, args)
		if err != nil {
			return err
		}
		if cfg.Labels == nil {
			cfg.Labels = map[string]string{}
		}
		for _, kv := range pairs {
			cfg.Labels[kv[0]] = kv[1]
		}
	case "EXPOSE":
		words, err := shellWords(args)
		if err != nil || len(words) == 0 {
			return fmt.Errorf("EXPOSE requires at least one argument")
		}
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		for _, w := range words {
			if !strings.Contains(w, "/") {
				w += "/tcp"
			}
			cfg.ExposedPorts[strings.ToLower(w)] = struct{}{}
		}
	case "ONBUILD":
		if args == "" {
			return fmt.Errorf("ONBUILD requires at least one argument")
		}
		cfg.OnBuild = append(cfg.OnBuild, args)
	case "STOPSIGNAL":
		if args == "" {
			return fmt.Errorf("STOPSIGNAL requires exactly one argument")
		}
		cfg.StopSignal = args
	case "USER":
		if args == "" {
			return fmt.Errorf("USER requires exactly one argument")
		}
		cfg.User = args
	case "VOLUME":
		var vols []string
		if err := json.Unmarshal([]byte(args), &vols); err != nil {
			if vols, err = shellWords(args); err != nil {
				return err
			}
		}
		if len(vols) == 0 {
			return fmt.Errorf("VOLUME requires at least one argument")
		}
		if cfg.Volumes == nil {
			cfg.Volumes = map[string]struct{}{}
		}
		for _, v := range vols {
			cfg.Volumes[v] = struct{}{}
		}
	case "WORKDIR":
		if args == "" {
			return fmt.Errorf("WORKDIR requires exactly one argument")
		}
		if !path.IsAbs(args) {
			args = path.Join("/", cfg.WorkingDir, args)
		}
		cfg.WorkingDir = args
	default:
		return fmt.Errorf("%s is not a valid change command", instr)
	}
	return nil
}

// command is CMD's or ENTRYPOINT's: a JSON array of strings, the exec
// form, or else a string for /bin/sh -c, the shell form.
func command(args string) []string {
	var argv []string
	if strings.HasPrefix(args, "[") && json.Unmarshal([]byte(args), &argv) == nil {
		return argv
	}
	return []string{"/bin/sh", "-c", args}
}

// keyValues parses ENV's and LABEL's KEY=VALUE pairs, quoted as a shell
// quotes them, or the old form KEY VALUE, where VALUE is the rest.
func keyValues(instr, args string) ([][2]string, error) {
	words, err := shellWords(args)
	if err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("%s requires at least one argument", instr)
	}
	if !strings.Contains(words[0], "=") {
		key, value, _ := strings.Cut(args, " ")
		if key == "" || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s names %q with no value", instr, key)
		}
		return [][2]string{{key, strings.TrimSpace(value)}}, nil
	}
	var out [][2]string
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s: %q is not KEY=VALUE", instr, w)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// shellWords splits s into words as a shell would, with single and double
// quotes and backslash escapes, but no expansion.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, c := range s {
		switch {
		case escaped:
			cur.WriteRune(c)
			escaped = false
		case c == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote, inWord = c, true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unmatched %c in %q", quote, s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func setEnv(env []string, key, value string) []string {
	for i, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			env[i] = key + "=" + value
			return env
		}
	}
	return append(env, key+"="+value)
}
