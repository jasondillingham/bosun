package proc

import "errors"

// ErrCwdUnsupported is returned by Cwd on platforms that don't
// expose per-process cwd through a stable filesystem interface.
// Today: macOS and Windows. Callers should treat it as a soft
// signal — degrade to non-validating behaviour, don't refuse.
//
// Linux has /proc/<pid>/cwd; that's the only platform Cwd
// implements today. macOS's libproc could be wired via cgo and
// Windows's NtQueryInformationProcess via golang.org/x/sys but
// both are deferred — the followup task that motivated Cwd
// (v0.12 L2: bosun_attach PID validation) is explicitly best-
// effort.
var ErrCwdUnsupported = errors.New("proc: cwd lookup unsupported on this platform")
