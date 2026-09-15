package setup

// Doctor repairs: RepairOptions maps each failing doctor check to guidance or an applyable repair, and
// ApplyRepair performs one selected, confirmed repair. Nothing applies automatically, and
// authentication, provider, model and trust changes stay guidance-only.

// RepairOption is one failing doctor check as a repair. Applyable is true only for an adapter
// binary-path issue (kind set_binary_path, which needs a path value); everything else is guidance.
type RepairOption struct {
	Check     string `json:"check"`
	Detail    string `json:"detail"`
	Message   string `json:"message"`
	Command   string `json:"command,omitempty"`
	Kind      string `json:"kind"` // "set_binary_path" | "guidance"
	Target    string `json:"target,omitempty"`
	Applyable bool   `json:"applyable"`
}
