package cliflags

import (
	"flag"
	"strings"
)

// SplitArgs separates flag tokens (and the values of value-taking flags) from POSITIONAL
// arguments, so a command may be typed with its flags before or after its operand.
//
// THE PROBLEM IT SOLVES. Go's flag parser stops at the first non-flag token and treats everything
// after it as positional. So `cmd "some question" --criteria a,b` parses ZERO flags and silently
// drops `--criteria` — the user typed it, the help text documents it, and nothing says it was
// ignored. Meanwhile `cmd --path src .` has the opposite failure: `src` is taken for the operand
// and the real operand is lost. Both read to a user as the tool not working.
//
// Pre-splitting the argv fixes both: flags (with their values) go to fs.Parse, operands come back
// separately, and order stops mattering.
//
// WHICH FLAGS TAKE A VALUE IS ASKED OF THE FLAG SET, never listed by the caller. A hand-maintained
// list is a list that falls behind — and when it does, a flag's VALUE is misread as the operand and
// the parser blames the flag ("flag needs an argument"), which points at the wrong thing. Everything
// except a bool consumes the next token, and a bool announces itself through `IsBoolFlag()`, the
// same interface Style reads to decide whether to render a value placeholder.
//
// A token this flag set does not define is assumed to take NO value, so fs.Parse reports it by name
// rather than silently swallowing whatever followed it.
func SplitArgs(fs *flag.FlagSet, args []string) (flags, positionals []string) {
	takesValue := func(tok string) bool {
		f := fs.Lookup(strings.TrimLeft(tok, "-"))
		if f == nil {
			return false
		}
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		return !ok || !b.IsBoolFlag()
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		// "--" is the conventional end-of-flags marker: everything after it is an operand, even if
		// it looks like a flag. Honouring it is what lets an operand legitimately begin with a dash.
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			return flags, positionals
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && takesValue(a) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return flags, positionals
}
