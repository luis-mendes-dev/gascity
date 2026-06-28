package runtime

import (
	"context"
	"strconv"
	"time"
)

// Carrier drives the high-level session interactions — input delivery, output
// capture, interrupt, scrollback — over a connection to the box, separating
// HOW an interaction is realized from the connection that reaches the box. Its
// scope is exactly those four driving verbs; liveness, attachment, activity,
// and metadata stay provider-specific, and higher-level orchestration
// (startup-dialog acceptance, no-wait nudge, wait-for-idle) is composed ABOVE a
// Carrier out of Peek/SendKeys, not added to it.
//
// Every op returns the underlying transport error verbatim. Whether a failure
// is fatal or best-effort is the PROVIDER facade's policy: a provider that is
// best-effort today (e.g. Kubernetes swallows a missing pod and ignores exec
// failures) must keep discarding the error when it delegates here — the Carrier
// itself never swallows.
//
// The tmux carrier ([NewTmuxCarrier]) realizes these verbs by issuing tmux
// commands over an [ExecProvider]. It is the shared driver for tmux-in-a-box
// exec-connection runtimes (Kubernetes, the SSH backend, and packs that opt
// into the tmux-box model). Such a provider must expose a name-keyed
// [ExecProvider] and owns name->box and name->target resolution plus any
// best-effort swallowing in its own adapter — the carrier knows nothing about
// pods or how the box is reached. Adopting it for a runtime that drives input
// via its own dedicated ops (e.g. an exec pack's nudge/peek subcommands) is a
// protocol change, not a silent migration. The local tmux control driver and
// the ACP stream driver realize these verbs differently and keep their own
// behavior.
type Carrier interface {
	// Nudge delivers content as input to the session, followed by a submit.
	Nudge(ctx context.Context, name string, content []ContentBlock) error
	// SendKeys sends bare keystrokes (e.g. "Enter", "C-c") without a submit.
	SendKeys(ctx context.Context, name string, keys ...string) error
	// Peek captures the last lines of output (all scrollback when lines <= 0).
	Peek(ctx context.Context, name string, lines int) (string, error)
	// Interrupt sends a soft interrupt (Ctrl-C) to the session.
	Interrupt(ctx context.Context, name string) error
	// ClearScrollback clears the session's scrollback history.
	ClearScrollback(ctx context.Context, name string) error
}

// tmuxCarrier drives a tmux session living inside the box by issuing tmux
// commands over an [ExecProvider] connection. target is the in-box tmux session
// the commands address (e.g. "main"); it is fixed per carrier today — a
// name->target resolver is the natural extension if one connection ever
// multiplexes sessions on distinct targets. The mapping mirrors the tmux
// commands the Kubernetes provider issues over execInPod today, so once k8s
// exposes an [ExecProvider], delegating its driving methods here is
// argv-for-argv behavior-preserving (the provider keeps its own best-effort
// error swallowing; see [Carrier]).
type tmuxCarrier struct {
	conn   ExecProvider
	target string
}

// NewTmuxCarrier returns a [Carrier] that drives the in-box tmux session
// target over conn.
func NewTmuxCarrier(conn ExecProvider, target string) Carrier {
	return &tmuxCarrier{conn: conn, target: target}
}

// tmux runs `tmux <args...>` in the box over the connection and returns its
// standard output. A non-zero command exit is reported via err only when the
// connection itself surfaces it; callers that are best-effort discard err.
func (c *tmuxCarrier) tmux(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, _, err := c.conn.Exec(ctx, name, append([]string{"tmux"}, args...))
	return out, err
}

func (c *tmuxCarrier) Nudge(ctx context.Context, name string, content []ContentBlock) error {
	message := FlattenText(content)
	if message == "" {
		return nil
	}
	// The box runs the agent TUI in a tmux pane that is typically DETACHED (no
	// client attached — the norm for k8s pods and pooled SSH boxes). A detached
	// TUI may not service its render/input loop until a terminal event occurs, so
	// a bare `send-keys -l <text>` + `send-keys Enter` is delivered by tmux but
	// silently dropped at the application layer — leaving a half-typed, UNSUBMITTED
	// line in the pane (the agent then idles on its own queued command, e.g. an
	// un-run `gc hook`). Mirror the local tmux NudgeSession reliability: fire a
	// SIGWINCH resize-dance to wake the pane before the type AND before the submit,
	// debounce so the paste settles, and retry the Enter — the submit is the step
	// that most often fails on a detached pane. resize -1 then +1 is net-zero, so
	// it is harmless for attached panes too.
	wake := func() {
		_, _ = c.tmux(ctx, name, "resize-pane", "-t", c.target, "-y", "-1")
		time.Sleep(50 * time.Millisecond)
		_, _ = c.tmux(ctx, name, "resize-pane", "-t", c.target, "-y", "+1")
	}

	wake()
	// Type the literal text (a single send-keys would interpret it as key names).
	if _, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "-l", message); err != nil {
		return err
	}
	// Let the paste settle, then wake again so the pane is servicing input when
	// Enter arrives.
	time.Sleep(300 * time.Millisecond)
	wake()
	// Send Enter with retry — the critical submit step on a detached pane. Only
	// retries on a transport error; a successful Enter returns immediately (no
	// double-submit).
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		if _, err = c.tmux(ctx, name, "send-keys", "-t", c.target, "Enter"); err == nil {
			wake() // process the submitted turn promptly
			return nil
		}
	}
	return err
}

func (c *tmuxCarrier) SendKeys(ctx context.Context, name string, keys ...string) error {
	// k8s issues a bare no-op `send-keys -t <target>` for empty keys; we
	// short-circuit to issue nothing (behaviorally identical — no keystrokes).
	if len(keys) == 0 {
		return nil
	}
	_, err := c.tmux(ctx, name, append([]string{"send-keys", "-t", c.target}, keys...)...)
	return err
}

func (c *tmuxCarrier) Peek(ctx context.Context, name string, lines int) (string, error) {
	start := "-"
	if lines > 0 {
		start = "-" + strconv.Itoa(lines)
	}
	out, err := c.tmux(ctx, name, "capture-pane", "-t", c.target, "-p", "-S", start)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *tmuxCarrier) Interrupt(ctx context.Context, name string) error {
	_, err := c.tmux(ctx, name, "send-keys", "-t", c.target, "C-c")
	return err
}

func (c *tmuxCarrier) ClearScrollback(ctx context.Context, name string) error {
	_, err := c.tmux(ctx, name, "clear-history", "-t", c.target)
	return err
}
