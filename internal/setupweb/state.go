package setupweb

// stateView is the JSON snapshot the page polls for. It carries every
// field any phase might need; the client keys off Phase to decide which
// fields apply and ignores the rest. Progress accumulates for the whole
// run and is never cleared.
type stateView struct {
	Seq        int             `json:"seq"`
	Phase      string          `json:"phase"`
	Candidates []candidateView `json:"candidates,omitempty"`
	Name       string          `json:"name,omitempty"`      // ConfirmNonGB10: the hostname that did not look like a GB10
	Target     string          `json:"target,omitempty"`    // AskUsername: the machine being connected to
	Suggested  string          `json:"suggested,omitempty"` // AskUsername: the pre-filled default
	Prompt     string          `json:"prompt,omitempty"`    // Password/Confirm: the exact prompt text (SSH may ask more than once)
	Remote     bool            `json:"remote,omitempty"`    // ChooseListen: whether the target differs from this machine
	Progress   []string        `json:"progress"`
	Summary    *summaryView    `json:"summary,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// candidateView renders one discovery.Candidate for the page: display name,
// address, and whether its hostname looks GB10-class (badge + dimming).
type candidateView struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Likely bool   `json:"likely"`
}

// summaryView renders a completed install's setup.InstallResult.
type summaryView struct {
	ConsoleURL string `json:"consoleUrl"`
	AltURL     string `json:"altUrl,omitempty"`
	Token      string `json:"token,omitempty"`
	Loopback   bool   `json:"loopback"`
}
