package cli

// `aimesh review acp --root` — the CLI half of the ACP trusted-root model. Over ACP the caller is
// a peer process, so the consent that the CLI gets from a typed path argument has to be given
// at LAUNCH instead. These tests pin the launch-time guards, which run BEFORE any server is
// started or any stdin is read (nothing here blocks on a stdio loop).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// A bad trusted-root set FAILS THE LAUNCH with a usage exit and a message naming the remedy —
// it never starts a server that would then refuse every request one at a time.
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
		{"no default and no root", []string{"acp", "--no-default-root"}, "--root"},
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
