//go:build windows

package proc

// Cwd is unsupported on Windows. Per-process working directory is
// available via NtQueryInformationProcess but binding it cleanly
// without a new third-party dep is more work than the L2 followup
// warrants. Callers degrade to non-validating behaviour.
func Cwd(pid int) (string, error) {
	return "", ErrCwdUnsupported
}
