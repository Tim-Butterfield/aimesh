package cliflags

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func styled(t *testing.T, command string, register func(fs *flag.FlagSet)) string {
	t.Helper()
	fs := flag.NewFlagSet("internal-name", flag.ContinueOnError)
	var b bytes.Buffer
	fs.SetOutput(&b)
	Style(fs, command)
	register(fs)
	fs.Usage()
	return b.String()
}

// TestStyle_UsesTheDoubleDashSpelling is the whole point: both forms parse, so the defect was that a
// reader could not tell which one the tool considers canonical. The hand-written usage text and every
// documented example write `--name`, so the generated help must too.
func TestStyle_UsesTheDoubleDashSpelling(t *testing.T) {
	out := styled(t, "tool verb", func(fs *flag.FlagSet) {
		fs.String("authority", "", "a document to judge against")
	})
	if !strings.Contains(out, "--authority") {
		t.Errorf("help does not use the double-dash spelling:\n%s", out)
	}
	if strings.Contains(out, "  -authority") {
		t.Errorf("help still emits the single-dash spelling:\n%s", out)
	}
}

// TestStyle_HeadsWithTheCommandAUserTypes. Go's default header prints the flag set's INTERNAL name,
// which here is not what anyone types — a command reached as three words cannot be documented as one.
func TestStyle_HeadsWithTheCommandAUserTypes(t *testing.T) {
	out := styled(t, "tool group verb <path>", func(fs *flag.FlagSet) {
		fs.Bool("json", false, "machine-readable output")
	})
	if !strings.HasPrefix(out, "usage: tool group verb <path> [flags]") {
		t.Errorf("header does not name the real command:\n%s", out)
	}
	if strings.Contains(out, "internal-name") {
		t.Errorf("header leaked the flag set's internal name:\n%s", out)
	}
}

// TestStyle_ABooleanIsNeverShownTakingAValue is the defect the new printer exposed and fixes.
//
// flag.UnquoteUsage reads a backquoted phrase ANYWHERE in a usage string as the value placeholder, so
// a boolean whose DESCRIPTION quotes a shell command was advertised as accepting that command as an
// argument. Nothing rejects the resulting invocation with a clear message — it is simply a value the
// flag does not take, printed as though it did.
func TestStyle_ABooleanIsNeverShownTakingAValue(t *testing.T) {
	out := styled(t, "tool verb", func(fs *flag.FlagSet) {
		fs.Bool("allow-dirty", false, "undoing ours (`git checkout -- .`) would discard yours too")
	})
	head := firstLineContaining(t, out, "--allow-dirty")
	if strings.TrimSpace(head) != "--allow-dirty" {
		t.Errorf("a boolean is advertised as taking a value: %q", head)
	}
	// The prose itself must survive — only the placeholder is dropped.
	if !strings.Contains(out, "would discard yours too") {
		t.Errorf("the description was lost:\n%s", out)
	}
}

// TestStyle_ANonBooleanKeepsItsPlaceholder: the fix must not strip value names from flags that DO
// take a value, which is the information a reader needs most.
func TestStyle_ANonBooleanKeepsItsPlaceholder(t *testing.T) {
	out := styled(t, "tool verb", func(fs *flag.FlagSet) {
		fs.String("changed-since", "", "a time `window` such as 2h")
	})
	head := firstLineContaining(t, out, "--changed-since")
	if !strings.Contains(head, "window") {
		t.Errorf("a value-taking flag lost its placeholder: %q", head)
	}
}

// TestStyle_StatesOnlyDefaultsWorthStating. "(default false)" on every boolean and "(default 0)" on
// every count is noise that trains a reader straight past the defaults that matter.
func TestStyle_StatesOnlyDefaultsWorthStating(t *testing.T) {
	out := styled(t, "tool verb", func(fs *flag.FlagSet) {
		fs.Bool("quiet", false, "say less")
		fs.Int("keep", 0, "how many to keep")
		fs.Int("seats", 3, "how many seats")
		fs.String("mode", "report", "the mode")
	})
	if strings.Contains(out, "default false") || strings.Contains(out, "default 0") {
		t.Errorf("zero-valued defaults are printed as noise:\n%s", out)
	}
	for _, want := range []string{"default 3", "default report"} {
		if !strings.Contains(out, want) {
			t.Errorf("a meaningful default is missing (%s):\n%s", want, out)
		}
	}
}

// TestStyle_WrapsWithoutBreakingWords: these descriptions are paragraphs, and a printer that split a
// path or a flag name across lines would make them unquotable.
func TestStyle_WrapsWithoutBreakingWords(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta epsilon ", 12)
	out := styled(t, "tool verb", func(fs *flag.FlagSet) {
		fs.String("x", "", long)
	})
	for _, line := range strings.Split(out, "\n") {
		if len(line) > wrapAt {
			t.Errorf("line exceeds the wrap width (%d): %q", len(line), line)
		}
	}
	// Every word survives intact.
	flat := strings.Join(strings.Fields(out), " ")
	if !strings.Contains(flat, "alpha beta gamma delta epsilon alpha") {
		t.Errorf("wrapping altered the text:\n%s", out)
	}
}

// TestStyle_NoFlagsPrintsNoEmptyHeading — a command with no flags gets a usage line and stops, not a
// "flags:" heading with nothing under it.
func TestStyle_NoFlagsPrintsNoEmptyHeading(t *testing.T) {
	out := styled(t, "tool verb", func(*flag.FlagSet) {})
	if strings.Contains(out, "flags:") {
		t.Errorf("a flagless command printed an empty flags section:\n%s", out)
	}
}

func firstLineContaining(t *testing.T, out, want string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, want) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no line containing %q in:\n%s", want, out)
	return ""
}
