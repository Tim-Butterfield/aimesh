package pathexpand

import (
	"errors"
	"runtime"
	"testing"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

func testEnv(goos, home string, vars map[string]string) Env {
	return Env{
		GOOS: goos,
		Lookup: func(name string) (string, bool) {
			v, ok := vars[name]
			return v, ok
		},
		Home: func() (string, error) {
			if home == "" {
				return "", errors.New("no home")
			}
			return home, nil
		},
	}
}

func TestExpand_Windows(t *testing.T) {
	vars := map[string]string{
		"LOCALAPPDATA": `C:\Users\alice\AppData\Local`,
		"REL":          `tools`,
		"PCT":          `C:\50%`,
	}
	env := testEnv("windows", `C:\Users\alice`, vars)
	cases := []struct {
		name, raw, want, reason string
	}{
		{"variable", `%LOCALAPPDATA%\devin\cli\bin\devin.exe`, `C:\Users\alice\AppData\Local\devin\cli\bin\devin.exe`, ""},
		{"literal path", `C:\tools\devin.exe`, `C:\tools\devin.exe`, ""},
		{"forward slashes", `C:/tools/devin.exe`, `C:/tools/devin.exe`, ""},
		{"admin share keeps dollar", `\\server\C$\tools\devin.exe`, `\\server\C$\tools\devin.exe`, ""},
		{"dollar is literal", `C:\$HOME\x.exe`, `C:\$HOME\x.exe`, ""},
		{"double percent is literal", `C:\100%%\x.exe`, `C:\100%\x.exe`, ""},
		{"value is not re-scanned", `%PCT%\x.exe`, `C:\50%\x.exe`, ""},
		{"tilde backslash", `~\bin\x.exe`, `C:\Users\alice\bin\x.exe`, ""},
		{"tilde slash", `~/bin/x.exe`, `C:\Users\alice/bin/x.exe`, ""},
		{"undefined variable", `%NOPE%\x.exe`, "", ReasonUndefinedVariable},
		{"unmatched percent", `C:\a%b\x.exe`, "", ReasonUnmatchedPercent},
		{"trailing lone percent", `%LOCALAPPDATA%%`, "", ReasonUnmatchedPercent},
		{"relative result", `%REL%\x.exe`, "", ReasonNotAbsolute},
		{"relative literal", `tools\x.exe`, "", ReasonNotAbsolute},
		{"other user home", `~bob\x.exe`, "", ReasonOtherUserHome},
		{"empty", `  `, "", ReasonEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.raw, env)
			check(t, tc.raw, got, err, tc.want, tc.reason)
		})
	}
}

func TestExpand_Unix(t *testing.T) {
	vars := map[string]string{
		"HOME":    "/Users/alice",
		"TOOLS":   "/opt/tools",
		"REL":     "tools",
		"WITHDOL": "/opt/$X",
	}
	env := testEnv("darwin", "/Users/alice", vars)
	cases := []struct {
		name, raw, want, reason string
	}{
		{"dollar name", "$HOME/.local/bin/devin", "/Users/alice/.local/bin/devin", ""},
		{"braced name", "${TOOLS}/devin", "/opt/tools/devin", ""},
		{"braced name before letters", "${TOOLS}bin/devin", "/opt/toolsbin/devin", ""},
		{"double dollar is literal", "/opt/$$/devin", "/opt/$/devin", ""},
		{"lone dollar is literal", "/opt/$/devin", "/opt/$/devin", ""},
		{"trailing dollar is literal", "/opt/devin$", "/opt/devin$", ""},
		{"percent is literal", "/opt/%TOOLS%/devin", "/opt/%TOOLS%/devin", ""},
		{"value is not re-scanned", "$WITHDOL/devin", "/opt/$X/devin", ""},
		{"tilde", "~/.local/bin/devin", "/Users/alice/.local/bin/devin", ""},
		{"tilde alone", "~", "/Users/alice", ""},
		{"backslash after tilde is not a separator", `~\bin`, "", ReasonOtherUserHome},
		{"undefined variable", "$NOPE/devin", "", ReasonUndefinedVariable},
		{"undefined braced variable", "${NOPE}/devin", "", ReasonUndefinedVariable},
		{"unterminated brace", "${TOOLS/devin", "", ReasonUnterminatedBrace},
		{"empty braces", "${}/devin", "", ReasonInvalidVariableName},
		{"invalid braced name", "${1X}/devin", "", ReasonInvalidVariableName},
		{"relative result", "$REL/devin", "", ReasonNotAbsolute},
		{"other user home", "~bob/devin", "", ReasonOtherUserHome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.raw, env)
			check(t, tc.raw, got, err, tc.want, tc.reason)
		})
	}
}

func TestExpand_HomeUnavailable(t *testing.T) {
	env := testEnv("linux", "", nil)
	_, err := Expand("~/bin/tool", env)
	if fault.ReasonOf(err) != ReasonHomeUnavailable {
		t.Fatalf("reason = %q, want %q (err %v)", fault.ReasonOf(err), ReasonHomeUnavailable, err)
	}
}

func TestExpand_RefusalIsConfigFault(t *testing.T) {
	_, err := Expand("$NOPE/x", testEnv("linux", "/home/a", nil))
	if fault.CodeOf(err) != fault.Config {
		t.Fatalf("code = %v, want config", fault.CodeOf(err))
	}
}

func TestOS_UsesThePlatformGrammar(t *testing.T) {
	t.Setenv("AIMESH_PATHEXPAND_TEST_DIR", map[bool]string{true: `C:\probe`, false: "/probe"}[runtime.GOOS == "windows"])
	raw := "$AIMESH_PATHEXPAND_TEST_DIR/bin/tool"
	want := "/probe/bin/tool"
	if runtime.GOOS == "windows" {
		raw, want = `%AIMESH_PATHEXPAND_TEST_DIR%\bin\tool.exe`, `C:\probe\bin\tool.exe`
	}
	got, err := Expand(raw, OS())
	if err != nil || got != want {
		t.Fatalf("Expand(%q, OS()) = %q, %v; want %q", raw, got, err, want)
	}
}

func check(t *testing.T, raw, got string, err error, want, reason string) {
	t.Helper()
	if reason != "" {
		if err == nil {
			t.Fatalf("Expand(%q) = %q, want refusal %q", raw, got, reason)
		}
		if r := fault.ReasonOf(err); r != reason {
			t.Fatalf("Expand(%q) reason = %q, want %q (err %v)", raw, r, reason, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Expand(%q) error: %v", raw, err)
	}
	if got != want {
		t.Fatalf("Expand(%q) = %q, want %q", raw, got, want)
	}
}
