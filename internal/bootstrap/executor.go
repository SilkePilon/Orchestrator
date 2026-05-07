package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Executor runs a Plan against a set of pre-dialled SSH clients,
// publishing every line of output and every lifecycle change as an Event
// on its public channel. It is intended to be driven by a single goroutine
// per call to Run.
type Executor struct {
	plan    *Plan
	clients map[string]*Client // keyed by Node.ID
	events  chan Event

	mu sync.Mutex
	// nodeToken is captured from the server's "read-token" step and made
	// available to agent install commands via TokenPlaceholder.
	nodeToken string
	// kubeconfig is captured from the server's "read-kubeconfig" step.
	kubeconfigYAML string
}

// NewExecutor wires the executor to a plan and a pre-built client map.
// The caller owns the clients and is responsible for closing them after
// Run returns.
func NewExecutor(plan *Plan, clients map[string]*Client) *Executor {
	return &Executor{
		plan:    plan,
		clients: clients,
		events:  make(chan Event, 64),
	}
}

// Events returns the event channel. The channel is closed when Run
// returns.
func (e *Executor) Events() <-chan Event { return e.events }

// NodeToken returns the cluster node-token captured from the server.
// Empty until the corresponding step has run.
func (e *Executor) NodeToken() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodeToken
}

// KubeconfigYAML returns the raw kubeconfig captured from the server.
// Empty until the corresponding step has run.
func (e *Executor) KubeconfigYAML() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.kubeconfigYAML
}

// Run executes the plan in node order. For each node, steps are run in
// order, stopping at the first failure unless the step has Skip=true. If
// a step requires the previously-captured node-token (TokenPlaceholder),
// it is substituted just-in-time so the user sees the exact command in
// the apply log.
//
// Run always closes Events() before returning. The returned error is
// non-nil only on transport failures (the executor surfaces step
// failures via Event.Status / Event.Err and continues to the next node
// only if the user already chose to skip on failure — by default it
// stops).
func (e *Executor) Run(ctx context.Context) error {
	defer close(e.events)

	for _, nodeID := range e.plan.NodeOrder {
		client, ok := e.clients[nodeID]
		if !ok {
			e.emit(Event{NodeID: nodeID, Kind: "log",
				Line: "no SSH client for this node — skipping"})
			continue
		}
		steps := e.plan.NodeSteps[nodeID]
		for i := range steps {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			st := steps[i]
			if st.Skip {
				e.emit(Event{NodeID: nodeID, StepID: st.ID, Kind: "step.start",
					When: time.Now()})
				e.emit(Event{NodeID: nodeID, StepID: st.ID, Kind: "log",
					Line: "skipped: " + st.SkipReason})
				e.emit(Event{NodeID: nodeID, StepID: st.ID, Kind: "step.end",
					Status: StatusSkipped, When: time.Now()})
				continue
			}

			cmd := e.prepareCommand(st, client.Node())
			status, code, err := e.runStep(ctx, client, st, cmd)
			if status == StatusFailed || err != nil {
				e.emit(Event{NodeID: nodeID, StepID: st.ID, Kind: "step.end",
					Status: StatusFailed, ExitCode: code, Err: err, When: time.Now()})
				return nil // step failures are reported via events; not transport errors
			}
			e.emit(Event{NodeID: nodeID, StepID: st.ID, Kind: "step.end",
				Status: status, ExitCode: code, When: time.Now()})
		}
	}
	return nil
}

// prepareCommand applies privilege escalation and runtime substitutions
// (e.g. the node-token) to the raw step command. The returned string is
// what is actually sent over SSH and what is shown to the user in the
// apply log.
func (e *Executor) prepareCommand(st Step, n Node) string {
	cmd := st.Command
	if strings.Contains(cmd, TokenPlaceholder) {
		cmd = strings.ReplaceAll(cmd, TokenPlaceholder, e.NodeToken())
	}

	if !st.RequiresRoot {
		return cmd
	}
	switch n.Become {
	case BecomeNone:
		return cmd
	case BecomeSudo:
		// -S reads the password from stdin if needed; we feed it via
		// printf below when a password is set. -E preserves env so the
		// installer's K3S_* vars survive.
		if n.BecomePassword != "" {
			return fmt.Sprintf("printf '%%s\\n' %s | sudo -S -E -p '' bash -c %s",
				shellQuote(n.BecomePassword), shellQuote(cmd))
		}
		return fmt.Sprintf("sudo -E bash -c %s", shellQuote(cmd))
	case BecomeSu:
		return fmt.Sprintf("su -c %s", shellQuote(cmd))
	default:
		return cmd
	}
}

func (e *Executor) runStep(ctx context.Context, client *Client, st Step, cmd string) (StepStatus, int, error) {
	e.emit(Event{NodeID: client.Node().ID, StepID: st.ID, Kind: "step.start", When: time.Now()})
	e.emit(Event{NodeID: client.Node().ID, StepID: st.ID, Kind: "log",
		Line: "$ " + summariseCommand(cmd)})

	// For steps that capture output (token / kubeconfig), use Run so we
	// have the full stdout. Stream lines for the UI as well.
	captureToken := strings.HasPrefix(st.ID, "read-token-")
	captureKubeconfig := strings.HasPrefix(st.ID, "read-kubeconfig-")

	if captureToken || captureKubeconfig {
		stdout, stderr, code, err := client.Run(ctx, cmd)
		// Mirror line-buffered output to the event stream so the user
		// still sees what happened.
		for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
			if line == "" {
				continue
			}
			e.emit(Event{NodeID: client.Node().ID, StepID: st.ID, Kind: "stdout", Line: line})
		}
		for _, line := range strings.Split(strings.TrimRight(stderr, "\n"), "\n") {
			if line == "" {
				continue
			}
			e.emit(Event{NodeID: client.Node().ID, StepID: st.ID, Kind: "stderr", Line: line})
		}
		if err != nil {
			return StatusFailed, code, err
		}
		if code != 0 {
			return StatusFailed, code, fmt.Errorf("exit %d", code)
		}
		e.mu.Lock()
		if captureToken {
			e.nodeToken = strings.TrimSpace(stdout)
		}
		if captureKubeconfig {
			e.kubeconfigYAML = stdout
		}
		e.mu.Unlock()
		return StatusDone, 0, nil
	}

	code, err := client.Stream(ctx, cmd, func(stream, line string) {
		e.emit(Event{NodeID: client.Node().ID, StepID: st.ID, Kind: stream, Line: line})
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return StatusCanceled, code, err
		}
		return StatusFailed, code, err
	}
	if code != 0 {
		return StatusFailed, code, fmt.Errorf("exit %d", code)
	}
	return StatusDone, 0, nil
}

func (e *Executor) emit(ev Event) {
	if ev.When.IsZero() {
		ev.When = time.Now()
	}
	// Best-effort send — we never want a slow consumer to block the
	// remote command. The buffer sizes the channel for typical install
	// log volume.
	select {
	case e.events <- ev:
	default:
		// Channel full — drop the line. The UI will see a gap, which is
		// acceptable; we'd rather drop than stall an SSH stream.
	}
}

// summariseCommand makes very long commands readable in the event log
// by replacing heredoc bodies with their byte length.
func summariseCommand(cmd string) string {
	if i := strings.Index(cmd, "<<'EOF_SEABIRD'"); i >= 0 {
		j := strings.LastIndex(cmd, "EOF_SEABIRD")
		if j > i {
			body := cmd[i+len("<<'EOF_SEABIRD'") : j]
			return cmd[:i] + fmt.Sprintf("<<'EOF_SEABIRD' (%d bytes) EOF_SEABIRD", len(strings.TrimSpace(body)))
		}
	}
	if len(cmd) > 240 {
		return cmd[:200] + "… (" + fmt.Sprintf("%d", len(cmd)) + " bytes)"
	}
	return cmd
}
