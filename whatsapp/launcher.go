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
	appName string
	cfg     Config
	flags   *flag.FlagSet

	sender    Sender
	media     MediaFetcher
	templates TemplateFetcher
	resolver  SessionResolver
	runtime   *runtime

	// catalog is the config catalog joined with the Twilio templates behind it,
	// resolved once during setup.
	catalog resolvedCatalog
	// stateDelta publishes the resolved catalog into session state on every run,
	// where [Toolset] reads it. Built once: it does not vary per message.
	stateDelta map[string]any
	// templateTimeout caps the Twilio calls that resolve the catalog.
	templateTimeout time.Duration

	pinnedBaseURL string

	setupOnce sync.Once
	setupErr  error
}

var (
	_ Launcher           = (*launcher)(nil)
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
	if strings.TrimSpace(appName) == "" {
		panic("whatsapp: app name is required")
	}
	if err := cfg.validate(); err != nil {
		panic(err.Error())
	}

	sender := newTwilioSender(cfg)
	l := &launcher{
		appName:   appName,
		cfg:       cfg,
		sender:    sender,
		media:     sender,
		templates: sender,
		resolver:  IdleWindowResolver{},
	}
	for _, opt := range opts {
		opt(l)
	}

	fs := flag.NewFlagSet(Keyword, flag.ContinueOnError)
	fs.StringVar(&l.appName, "app_name", l.appName, "ADK app name to run for inbound WhatsApp messages")
	l.flags = fs

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
	l.setupOnce.Do(func() {
		if l.runtime, l.setupErr = newRuntime(config, l.appName); l.setupErr != nil {
			return
		}
		if l.setupErr = l.resolveCatalog(); l.setupErr != nil {
			return
		}
		// The webhook is public and authenticated by the Twilio signature. The
		// task handler runs agents, so it requires the environment service
		// account's Google ID token.
		alismux.Post(WebhookPath, l.handleWebhook)
		alismux.SystemPost(TaskPath, l.handleTask)
	})
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
