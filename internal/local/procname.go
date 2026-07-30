package local

import (
	"strings"
)

// exeName reduces a path to a comparable executable name: base name, lowercased,
// without a .exe suffix. It backs the Windows guard that refuses to terminate a
// pid whose image is not the configured daemon, so it lives here - outside the
// build-tagged files - to stay testable on every platform.
//
// The configured binary may be a bare name ("llama-server") or a full path,
// while the OS always reports a full path, so only the base names are compared.
// Both separators are handled explicitly rather than via filepath.Base, which
// only splits on "\" when the test itself runs on Windows.
func exeName(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".exe")
}
