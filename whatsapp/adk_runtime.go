package whatsapp

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/agent"
	adklauncher "google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// runtime runs the agent in-process using the services already wired into the
// ADK launcher config, so a WhatsApp turn shares session, memory, and artifact
// state with every other surface the agent is launched on.
type runtime struct {
	cfg     *adklauncher.Config
	appName string
}

// newRuntime validates that the launcher config carries what a run needs.
func newRuntime(cfg *adklauncher.Config, appName string) (*runtime, error) {
	switch {
	case cfg == nil:
		return nil, fmt.Errorf("whatsapp: launcher config is required")
	case cfg.AgentLoader == nil:
		return nil, fmt.Errorf("whatsapp: launcher config has no AgentLoader")
	case cfg.SessionService == nil:
		return nil, fmt.Errorf("whatsapp: launcher config has no SessionService")
	}
	appName = strings.TrimSpace(appName)
	if appName == "" {
		return nil, fmt.Errorf("whatsapp: app name is required")
	}
	return &runtime{cfg: cfg, appName: appName}, nil
}

// runRequest is one agent turn.
type runRequest struct {
	// UserID is the ADK user the WhatsApp sender maps to.
	UserID string
	// SessionID is the session to continue. Empty mints a new one.
	SessionID string
	// Message is the user turn to append.
	Message *genai.Content
	// StateDelta merges into session state before the run. The launcher uses it
	// to publish the component catalog under [StateKey].
	StateDelta map[string]any
}

// run executes one turn and returns the session ID it ran against plus the event
// stream. Streaming is off: WhatsApp has no partial-message surface, so partial
// tokens would only be discarded.
func (rt *runtime) run(ctx context.Context, req runRequest) (string, iter.Seq2[*session.Event, error], error) {
	if req.UserID == "" {
		return "", nil, fmt.Errorf("whatsapp: user id is required")
	}
	if req.Message == nil || len(req.Message.Parts) == 0 {
		return "", nil, fmt.Errorf("whatsapp: message has no parts")
	}

	sessionID := req.SessionID
	if sessionID == "" {
		// Hyphens are stripped because Vertex AI memory bank rejects them in
		// session IDs. Matches what the upstream ADK launchers do.
		sessionID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}

	target, err := rt.cfg.AgentLoader.LoadAgent(rt.appName)
	if err != nil {
		return "", nil, fmt.Errorf("whatsapp: load agent %q: %w", rt.appName, err)
	}

	// A runner per turn matches the stock ADK REST server: it rebuilds the agent
	// tree and plugin manager, isolating concurrent runs from each other.
	r, err := runner.New(runner.Config{
		AppName:           rt.appName,
		Agent:             target,
		SessionService:    rt.cfg.SessionService,
		MemoryService:     rt.cfg.MemoryService,
		ArtifactService:   rt.cfg.ArtifactService,
		PluginConfig:      rt.cfg.PluginConfig,
		AutoCreateSession: true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("whatsapp: create runner: %w", err)
	}

	runCfg := agent.RunConfig{
		StreamingMode: agent.StreamingModeNone,
		// Inbound WhatsApp media arrives as inline bytes; persisting it as an
		// artifact is what lets later turns and other surfaces refer back to it.
		SaveInputBlobsAsArtifacts: true,
	}

	var opts []runner.RunOption
	if len(req.StateDelta) > 0 {
		opts = append(opts, runner.WithStateDelta(req.StateDelta))
	}
	return sessionID, r.Run(ctx, req.UserID, sessionID, req.Message, runCfg, opts...), nil
}
