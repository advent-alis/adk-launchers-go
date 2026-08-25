package whatsapp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gmux "github.com/gorilla/mux"
	"go.alis.build/alog"
	alismux "go.alis.build/mux"
	adklauncher "google.golang.org/adk/v2/cmd/launcher"
	adkweb "google.golang.org/adk/v2/cmd/launcher/web"
)

// Routes this launcher mounts on the host mux.
const (
	// WebhookPath receives inbound messages from Twilio. Configure it as the
	// "When a message comes in" webhook on your WhatsApp sender.
	WebhookPath = "/webhooks/whatsapp"
	// TaskPath receives the Cloud Task that runs the agent. Not called by
	// Twilio; only the environment service account may reach it.
	TaskPath = "/tasks/whatsapp"
)

// Keyword is the CLI sublauncher keyword: adk web ... whatsapp
const Keyword = "whatsapp"

// DefaultTemplateTimeout caps the Twilio calls that resolve the catalog at
// startup.
const DefaultTemplateTimeout = 30 * time.Second

// UserID returns the ADK user ID for a WhatsApp sender in E.164.
//
// The mapping is deterministic and prefixed, so a WhatsApp participant is a
// distinct ADK user from the same human signed into the console. Sessions and
// memory therefore do not cross between the two surfaces. Resolving a phone
// number to a platform identity is a deliberate extra step, not the default.
func UserID(phoneE164 string) string {
	return "whatsapp:" + phoneE164
}

// Config is the infrastructure this launcher needs. All fields are required.
type Config struct {
	// AccountSid is the Twilio account SID.
	AccountSid string
	// AuthToken is the Twilio auth token. It signs outbound API calls and
	// validates inbound webhook signatures.
	AuthToken string
	// PhoneNumber is the WhatsApp sender in E.164, e.g. "+17405307773".
	PhoneNumber string
	// Queue is the Cloud Tasks queue that carries inbound messages to the agent.
	// Either a queue ID (resolved against ALIS_OS_PROJECT and ALIS_REGION) or a
	// full projects/{p}/locations/{l}/queues/{q} name.
	Queue string
	// Catalog is the set of interactive components the agent may send. It may be
	// empty, in which case the agent replies in plain text only.
	Catalog Catalog
}

// validate reports whether the config can serve traffic.
func (c Config) validate() error {
	switch {
	case c.AccountSid == "":
		return fmt.Errorf("whatsapp: config.AccountSid is required")
	case c.AuthToken == "":
		return fmt.Errorf("whatsapp: config.AuthToken is required")
	case !strings.HasPrefix(c.PhoneNumber, "+"):
		return fmt.Errorf("whatsapp: config.PhoneNumber must be E.164 with a leading +, got %q", c.PhoneNumber)
	case c.Queue == "":
		return fmt.Errorf("whatsapp: config.Queue is required")
	}
	return c.Catalog.Validate()
}

// Launcher is the public surface of [NewLauncher]. Compose it with
// go.alis.build/adk/launchers/web.NewLauncher.
type Launcher interface {
	adkweb.Sublauncher
	// SetupHostRoutes registers this launcher's routes on the process-wide mux.
	//
	// This mirrors go.alis.build/adk/launchers/web.HostRouteSetup, declared here
	// rather than imported: the composing launcher discovers the method
	// structurally, so restating it keeps that whole module off this package's
	// dependency list while still checking the signature at compile time.
	SetupHostRoutes(config *adklauncher.Config) error
}

// Option configures optional launcher behaviour.
type Option func(*launcher)

// WithSessionResolver replaces the default [IdleWindowResolver], which continues
// the user's most recent session while it is younger than [DefaultIdleWindow].
//
// This is the seam for a smarter policy — for example a resolver that reads the
// recent history from SessionRequest.Sessions and asks a model whether the topic
// has turned over. A nil resolver is ignored.
func WithSessionResolver(resolver SessionResolver) Option {
	return func(l *launcher) {
		if resolver != nil {
			l.resolver = resolver
		}
	}
}

// WithIdleWindow sets the idle period on the default resolver. It has no effect
// once [WithSessionResolver] has replaced that resolver.
func WithIdleWindow(window IdleWindowResolver) Option {
	return func(l *launcher) { l.resolver = window }
}

// WithSender replaces the Twilio sender, for testing a launcher without reaching
// Twilio. If sender also implements [MediaFetcher] it serves inbound media, and
// if it implements [TemplateFetcher] it resolves the catalog; otherwise those
// capabilities are dropped. A nil sender is ignored.
func WithSender(sender Sender) Option {
	return func(l *launcher) {
		if sender == nil {
			return
		}
		l.sender = sender
		l.media, _ = sender.(MediaFetcher)
		l.templates, _ = sender.(TemplateFetcher)
	}
}

// WithTemplateTimeout caps how long startup waits on Twilio while resolving the
// catalog. Zero means [DefaultTemplateTimeout].
func WithTemplateTimeout(timeout time.Duration) Option {
	return func(l *launcher) { l.templateTimeout = timeout }
}

// WithBaseURL pins the origin used for the Twilio signature check and the Cloud
// Task callback, e.g. "https://my-agent-abc123.a.run.app".
//
// By default the origin is derived from the inbound request, which is correct on
// Cloud Run and behind a proxy that preserves Host. Pin it when it is not — a
// mismatch fails the signature check, since Twilio signs the URL it called.
func WithBaseURL(baseURL string) Option {
	return func(l *launcher) { l.pinnedBaseURL = strings.TrimSuffix(baseURL, "/") }
}

// launcher implements [Launcher].
type launcher struct {
	// appName is the ADK app the agent runs for inbound WhatsApp messages.
	appName string
	// cfg is the infrastructure this launcher needs. All fields are required.
	cfg     Config
	// flags is the CLI flag set this launcher parses. It is built once and reused
	// on every Parse call, so the launcher can be reused in multiple CLI contexts.
	flags   *flag.FlagSet

	// sender is the Twilio client that sends messages and fetches media and templates.
	sender    Sender
	// media fetches inbound media from Twilio.
	// It may be nil if the sender does not implement [MediaFetcher].
	media     MediaFetcher
	// templates fetches templates from Twilio.
	// It may be nil if the sender does not implement [TemplateFetcher].
	templates TemplateFetcher
	// resolver decides which ADK session an inbound WhatsApp message belongs to.
	resolver  SessionResolver
	// runtime runs the agent in-process using the services already wired into the
	// ADK launcher config, so a WhatsApp turn shares session, memory, and artifact
	// state with every other surface the agent is launched on.
	runtime   *runtime

	// catalog is the config catalog joined with the Twilio templates behind it,
	// resolved once during setup.
	catalog resolvedCatalog
	// stateDelta publishes the resolved catalog into session state on every run,
	// where [Toolset] reads it. Built once: it does not vary per message.
	stateDelta map[string]any
	// templateTimeout caps the Twilio calls that resolve the catalog.
	templateTimeout time.Duration

	// pinnedBaseURL is the origin used for the Twilio signature check and the Cloud Task callback.
	// By default the origin is derived from the inbound request, which is correct on
	// Cloud Run and behind a proxy that preserves Host. Pin it when it is not — a
	// mismatch fails the signature check, since Twilio signs the URL it called.
	pinnedBaseURL string

	// setupOnce guards the one-time setup of the runtime and the catalog, which
	// must happen before the webhook and task handler can serve traffic. The
	// setup error is returned on every subsequent call.
	setupOnce sync.Once
	// setupErr is the error returned by SetupHostRoutes if the one-time setup failed.
	setupErr  error
}

// variables to satisfy the [Launcher] interface. They are declared here so the
// compiler checks the signature at compile time, rather than importing the
// interface from the web launcher package.
var (
	// launcher implements [Launcher] 
	_ Launcher           = (*launcher)(nil)
	// launcher implements [adkweb.Sublauncher].
	_ adkweb.Sublauncher = (*launcher)(nil)
)

// NewLauncher returns a WhatsApp sublauncher for appName.
//
// It panics on invalid input, because a launcher that cannot serve is a
// programming error at wiring time, not a runtime condition:
//
//	wa := whatsapp.NewLauncher("my.agent", whatsapp.Config{
//	    AccountSid:  os.Getenv("TWILIO_ACCOUNT_SID"),
//	    AuthToken:   os.Getenv("TWILIO_AUTH_TOKEN"),
//	    PhoneNumber: "+17405307773",
//	    Queue:       "my-agent",
//	    Catalog:     catalog,
//	})
//	launchersweb.NewLauncher(webapi.NewLauncher(), wa)
func NewLauncher(appName string, cfg Config, opts ...Option) Launcher {
	// Validate the app name and config. The launcher cannot serve without them, so panic on invalid input.
	if strings.TrimSpace(appName) == "" {
		panic("whatsapp: app name is required")
	}
	if err := cfg.validate(); err != nil {
		panic(err.Error())
	}

	// Build the Twilio sender. It sends messages and fetches media and templates.
	sender := newTwilioSender(cfg)

	// Build the launcher with defaults, then apply the options. The defaults are:
	// - IdleWindowResolver with [DefaultIdleWindow]
	// - Twilio sender that sends messages and fetches media and templates
	l := &launcher{
		appName:   appName,
		cfg:       cfg,
		sender:    sender,
		media:     sender,
		templates: sender,
		resolver:  IdleWindowResolver{},
	}

	// Apply the options. They may replace the resolver, sender, or other fields.
	for _, opt := range opts {
		opt(l)
	}

	// Build the CLI flag set. It is reused on every Parse call, so the launcher can be reused in multiple CLI contexts.

	// Instantiate an empty CLI flag set,
	fs := flag.NewFlagSet(Keyword, flag.ContinueOnError)

	// Add the app name flag. It is required, but the launcher already validated it, so the default is safe.
	fs.StringVar(&l.appName, "app_name", l.appName, "ADK app name to run for inbound WhatsApp messages")

	// Add flag set on the launcher
	l.flags = fs

	// Return the launcher. 
	// (It is ready to serve traffic, but the runtime and catalog are not set up until SetupHostRoutes is called.)
	return l
}

// Keyword returns the CLI sublauncher keyword.
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

// UserMessage prints the WhatsApp endpoints when the web server starts.
func (l *launcher) UserMessage(webURL string, printer func(v ...any)) {
	printer(fmt.Sprintf("        whatsapp:  webhook %s%s", webURL, WebhookPath))
	printer(fmt.Sprintf("        whatsapp:  task handler %s%s", webURL, TaskPath))
}

// SetupHostRoutes registers the webhook and task handler on go.alis.build/mux.
// Safe to call more than once; mounting happens once per launcher.
func (l *launcher) SetupHostRoutes(config *adklauncher.Config) error {
	
	// Setup the runtime once: 
	// 1. validates the config and app name, 
	// 2. resolves the catalog, and 
	// 3. mounts the routes.
	l.setupOnce.Do(func() {
		// Create the runtime that runs the agent in-process. It needs the launcher config and the app name, and validates both.
		if l.runtime, l.setupErr = newRuntime(config, l.appName); l.setupErr != nil {
			return
		}
		// Resolve the catalog once at startup, so the launcher can publish it into session state on every run. 
		// This is deliberate: a component whose schema does not match its template produces messages that fail 
		// to send, and a send failure reaches the user as silence.
		if l.setupErr = l.resolveCatalog(); l.setupErr != nil {
			return
		}

		// The webhook is public and authenticated by the Twilio signature.
		alismux.Post(WebhookPath, l.handleWebhook)

		// The task handler runs agents, so it requires the environment service
		// account's Google ID token (not public).
		// The systemPost middleware checks the ID token and rejects requests from any other caller. 
		alismux.SystemPost(TaskPath, l.handleTask)
	})

	// Return the setup error if any, so the composing launcher can fail fast on startup.
	return l.setupErr
}

// resolveCatalog reads each catalog entry's template from Twilio and derives the
// arguments the model must supply, then publishes the result into session state.
//
// The launcher will not start on a template it cannot read or render. That is
// deliberate: a component whose schema does not match its template produces
// messages that fail to send, and a send failure reaches the user as silence.
func (l *launcher) resolveCatalog() error {
	if len(l.cfg.Catalog) == 0 {
		return nil
	}
	if l.templates == nil {
		return fmt.Errorf("whatsapp: a catalog was configured but the sender cannot fetch templates")
	}

	timeout := l.templateTimeout
	if timeout <= 0 {
		timeout = DefaultTemplateTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	l.catalog = make(resolvedCatalog, 0, len(l.cfg.Catalog))
	for _, comp := range l.cfg.Catalog {
		tmpl, err := l.templates.FetchTemplate(ctx, comp.ContentSid)
		if err != nil {
			return err
		}
		resolved, err := resolveTemplate(comp, tmpl)
		if err != nil {
			return err
		}
		if len(resolved.Fields) == 0 {
			// Every value is fixed in the template, so the model has nothing to
			// supply. Allowed, and worth saying out loud: it is more often a
			// template built without placeholders than a deliberate choice.
			alog.Infof(ctx, "whatsapp: component %q (%s) declares no variables; the model can only trigger it",
				comp.ID, comp.ContentSid)
		}
		l.catalog = append(l.catalog, *resolved)
	}

	encoded, err := json.Marshal(l.catalog)
	if err != nil {
		return fmt.Errorf("whatsapp: marshal resolved catalog: %w", err)
	}
	l.stateDelta = map[string]any{StateKey: string(encoded)}
	return nil
}

// baseURL returns the origin Twilio called, used for the signature check and the
// Cloud Task callback.
func (l *launcher) baseURL(r *http.Request) string {
	if l.pinnedBaseURL != "" {
		return l.pinnedBaseURL
	}
	if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.0.0.1")) {
		return "http://" + r.Host
	}
	// Cloud Run and ngrok both terminate TLS upstream, so the inbound request is
	// plaintext while the URL Twilio signed is https.
	return "https://" + r.Host
}
