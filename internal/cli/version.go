package cli

import "runtime/debug"

// version is set by the release build (-ldflags "-X .../internal/cli.version=v1.2.3");
// a `go install module@version` build reports the module version from the build info.
var version string

// Version is the version string `sqlshape version` prints: the release build's, else the
// module version recorded by `go install`, else "devel" with the VCS revision when known.
func Version() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev == "" {
		return "devel"
	}
	return "devel (" + rev + dirty + ")"
}
