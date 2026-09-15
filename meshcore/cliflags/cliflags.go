// Package cliflags renders a flag set's help the way the command line documents itself: flags as
// `--name` rather than Go's default `-name`, under a header naming the command a user types rather than
// the flag set's internal name. The caller supplies the command; everything else is read from the flag
// set.
package cliflags

import (
	"flag"
	"fmt"
	"strings"
)

// wrapAt is where a description line wraps. Wide enough for a long sentence, narrow enough to read
// in a default terminal beside its own indent.
const wrapAt = 92

// Style installs a usage printer on fs that writes `--name` flags under command, what a user types
// (such as "tool group verb"), operands included. Output goes to fs.Output(), so help and parse errors
// share a stream.
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
			// A boolean takes no value, so it never gets a placeholder. UnquoteUsage treats any
			// backquoted phrase in the usage as the value name, which would mislabel a boolean.
			if name != "" && !isBool(f) {
				head += " " + name
			}
			// Zero-value defaults are omitted as noise.
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
