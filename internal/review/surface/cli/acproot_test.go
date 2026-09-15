package cli

// These tests cover the launch-time guards on `aimesh review acp --root`, which run before any server
// starts or stdin is read.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// A bad --root fails the launch with a usage exit and a message naming the problem.
func TestACPRoots_BadRootSetFailsLaunch(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	ssh := filepath.Join(t.TempDir(), ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"nonexistent root", []string{"acp", "--root", missing}, "cannot be used"},
		{"protected root", []string{"acp", "--root", ssh}, "protected path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := run(t, tc.args...)
			if code != int(fault.Usage) {
				t.Fatalf("code = %d, want %d (usage); stderr=%s", code, fault.Usage, errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", errs, tc.want)
			}
		})
	}
}
