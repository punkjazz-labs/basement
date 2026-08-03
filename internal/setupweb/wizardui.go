package setupweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/punkjazz-labs/basement/internal/discovery"
	"github.com/punkjazz-labs/basement/internal/setup"
)

// Server implements setup.WizardUI: each blocking method publishes its
// question into the polled state and waits for the one matching POST
// /api/answer. Only one question is ever pending at a time, because the
// flow (internal/setup) only ever calls one WizardUI method at a time —
// same single-threaded assumption the terminal implementation relies on.

// ask publishes a question of the given kind (used both as the JSON "kind"
// tag the answer must carry and as the page's Phase) and blocks for the
// matching answer or for the flow's context to end.
func (s *Server) ask(kind string, apply func(*stateView)) (json.RawMessage, error) {
	s.mu.Lock()
	apply(&s.state)
	s.state.Phase = kind
	s.state.Seq++
	answer := make(chan json.RawMessage, 1)
	s.pending = &pendingAnswer{kind: kind, answer: answer}
	s.mu.Unlock()

	select {
	case raw := <-answer:
		return raw, nil
	case <-s.flowCtx.Done():
		return nil, s.flowCtx.Err()
	}
}

func (s *Server) Password(prompt string) (string, error) {
	raw, err := s.ask("password", func(st *stateView) { st.Prompt = prompt })
	if err != nil {
		return "", err
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errors.New("invalid password payload")
	}
	return body.Value, nil
}

func (s *Server) Confirm(prompt string) (bool, error) {
	raw, err := s.ask("confirm", func(st *stateView) { st.Prompt = prompt })
	if err != nil {
		return false, err
	}
	var body struct {
		Proceed bool `json:"proceed"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false, errors.New("invalid response")
	}
	return body.Proceed, nil
}

// ConfirmAlways is the same yes/no card as Confirm. The distinction the
// interface draws — a question a --yes flag may not answer — has no meaning
// in the browser wizard, which has no flags and always asks.
func (s *Server) ConfirmAlways(prompt string) (bool, error) {
	return s.Confirm(prompt)
}

func (s *Server) ChooseMachine(candidates []discovery.Candidate) (int, error) {
	views := make([]candidateView, len(candidates))
	for index, candidate := range candidates {
		views[index] = candidateView{
			Name:   setup.DisplayHost(candidate),
			IP:     candidate.IP.String(),
			Likely: discovery.LikelyGB10Name(candidate.Hostname),
		}
	}
	raw, err := s.ask("machines", func(st *stateView) { st.Candidates = views })
	if err != nil {
		return 0, err
	}
	var body struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, errors.New("invalid choice")
	}
	if body.Index < 0 || body.Index >= len(candidates) {
		return 0, errors.New("not a valid choice")
	}
	return body.Index, nil
}

func (s *Server) ConfirmNonGB10(name string) (bool, error) {
	raw, err := s.ask("nongb10", func(st *stateView) { st.Name = name })
	if err != nil {
		return false, err
	}
	var body struct {
		Proceed bool `json:"proceed"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false, errors.New("invalid response")
	}
	if !body.Proceed {
		return false, setup.ErrDeclined
	}
	return true, nil
}

func (s *Server) AskUsername(target, suggested string) (string, error) {
	raw, err := s.ask("username", func(st *stateView) { st.Target = target; st.Suggested = suggested })
	if err != nil {
		return "", err
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errors.New("invalid username")
	}
	if strings.TrimSpace(body.Username) == "" {
		return suggested, nil
	}
	return strings.TrimSpace(body.Username), nil
}

func (s *Server) ChooseListen(remote bool) (setup.ListenMode, error) {
	raw, err := s.ask("listen", func(st *stateView) { st.Remote = remote })
	if err != nil {
		return "", err
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errors.New("invalid listen choice")
	}
	switch body.Mode {
	case "loopback":
		return setup.ListenLoopback, nil
	case "tailscale":
		return setup.ListenTailscale, nil
	case "lan":
		return setup.ListenLAN, nil
	default:
		return "", fmt.Errorf("unknown listen mode %q", body.Mode)
	}
}

func (s *Server) Progress(line string) {
	s.mu.Lock()
	s.state.Progress = append(s.state.Progress, line)
	s.state.Phase = "progress"
	s.state.Seq++
	s.mu.Unlock()
}

// Summary appends one finished machine's card. A run that sets up two
// Sparks calls it twice and the page shows both.
func (s *Server) Summary(result setup.InstallResult) {
	s.mu.Lock()
	s.state.Phase = "summary"
	s.state.Summaries = append(s.state.Summaries, summaryView{
		ConsoleURL: result.ConsoleURL,
		AltURL:     result.AltURL,
		Token:      result.Token,
		Loopback:   result.Loopback,
	})
	s.state.Seq++
	s.mu.Unlock()
}

func (s *Server) NextSteps(lines []string) {
	s.mu.Lock()
	s.state.Phase = "summary"
	s.state.NextSteps = lines
	s.state.Seq++
	s.mu.Unlock()
}
