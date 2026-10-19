package a4c_test

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// a4cExeSuffix is ".exe" on Windows and empty elsewhere. Windows resolves and
// executes binaries by extension, so a build output without the ".exe" suffix
// cannot be run through os/exec.
func a4cExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// requireMake skips a test that executes a Makefile-based verification
// contract on hosts without make installed (commonly the case on Windows).
func requireMake(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available on this host, skipping Makefile verification test: %v", err)
	}
}

// isExecutableFile reports whether path looks like a runnable binary: a
// regular file whose extension the platform will execute. Windows keys on the
// extension (no executable bit), so only existence matters there.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode()&0111 != 0
}
