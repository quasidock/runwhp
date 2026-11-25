package build

import (
	"fmt"
	"strconv"
	"strings"
)

// parseChmod parses COPY --chmod: an octal mode, which replaces a file's,
// or chmod's symbolic modes, which change it, as BuildKit takes them.
func parseChmod(spec string) (func(mode int64, dir bool) int64, error) {
	if n, err := strconv.ParseUint(spec, 8, 32); err == nil {
		if n > 0o7777 {
			return nil, fmt.Errorf("invalid chmod parameter: '%v'. it should be octal string and between 0 and 07777", spec)
		}
		return func(mode int64, _ bool) int64 { return mode&^0o7777 | int64(n) }, nil
	} else if strings.Trim(spec, "0123456789") == "" {
		return nil, fmt.Errorf("invalid chmod parameter: '%v'. it should be octal string and between 0 and 07777", spec)
	}
	type clause struct {
		who   int64
		op    byte
		perms string
	}
	var clauses []clause
	for _, part := range strings.Split(spec, ",") {
		i := 0
		var who int64
		for ; i < len(part) && strings.IndexByte("ugoa", part[i]) >= 0; i++ {
			switch part[i] {
			case 'u':
				who |= 0o4700
			case 'g':
				who |= 0o2070
			case 'o':
				who |= 0o1007
			case 'a':
				who |= 0o7777
			}
		}
		if i == len(part) || strings.IndexByte("+-=", part[i]) < 0 {
			return nil, fmt.Errorf("invalid chmod parameter: %q", spec)
		}
		op := part[i]
		perms := part[i+1:]
		if strings.Trim(perms, "rwxXst") != "" {
			return nil, fmt.Errorf("invalid chmod parameter: %q", spec)
		}
		if who == 0 {
			who = 0o7777
		}
		clauses = append(clauses, clause{who, op, perms})
	}
	return func(mode int64, dir bool) int64 {
		m := mode & 0o7777
		for _, c := range clauses {
			var bits int64
			for _, p := range c.perms {
				switch p {
				case 'r':
					bits |= 0o444
				case 'w':
					bits |= 0o222
				case 'x':
					bits |= 0o111
				case 'X':
					if dir || m&0o111 != 0 {
						bits |= 0o111
					}
				case 's':
					bits |= 0o6000
				case 't':
					bits |= 0o1000
				}
			}
			bits &= c.who
			switch c.op {
			case '+':
				m |= bits
			case '-':
				m &^= bits
			case '=':
				m = m&^(c.who&0o777) | bits
			}
		}
		return mode&^0o7777 | m
	}, nil
}
