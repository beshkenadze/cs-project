// Package caps computes the capabilities of a member. Members have their own allow-list,
// not the set of the launcher.
package caps

import (
	"fmt"
	"sort"
	"strings"
)

// Default is the Docker default set.
var Default = []string{
	"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER", "CAP_MKNOD", "CAP_NET_RAW",
	"CAP_SETGID", "CAP_SETUID", "CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
	"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
}

// Additions are the only names that CapAdd can add.
var Additions = []string{"CAP_IPC_LOCK", "CAP_SYS_NICE"}

// Normalize gives the CAP_-prefixed upper-case name.
func Normalize(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "ALL" || strings.HasPrefix(n, "CAP_") {
		return n
	}
	return "CAP_" + n
}

// Effective gives the sorted set: Default plus add minus drop. It refuses an addition outside
// Default and Additions.
func Effective(add, drop []string) ([]string, error) {
	set := map[string]bool{}
	for _, c := range Default {
		set[c] = true
	}
	allowed := map[string]bool{}
	for c := range set {
		allowed[c] = true
	}
	for _, raw := range drop {
		if Normalize(raw) == "ALL" {
			set = map[string]bool{}
		}
	}
	for _, raw := range add {
		c := Normalize(raw)
		if !allowed[c] && !contains(Additions, c) {
			return nil, fmt.Errorf("cap:%s", strings.TrimPrefix(c, "CAP_"))
		}
		set[c] = true
	}
	for _, raw := range drop {
		delete(set, Normalize(raw))
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
