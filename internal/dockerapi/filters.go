package dockerapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DecodeFilters reads a request's filters parameter: JSON that maps each
// key to its values, either as a list, as older clients send it, or as a
// set ({"value": true}), as Docker's own client sends it. Keys are lower
// cased. An empty parameter is no filters.
func DecodeFilters(s string) (map[string][]string, error) {
	out := map[string][]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, fmt.Errorf("invalid filter: %v", err)
	}
	for k, v := range raw {
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			var set map[string]bool
			if err := json.Unmarshal(v, &set); err != nil {
				return nil, fmt.Errorf("invalid filter %q: want a list or a set of values", k)
			}
			for value, on := range set {
				if on {
					list = append(list, value)
				}
			}
			sort.Strings(list)
		}
		key := strings.ToLower(k)
		out[key] = append(out[key], list...)
	}
	return out, nil
}

// CheckFilterKeys refuses keys not among valid, as Docker's daemon does.
func CheckFilterKeys(f map[string][]string, valid ...string) error {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		ok := false
		for _, v := range valid {
			ok = ok || k == v
		}
		if !ok {
			return fmt.Errorf("invalid filter '%s'", k)
		}
	}
	return nil
}
