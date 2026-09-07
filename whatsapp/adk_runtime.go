package whatsapp

import (
	"context"
	"fmt"
	"iter"

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

	// Validate the request.
	if req.UserID == "" {
		return "", nil, fmt.Errorf("whatsapp: user id is required")
	}
	if req.Message == nil || len(req.Message.Parts) == 0 {
		return "", nil, fmt.Errorf("whatsapp: message has no parts")
	}

	// Pick a session to run against. If the request has one, continue it; if not,
	// open a new one. The ADK launcher's REST server does this too, but we don't
	// have that here.
	sessionID := req.SessionID
	if sessionID == "" {
		var err error
		sessionID, err = createSession(ctx, rt.cfg.SessionService, rt.appName, req.UserID)
		if err != nil {
			return "", nil, err
		}
	}

	// Load the agent to run.
	// The launcher config has the AgentLoader, which knows how to load the agent by name.
	target, err := rt.cfg.AgentLoader.LoadAgent(rt.appName)
	if err != nil {
		return "", nil, fmt.Errorf("whatsapp: load agent %q: %w", rt.appName, err)
	}

	// A runner per turn matches the stock ADK REST server: it rebuilds the agent
	// tree and plugin manager, isolating concurrent runs from each other.
	r, err := runner.New(runner.Config{
		AppName:         rt.appName,
		Agent:           target,
		SessionService:  rt.cfg.SessionService,
		MemoryService:   rt.cfg.MemoryService,
		ArtifactService: rt.cfg.ArtifactService,
		PluginConfig:    rt.cfg.PluginConfig,
		// A new session is created above, so every ID reaching the runner is one
		// that exists. Auto-creation would instead create whatever ID it is
		// handed, turning a session that has since been deleted into a fresh
		// empty one rather than the not-found error it should be.
		AutoCreateSession: false,
	})
	if err != nil {
		return "", nil, fmt.Errorf("whatsapp: create runner: %w", err)
	}

	// Set up the run config.
	// Streaming is off: WhatsApp has no partial-message surface, so partial tokens would only be discarded.
	runCfg := agent.RunConfig{
		StreamingMode: agent.StreamingModeNone,
		// Inbound WhatsApp media arrives as inline bytes; persisting it as an
		// artifact is what lets later turns and other surfaces refer back to it.
		SaveInputBlobsAsArtifacts: true,
	}

	// Add component catalog to the session state via StateDelta. The launcher uses this to publish the component catalog under [StateKey].
	var opts []runner.RunOption
	if len(req.StateDelta) > 0 {
		opts = append(opts, runner.WithStateDelta(req.StateDelta))
	}

	// Run the agent turn.
	// The runner returns an event stream that includes the final session state.
	return sessionID, r.Run(ctx, req.UserID, sessionID, req.Message, runCfg, opts...), nil
}
