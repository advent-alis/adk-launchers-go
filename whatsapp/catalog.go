package whatsapp

import (
	"fmt"
	"regexp"
)

// componentIDPattern constrains a component ID to a valid function name, since
// the ID is used verbatim as the tool name the model calls.
var componentIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Component names a Twilio content template the agent may send, and tells the
// model when to reach for it.
//
// The template's shape — its content type, how many buttons or rows it has, and
// which parts of it are variable — is read from Twilio at startup, not declared
// here. See [Resolved].
type Component struct {
	// ID names the component and is used verbatim as the tool name exposed to
	// the model. Must match ^[a-z][a-z0-9_]{0,62}$.
	ID string
	// ContentSid is the Twilio content template SID (HX...).
	ContentSid string
	// Description tells the model when to reach for this component. It becomes
	// the tool description, so it is prompt surface: write it for the model.
	// Twilio's own friendly name is a slug, which is why this is not derived.
	Description string
}

// Catalog is the set of components an agent may send. It is a launcher input;
// each entry is resolved against Twilio at startup and published into session
// state, where [Toolset] turns it into a tool.
type Catalog []Component

// Validate reports whether every entry is well formed. It checks only what can
// be known without Twilio; the template itself is verified when the launcher
// resolves it. Called by [NewLauncher] so a typo fails at wiring time.
func (c Catalog) Validate() error {
	ids := make(map[string]bool, len(c))
	sids := make(map[string]bool, len(c))
	for i, comp := range c {
		switch {
		case !componentIDPattern.MatchString(comp.ID):
			return fmt.Errorf("whatsapp: catalog[%d]: id %q must match %s", i, comp.ID, componentIDPattern)
		case ids[comp.ID]:
			return fmt.Errorf("whatsapp: catalog[%d]: duplicate id %q", i, comp.ID)
		case comp.ContentSid == "":
			return fmt.Errorf("whatsapp: catalog[%d] (%s): content sid is required", i, comp.ID)
		case sids[comp.ContentSid]:
			return fmt.Errorf("whatsapp: catalog[%d] (%s): content sid %s is already used by another component", i, comp.ID, comp.ContentSid)
		case comp.Description == "":
			return fmt.Errorf("whatsapp: catalog[%d] (%s): description is required (the model reads it)", i, comp.ID)
		}
		ids[comp.ID] = true
		sids[comp.ContentSid] = true
	}
	return nil
}

// resolvedCatalog is a catalog joined with the Twilio templates behind it.
type resolvedCatalog []Resolved

// find returns the resolved component with the given ID.
func (rc resolvedCatalog) find(id string) (*Resolved, bool) {
	for i := range rc {
		if rc[i].ID == id {
			return &rc[i], true
		}
	}
	return nil, false
}
