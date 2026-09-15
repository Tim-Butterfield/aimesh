// Package pathexpand expands environment variables and a leading home-directory marker in a
// filesystem path an operator wrote into a launch configuration, using each operating system's own
// syntax.
//
// The syntax follows the platform because the people writing these paths already know it, and a
// path copied from that platform's documentation or file browser works unchanged:
//
//   - Windows: `%NAME%` expands; `%%` is a literal `%`; any other `%` is refused. `$` is always
//     literal, so an administrative share such as `\\server\C$\tools` is untouched.
//   - macOS/Linux: `$NAME` and `${NAME}` expand; `$$` is a literal `$`; a `$` not followed by a name,
//     `{` or `$` is literal; an unterminated `${` is refused. `%` is always literal.
//   - Every platform: a leading `~` on its own, or followed by a path separator, is the user's home
//     directory. `~name` (another user's home) is refused rather than guessed.
//
// An undefined variable is refused, never expanded to an empty string: `%LOCALAPPDATA%\tool.exe`
// with the variable unset would otherwise become `\tool.exe`, a different file. The expanded result
// must be absolute, so the meaning of the path never depends on the working directory of whatever
// process launched this one.
package pathexpand

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Reason codes carried by every refusal, so a caller can branch without parsing the message.
const (
	ReasonEmpty               = "path_expand_empty"
	ReasonUndefinedVariable   = "path_expand_undefined_var"
	ReasonUnmatchedPercent    = "path_expand_unmatched_percent"
	ReasonUnterminatedBrace   = "path_expand_unterminated_brace"
	ReasonInvalidVariableName = "path_expand_invalid_var_name"
	ReasonOtherUserHome       = "path_expand_user_home"
	ReasonHomeUnavailable     = "path_expand_home_unavailable"
	ReasonNotAbsolute         = "path_expand_not_absolute"
)

// Env supplies everything expansion reads from the process, so both grammars are testable on any
// host.
type Env struct {
	// GOOS selects the grammar: "windows" uses the Windows rules, anything else the Unix rules.
	GOOS string
	// Lookup returns a variable's value and whether it is defined.
	Lookup func(name string) (string, bool)
	// Home returns the user's home directory.
	Home func() (string, error)
}

// OS is the Env of the running process.
func OS() Env {
	return Env{GOOS: runtime.GOOS, Lookup: os.LookupEnv, Home: os.UserHomeDir}
}

// Expand returns raw with its home marker and variables expanded under env's grammar. The result is
// absolute or the call fails.
func Expand(raw string, env Env) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", refuse(ReasonEmpty, "the path is empty")
	}
	windows := env.GOOS == "windows"

	prefix, rest, err := expandHome(s, windows, env)
	if err != nil {
		return "", err
	}
	var body string
	if windows {
		body, err = expandWindows(rest, len(s)-len(rest), env)
	} else {
		body, err = expandUnix(rest, len(s)-len(rest), env)
	}
	if err != nil {
		return "", err
	}
	out := prefix + body
	if !isAbs(out, windows) {
		return "", refuse(ReasonNotAbsolute, fmt.Sprintf("%q expands to %q, which is not an absolute path; write the full path, or start it with a variable or ~ that supplies one", raw, out))
	}
	return out, nil
}

// expandHome replaces a leading `~` marker with the home directory. It returns the replacement and
// the remainder still to be scanned for variables; the home directory itself is never re-scanned.
func expandHome(s string, windows bool, env Env) (prefix, rest string, err error) {
	if !strings.HasPrefix(s, "~") {
		return "", s, nil
	}
	after := s[1:]
	if after != "" && !isSeparator(after[0], windows) {
		return "", "", refuse(ReasonOtherUserHome, fmt.Sprintf("%q names another user's home directory; only a leading ~ on its own or followed by a path separator is expanded", s))
	}
	if env.Home == nil {
		return "", "", refuse(ReasonHomeUnavailable, "the home directory cannot be determined")
	}
	home, herr := env.Home()
	if herr != nil || strings.TrimSpace(home) == "" {
		return "", "", refuse(ReasonHomeUnavailable, fmt.Sprintf("the home directory cannot be determined for %q", s))
	}
	home = strings.TrimRight(home, `/\`)
	if home == "" {
		home = "/"
	}
	return home, after, nil
}

// expandWindows applies the `%NAME%` grammar. offset is rest's position in the original input, so an
// error names the position the operator wrote.
func expandWindows(rest string, offset int, env Env) (string, error) {
	var b strings.Builder
	for i := 0; i < len(rest); {
		c := rest[i]
		if c != '%' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(rest) && rest[i+1] == '%' {
			b.WriteByte('%')
			i += 2
			continue
		}
		end := strings.IndexByte(rest[i+1:], '%')
		if end < 0 {
			return "", refuse(ReasonUnmatchedPercent, fmt.Sprintf("the %% at position %d has no closing %%; write %%%% for a literal percent sign", offset+i+1))
		}
		name := rest[i+1 : i+1+end]
		val, err := lookup(name, env)
		if err != nil {
			return "", err
		}
		b.WriteString(val)
		i += end + 2
	}
	return b.String(), nil
}

// expandUnix applies the `$NAME` / `${NAME}` grammar.
func expandUnix(rest string, offset int, env Env) (string, error) {
	var b strings.Builder
	for i := 0; i < len(rest); {
		c := rest[i]
		if c != '$' || i+1 >= len(rest) {
			b.WriteByte(c)
			i++
			continue
		}
		next := rest[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == '{':
			end := strings.IndexByte(rest[i+2:], '}')
			if end < 0 {
				return "", refuse(ReasonUnterminatedBrace, fmt.Sprintf("the ${ at position %d has no closing }", offset+i+1))
			}
			name := rest[i+2 : i+2+end]
			if !validUnixName(name) {
				return "", refuse(ReasonInvalidVariableName, fmt.Sprintf("${%s} at position %d is not a valid variable name (letters, digits and _, not starting with a digit)", name, offset+i+1))
			}
			val, err := lookup(name, env)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i += end + 3
		case isNameStart(next):
			j := i + 2
			for j < len(rest) && isNameChar(rest[j]) {
				j++
			}
			val, err := lookup(rest[i+1:j], env)
			if err != nil {
				return "", err
			}
			b.WriteString(val)
			i = j
		default:
			b.WriteByte('$')
			i++
		}
	}
	return b.String(), nil
}

func lookup(name string, env Env) (string, error) {
	if env.Lookup != nil {
		if v, ok := env.Lookup(name); ok {
			return v, nil
		}
	}
	return "", refuse(ReasonUndefinedVariable, fmt.Sprintf("environment variable %s is not defined, so the path cannot be expanded; set it in the environment that launches this process, or write the full path", name))
}

func isAbs(p string, windows bool) bool {
	if !windows {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true
	}
	return len(p) >= 3 && isLetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isSeparator(c byte, windows bool) bool {
	return c == '/' || (windows && c == '\\')
}

func validUnixName(name string) bool {
	if name == "" || !isNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isNameChar(name[i]) {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool    { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isNameStart(c byte) bool { return isLetter(c) || c == '_' }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }

func refuse(reason, msg string) error {
	return fault.New(fault.Config, msg).WithReason(reason)
}
