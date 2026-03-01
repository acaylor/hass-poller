package filter

import "path"

type GlobFilter struct {
	allowlist []string
	blocklist []string
}

func NewGlobFilter(allowlist []string, blocklist []string) *GlobFilter {
	return &GlobFilter{allowlist: allowlist, blocklist: blocklist}
}

func (f *GlobFilter) Allowed(entityID string) bool {
	if len(f.allowlist) > 0 && !matchAny(f.allowlist, entityID) {
		return false
	}
	if len(f.blocklist) > 0 && matchAny(f.blocklist, entityID) {
		return false
	}
	return true
}

func matchAny(globs []string, value string) bool {
	for _, glob := range globs {
		matched, err := path.Match(glob, value)
		if err != nil {
			continue
		}
		if matched {
			return true
		}
	}
	return false
}
