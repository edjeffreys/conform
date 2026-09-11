package webhook

import (
	"path/filepath"
	"strings"

	"github.com/edjeffreys/conform/internal/config"
)

// Longest prefix wins, so a narrower rule can sit inside a broader one.
func rewrite(path string, rules []config.Rewrite) string {
	path = filepath.Clean(path)
	best := -1
	for i, r := range rules {
		if !underPrefix(path, r.From) {
			continue
		}
		if best < 0 || len(r.From) > len(rules[best].From) {
			best = i
		}
	}
	if best < 0 {
		return path
	}
	return filepath.Join(rules[best].To, strings.TrimPrefix(path, rules[best].From))
}

// Whole components only: /tv must not match /tvshows.
func underPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(path, prefix)
}
