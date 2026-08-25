package whatsapp

import (
	"encoding/json"
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// StateKey is the session state key under which the launcher publishes the
// component [Catalog] on every run. [Toolset] reads this key at invocation time.
//
// The coupling is deliberate and the constant is exported so both sides
// reference the same value.
//
// The temp: prefix is load-bearing. The catalog is per-invocation configuration,
// not conversation history: ADK applies a temp: key to the live session state the
// toolset reads, then strips it before persisting. Without the prefix every
// WhatsApp session would carry a copy of the catalog in storage forever.
const StateKey = session.KeyPrefixTemp + "_whatsapp_components"

// Toolset exposes the launcher's component [Catalog] to the model as tools.
// Add it to the agent that this launcher serves:
//
//	llmagent.New(llmagent.Config{
//	    Name:     "my_agent",
//	    Toolsets: []tool.Toolset{whatsapp.NewToolset()},
//	})
//
// When the catalog is absent from state — the agent was reached over AG-UI, the
// console, or a cron rather than WhatsApp — the toolset returns zero tools and
// leaves the agent's own tools untouched.
type Toolset struct{}

// NewToolset returns a [Toolset] ready for an agent's Toolsets config.
func NewToolset() *Toolset { return &Toolset{} }

// Name implements [tool.Toolset].
func (ts *Toolset) Name() string { return "whatsapp-components" }

// Tools implements [tool.Toolset], returning one tool per catalog entry.
func (ts *Toolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	raw, err := ctx.ReadonlyState().Get(StateKey)
	if err != nil {
		if err == session.ErrStateKeyNotExist {
			return nil, nil
		}
		return nil, fmt.Errorf("whatsapp: read state %q: %w", StateKey, err)
	}

	catalog, err := decodeCatalog(raw)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: decode catalog: %w", err)
	}

	tools := make([]tool.Tool, 0, len(catalog))
	for i := range catalog {
		comp := &catalog[i]
		tools = append(tools, &componentTool{component: comp, schema: comp.schema()})
	}
	if len(tools) == 0 {
		return nil, nil
	}
	return tools, nil
}

// decodeCatalog reads the resolved catalog from a state value, which may arrive
// as JSON bytes or as an already-decoded structure depending on the session
// backend.
func decodeCatalog(raw any) (resolvedCatalog, error) {
	var data []byte
	switch v := raw.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			return nil, fmt.Errorf("marshal state value: %w", err)
		}
	}
	var catalog resolvedCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("unmarshal catalog: %w", err)
	}
	return catalog, nil
}

// componentTool is a long-running tool standing in for one WhatsApp component.
// Calling it does nothing in-process: the launcher watches the event stream,
// picks the function call up by name, and renders it to Twilio. Delivery is the
// launcher's job, so the model must not wait on a result.
type componentTool struct {
	component *Resolved
	schema    map[string]any
}

// Name implements [tool.Tool]. The component ID is the tool name.
func (t *componentTool) Name() string { return t.component.ID }

// Description implements [tool.Tool].
func (t *componentTool) Description() string { return t.component.Description }

// IsLongRunning implements [tool.Tool]. Always true — the launcher delivers the
// message after the run, and the user's reply arrives as a new WhatsApp message.
func (t *componentTool) IsLongRunning() bool { return true }

// Declaration returns the function declaration the model sees.
func (t *componentTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name: t.component.ID,
		Description: t.component.Description +
			"\n\nSends this component to the user over WhatsApp. It returns immediately: " +
			"the user's response arrives as a new message, so do not call it again while waiting.",
		ParametersJsonSchema: t.schema,
	}
}

// Run implements the runnable tool interface. Delivery happens in the launcher
// after the run completes, so there is nothing to do here.
func (t *componentTool) Run(_ agent.Context, _ any) (map[string]any, error) {
	return map[string]any{
		"status":  "queued",
		"message": "Component queued for delivery over WhatsApp.",
	}, nil
}

// ProcessRequest packs the declaration into the LLM request. ADK discovers this
// method structurally, which is why it is not part of [tool.Tool].
func (t *componentTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	if _, exists := req.Tools[t.component.ID]; exists {
		return fmt.Errorf("whatsapp: duplicate tool %q", t.component.ID)
	}
	req.Tools[t.component.ID] = t

	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	decl := t.Declaration()
	for _, gt := range req.Config.Tools {
		if gt != nil && gt.FunctionDeclarations != nil {
			gt.FunctionDeclarations = append(gt.FunctionDeclarations, decl)
			return nil
		}
	}
	req.Config.Tools = append(req.Config.Tools, &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{decl},
	})
	return nil
}
