package setup

// SaveReviewerPanel is the only write path for a profile's ordered blind reviewer panel. Adding,
// removing and reordering seats all write the whole list in one patch, so a rejected panel persists
// nothing.

// PanelSeatInput is one submitted seat: a configured adapter and a model-catalog key for it.
type PanelSeatInput struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
}

// SavePanelResult reports the outcome of SaveReviewerPanel.
type SavePanelResult struct {
	Written  bool
	Profile  string
	Seats    int
	Path     string
	Messages []string
}
