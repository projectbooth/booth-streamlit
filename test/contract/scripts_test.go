package contract

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Every integration script the workflow runs directly must be executable in the checkout. Step c's
// first Integration run failed 20 minutes in with "lakehouse-access.sh: Permission denied" (run
// 37874512871); this catches it in CI's go-test job instead. lib.sh is sourced, but is executable
// too, as the others are.
func TestIntegrationScriptsAreExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes come from git on Linux; Windows checkouts don't carry them")
	}
	scripts, err := filepath.Glob(filepath.Join("..", "integration", "*.sh"))
	if err != nil || len(scripts) == 0 {
		t.Fatalf("no integration scripts found: %v", err)
	}
	for _, s := range scripts {
		fi, err := os.Stat(s)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (git update-index --chmod=+x)", filepath.Base(s))
		}
	}
}
