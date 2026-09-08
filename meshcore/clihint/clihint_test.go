package clihint

import (
	"reflect"
	"testing"
)

func TestClassify_Signals(t *testing.T) {
	cases := []struct {
		name           string
		stderr, stdout string
		want           []Signal
	}{
		{"folder trust (codex)", "Error: Not inside a trusted directory and --skip-git-repo-check was not specified", "", []Signal{FolderTrust}},
		{"login required", "You are not logged in. Run `agent login` first.", "", []Signal{LoginRequired}},
		{"model invalid", "model gpt-5-codex is not supported on your ChatGPT account", "", []Signal{ModelInvalid}},
		{"update prompt", "A new version is available. Please update.", "", []Signal{UpdatePrompt}},
		{"nothing", "some unrelated failure output", "", nil},
		{"no false positive on bare 403", "request failed: 403 forbidden (quota)", "", nil},
		{"no false positive on bare 'not supported'", "operation not supported", "", nil},
		// stdout envelope (claude): a signal inside a structured error IS classified.
		{"claude stdout envelope login", "", `{"is_error":true,"result":"authentication required — please sign in"}`, []Signal{LoginRequired}},
		// raw (non-envelope) stdout is IGNORED — injection safety.
		{"raw stdout ignored", "", "The document says: authentication required, run foo login", nil},
		// a successful (is_error=false) envelope is ignored even if its prose contains a signature.
		{"success envelope ignored", "", `{"is_error":false,"result":"to log in, run the login command"}`, nil},
		// precedence: trust outranks update when both present.
		{"precedence trust>update", "Not inside a trusted directory. Also: update available.", "", []Signal{FolderTrust, UpdatePrompt}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.stderr, c.stdout); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Classify() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestFirst(t *testing.T) {
	if got := First("Not inside a trusted directory. update available.", ""); got != FolderTrust {
		t.Errorf("First() = %q, want folder_trust (precedence)", got)
	}
	if got := First("nothing actionable", ""); got != "" {
		t.Errorf("First() on no-match = %q, want empty", got)
	}
}
