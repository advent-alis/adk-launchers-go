package whatsapp

import (
	"fmt"
	"strings"

	gmux "github.com/gorilla/mux"
	adklauncher "google.golang.org/adk/v2/cmd/launcher"
)

// The methods below are the framework contract, not application API. Five of
// them satisfy [adkweb.Sublauncher] and SetupHostRoutes satisfies the alis web
// launcher's optional host-route extension. The composing launcher calls them
// while parsing the command line and starting the server; nothing in this
// package calls them, and neither should a caller.
//
// They are exported because Go requires a method name to match the interface
// that declares it. Since [Launcher] embeds [adkweb.Sublauncher], they are
// reachable on the value [NewLauncher] returns — reachable, but not for use.
//
// Needs to satisfy the [adkweb.Sublauncher] interface:
// type Sublauncher interface {
//       Keyword() string          // ← required
//       Parse(args []string) ([]string, error)
//       CommandLineSyntax() string
//       SimpleDescription() string
//       SetupSubrouters(router *mux.Router, config *launcher.Config) error
//       UserMessage(...)
// }
// So these methods are satisfied here:


// Keyword is the CLI sublauncher keyword: adk web ... whatsapp
const Keyword = "whatsapp"

// Keyword returns the CLI sublauncher keyword. Called by the web launcher to
// match this sublauncher against the keywords on the command line.
func (l *launcher) Keyword() string { return Keyword }

// Parse parses whatsapp-specific CLI flags and returns the remaining args.
func (l *launcher) Parse(args []string) ([]string, error) {
	if err := l.flags.Parse(args); err != nil || !l.flags.Parsed() {
		return nil, fmt.Errorf("whatsapp: parse flags: %w", err)
	}
	return l.flags.Args(), nil
}

// CommandLineSyntax returns flag usage for help output.
func (l *launcher) CommandLineSyntax() string {
	var b strings.Builder
	l.flags.SetOutput(&b)
	l.flags.PrintDefaults()
	return b.String()
}

// SimpleDescription returns a one-line summary for the web launcher help text.
func (l *launcher) SimpleDescription() string {
	return "WhatsApp webhook and agent delivery via Twilio"
}

// SetupSubrouters is a no-op: both routes live on the host mux so they can carry
// the system-auth middleware. See [launcher.SetupHostRoutes].
func (l *launcher) SetupSubrouters(_ *gmux.Router, _ *adklauncher.Config) error {
	return nil
}

// UserMessage prints the WhatsApp endpoints when the web server starts. The web
// launcher calls it once per active sublauncher and supplies both the server URL
// and the printer to write with, so the output lands in the same log as
// everything else the server reports at startup.
func (l *launcher) UserMessage(webURL string, printer func(v ...any)) {
	printer(fmt.Sprintf("        whatsapp:  webhook %s%s", webURL, WebhookPath))
	printer(fmt.Sprintf("        whatsapp:  task handler %s%s", webURL, TaskPath))
}
