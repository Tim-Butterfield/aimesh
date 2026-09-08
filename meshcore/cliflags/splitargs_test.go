package cliflags

import (
	"flag"
	"io"
	"slices"
	"testing"
)

type repeatable []string

func (r *repeatable) String() string     { return "" }
func (r *repeatable) Set(v string) error { *r = append(*r, v); return nil }

func fixture(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("cmd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Bool("report", false, "")
	fs.String("mode", "", "")
	fs.Int("count", 0, "")
	var rep repeatable
	fs.Var(&rep, "path", "")
	return fs
}

// TestSplitArgs_AnOperandDoesNotDisarmTheFlagsAfterIt is the failure this exists for. Go's parser
// stops at the first non-flag token, so a command documented as taking an operand silently ignored
// every flag typed after it — and then refused the run for missing the very thing that was typed.
func TestSplitArgs_AnOperandDoesNotDisarmTheFlagsAfterIt(t *testing.T) {
	fs := fixture(t)
	flags, pos := SplitArgs(fs, []string{"a question", "--mode", "map", "--report"})
	if !slices.Equal(pos, []string{"a question"}) {
		t.Fatalf("positionals = %v, want the operand alone", pos)
	}
	if !slices.Equal(flags, []string{"--mode", "map", "--report"}) {
		t.Fatalf("flags = %v — a flag after the operand was dropped", flags)
	}
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := fs.Lookup("mode").Value.String(); got != "map" {
		t.Errorf("mode = %q, want map", got)
	}
}

// TestSplitArgs_AFlagValueIsNotMistakenForTheOperand is the mirror failure: the value was taken for
// the operand, the real operand was lost, and the parser blamed the flag.
func TestSplitArgs_AFlagValueIsNotMistakenForTheOperand(t *testing.T) {
	fs := fixture(t)
	flags, pos := SplitArgs(fs, []string{"--path", "src", "."})
	if !slices.Equal(pos, []string{"."}) {
		t.Fatalf("positionals = %v, want the operand — the flag's value was taken for it", pos)
	}
	if !slices.Equal(flags, []string{"--path", "src"}) {
		t.Fatalf("flags = %v, want the flag and its value kept together", flags)
	}
}

// TestSplitArgs_BoolsAndInlineValuesKeepTheirHands off the next token.
func TestSplitArgs_BoolsAndInlineValuesKeepTheirHands(t *testing.T) {
	fs := fixture(t)
	if _, pos := SplitArgs(fs, []string{"--report", "."}); !slices.Equal(pos, []string{"."}) {
		t.Errorf("a bool swallowed the operand: %v", pos)
	}
	if _, pos := SplitArgs(fs, []string{"--mode=map", "."}); !slices.Equal(pos, []string{"."}) {
		t.Errorf("--flag=value swallowed the operand: %v", pos)
	}
}

// TestSplitArgs_UnknownFlagIsLeftForTheParser. Assuming an unknown flag takes a value would swallow
// a real operand behind a typo; leaving it lets fs.Parse name the flag the user got wrong.
func TestSplitArgs_UnknownFlagIsLeftForTheParser(t *testing.T) {
	fs := fixture(t)
	flags, pos := SplitArgs(fs, []string{"--nope", "."})
	if !slices.Equal(pos, []string{"."}) {
		t.Errorf("a typo'd flag swallowed the operand: %v", pos)
	}
	if err := fs.Parse(flags); err == nil {
		t.Error("an unknown flag must still be reported by the parser")
	}
}

// TestSplitArgs_EndOfFlagsMarker lets an operand legitimately begin with a dash.
func TestSplitArgs_EndOfFlagsMarker(t *testing.T) {
	fs := fixture(t)
	flags, pos := SplitArgs(fs, []string{"--report", "--", "--not-a-flag"})
	if !slices.Equal(flags, []string{"--report"}) {
		t.Errorf("flags = %v", flags)
	}
	if !slices.Equal(pos, []string{"--not-a-flag"}) {
		t.Errorf("positionals = %v, want everything after -- treated as an operand", pos)
	}
}
