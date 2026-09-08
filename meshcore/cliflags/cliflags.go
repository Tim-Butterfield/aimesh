// Package cliflags renders a flag set's help the way the command line documents itself.
//
// THE PROBLEM IT SOLVES. Go's default printer emits `-name`, while hand-written usage text — and
// every example in this project's documentation — writes `--name`. Both forms parse identically, so
// nothing is broken; what is broken is that a reader cannot tell which spelling is canonical, and a
// tool that documents itself two ways in two places is asking its user to guess.
//
// The default header has the same problem one level up: `Usage of <flagset name>:` prints the
// INTERNAL name a flag set was constructed with, which is not necessarily what a user types to reach
// it. A command reached as three words cannot be documented as one.
//
// This package holds no vocabulary of its own: the caller supplies the exact command line, and
// everything else is read off the flag set.
package cliflags

import (
	"flag"
	"fmt"
	"strings"
)

// wrapAt is where a description line wraps. Wide enough for a long sentence, narrow enough to read
// in a default terminal beside its own indent.
const wrapAt = 92

// Style installs a usage printer on fs that writes `--name`, headed by the REAL command.
//
// command is what a user types (e.g. "tool group verb"), operands included if the command takes any.
// It is passed rather than derived because only the caller knows how its own dispatch spells it.
//
// Output goes to fs.Output(), so a caller that has already pointed the flag set at a stream does not
// have to say so twice — and a help request and a parse error land in the same place, which is the
// behaviour a script redirecting one of them expects of the other.
func Style(fs *flag.FlagSet, command string) {
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintf(w, "usage: %s [flags]\n", command)
		if !hasFlags(fs) {
			return
		}
		fmt.Fprintln(w, "\nflags:")
		fs.VisitAll(func(f *flag.Flag) {
			name, usage := flag.UnquoteUsage(f)
			head := "  --" + f.Name
			// A BOOLEAN TAKES NO VALUE, so it must never be shown with a placeholder. This is not
			// hypothetical tidiness: UnquoteUsage reads a backquoted phrase ANYWHERE in the usage
			// string as the value name, so a flag whose description quotes a shell command was being
			// advertised as `--allow-dirty git checkout -- .` — a value the flag does not accept,
			// printed as though it did. Go's own printer has the same behaviour; this is the one
			// place it can be corrected for every flag at once.
			if name != "" && !isBool(f) {
				head += " " + name
			}
			// A default worth stating is one a user could otherwise only discover by running the
			// command. Zero values are omitted: "(default false)" on every boolean is noise that
			// trains a reader past the defaults that matter.
			if d := strings.TrimSpace(f.DefValue); d != "" && d != "false" && d != "0" {
				head += fmt.Sprintf("  (default %s)", d)
			}
			fmt.Fprintln(w, head)
			for _, line := range wrap(usage, wrapAt-8) {
				fmt.Fprintln(w, "        "+line)
			}
		})
	}
}

// isBool reports whether a flag takes no value. `IsBoolFlag() bool` is the interface the flag package
// itself uses for this, so asking the value directly agrees with the parser rather than guessing from
// the default string (where a non-boolean flag defaulting to "false" would be misread).
func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func hasFlags(fs *flag.FlagSet) bool {
	found := false
	fs.VisitAll(func(*flag.Flag) { found = true })
	return found
}

// wrap breaks text into lines no longer than width, splitting only at existing spaces so a flag name
// or a path inside a description is never broken across lines. An empty description yields no lines
// at all rather than one blank one.
func wrap(text string, width int) []string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return nil
	}
	var (
		lines []string
		cur   strings.Builder
	)
	for _, word := range fields {
		switch {
		case cur.Len() == 0:
			cur.WriteString(word)
		case cur.Len()+1+len(word) <= width:
			cur.WriteString(" ")
			cur.WriteString(word)
		default:
			lines = append(lines, cur.String())
			cur.Reset()
			cur.WriteString(word)
		}
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}
