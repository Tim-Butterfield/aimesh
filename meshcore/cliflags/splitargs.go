package cliflags

import (
	"flag"
	"strings"
)

// SplitArgs separates flag tokens (with the values of value-taking flags) from positional arguments,
// so flags may come before or after an operand. Go's flag parser stops at the first non-flag token,
// which would silently ignore later flags.
//
// Whether a flag takes a value is read from the flag set: every flag except a boolean (IsBoolFlag)
// consumes the next token. An undefined flag is assumed to take no value, so fs.Parse reports it by
// name.
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
