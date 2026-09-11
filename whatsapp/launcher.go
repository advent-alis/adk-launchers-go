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

	"github.com/twilio/twilio-go"
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

// DefaultTemplateTimeout caps the Twilio calls that resolve the catalog at
// startup.
const DefaultTemplateTimeout = 30 * time.Second

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
	// Users resolves a sender to a user of the product's own account system,
	// creating one the first time a number is seen.
	//
	// Required, and not optional by omission: a WhatsApp sender is a phone number,
	// a phone number is not an account, and there is no safe default for that gap.
	// A launcher that ran without one would admit every sender as an ADK user
	// derived from their number, making the conversation history a bearer asset
	// held by whoever holds the number next.
	//
	// It carries this package's one opinion about identity: a user exists before a
	// session does. Whether a given sender may reach the agent at all is a
	// separate question, answered by an optional [Gate] — see [WithGate].
	Users UserStore
}

// Launcher is the public surface of [NewLauncher]. Compose it with
// go.alis.build/adk/launchers/web.NewLauncher.
type Launcher interface {
	// The launcher is a sublauncher, so it implements [adkweb.Sublauncher].
	adkweb.Sublauncher
	// SetupHostRoutes registers this launcher's routes on the process-wide mux.
	//
	// This mirrors go.alis.build/adk/launchers/web.HostRouteSetup, declared here
	// rather than imported: the composing launcher discovers the method
	// structurally, so restating it keeps that whole module off this package's
	// dependency list while still checking the signature at compile time.
	SetupHostRoutes(config *adklauncher.Config) error
}

// launcher implements [Launcher].
type launcher struct {
	// appName is the ADK app the agent runs for inbound WhatsApp messages.
	appName string
	// cfg is the infrastructure this launcher needs. All fields are required.
	cfg Config
	// flags is the CLI flag set this launcher parses. It is built once and reused
	// on every Parse call, so the launcher can be reused in multiple CLI contexts.
	flags *flag.FlagSet

	// sender is the Twilio client that sends messages and fetches media and templates.
	sender Sender
	// media fetches inbound media from Twilio.
	// It may be nil if the sender does not implement [MediaFetcher].
	media MediaFetcher
	// templates fetches templates from Twilio.
	// It may be nil if the sender does not implement [TemplateFetcher].
	templates TemplateFetcher
	// resolver decides which ADK session an inbound WhatsApp message belongs to.
	resolver SessionResolver
	// users finds the user behind a sender's number, and creates one when nobody
	// holds it. Never nil: [NewLauncher] refuses to build a launcher without it.
	users UserStore
	// gate vetoes senders before their user is created. Nil admits everybody,
	// which is the default: [Config].Users has already answered who they are.
	gate Gate
	// runtime runs the agent in-process using the services already wired into the
	// ADK launcher config, so a WhatsApp turn shares session, memory, and artifact
	// state with every other surface the agent is launched on.
	runtime *runtime

	// catalog is the config catalog joined with the Twilio templates behind it,
	// resolved once during setup.
	catalog resolvedCatalog
	// stateDelta publishes the resolved catalog into session state on every run,
	// where [Toolset] reads it. Built once: it does not vary per message.
	stateDelta map[string]any
	// templateTimeout caps the Twilio calls that resolve the catalog.
	templateTimeout time.Duration

	// pinnedBaseURL is the origin the Twilio signature is checked against: the URL
	// Twilio was configured to call. By default it is derived from the inbound
	// request, which is correct on Cloud Run and behind a proxy that preserves
	// Host. Pin it when it is not — a mismatch fails the signature check, since
	// Twilio signs the URL it called.
	pinnedBaseURL string

	// pinnedTaskURL is the origin Cloud Tasks calls back on. It differs from
	// pinnedBaseURL only when something other than this service serves the
	// webhook — a BFF proxying it, say — because the signature must then be
	// checked against that public origin while the task must still reach a host
	// serving TaskPath. Empty means the two are the same.
	pinnedTaskURL string

	// setupOnce guards the one-time setup of the runtime and the catalog, which
	// must happen before the webhook and task handler can serve traffic. The
	// setup error is returned on every subsequent call.
	setupOnce sync.Once
	// setupErr is the error returned by SetupHostRoutes if the one-time setup failed.
	setupErr error
}

// variables to satisfy the [Launcher] interface. They are declared here so the
// compiler checks the signature at compile time, rather than importing the
// interface from the web launcher package.
var (
	// launcher implements [Launcher]
	_ Launcher = (*launcher)(nil)
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
//	    Users:       myUserStore{},
//	}, whatsapp.WithGate(myGate{}))
//	launchersweb.NewLauncher(webapi.NewLauncher(), wa)
func NewLauncher(appName string, cfg Config, opts ...Option) Launcher {
	// Validate the app name and config. The launcher cannot serve without them, so panic on invalid input.
	if strings.TrimSpace(appName) == "" {
		panic("whatsapp: app name is required")
	}
	{
		var err error
		switch {
		case cfg.AccountSid == "":
			err = fmt.Errorf("whatsapp: config.AccountSid is required")
		case cfg.AuthToken == "":
			err = fmt.Errorf("whatsapp: config.AuthToken is required")
		case !strings.HasPrefix(cfg.PhoneNumber, "+"):
			err = fmt.Errorf("whatsapp: config.PhoneNumber must be E.164 with a leading +, got %q", cfg.PhoneNumber)
		case cfg.Queue == "":
			err = fmt.Errorf("whatsapp: config.Queue is required")
		case cfg.Users == nil:
			err = fmt.Errorf("whatsapp: config.Users is required, since a phone number is not an account")
		default:
			err = cfg.Catalog.Validate()
		}
		if err != nil {
			panic(err.Error())
		}
	}

	// Build the Twilio sender. It sends messages and fetches media and templates.
	var sender *twilioSender
	{
		sender = &twilioSender{
			client: twilio.NewRestClientWithParams(twilio.ClientParams{
				Username:   cfg.AccountSid,
				AccountSid: cfg.AccountSid,
				Password:   cfg.AuthToken,
			}),
			accountSid: cfg.AccountSid,
			authToken:  cfg.AuthToken,
			from:       cfg.PhoneNumber,
			http:       http.DefaultClient,
		}
	}

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
		users:     cfg.Users,
	}

	// Apply the options. They may replace the resolver, sender, or other fields.
	for _, opt := range opts {
		opt(l)
	}

	// Build the CLI flag set. It is reused on every Parse call, so the launcher can be reused in multiple CLI contexts.

	// Instantiate the whatsapp sublauncher with the keyword "whatsapp".
	// The composing launcher mounts it under that path, e.g. /whatsapp/..., and
	// the CLI flag set is namespaced to that keyword, e.g. --whatsapp.app_name.
	// where Keyword = "whatsapp" in this case.
	fs := flag.NewFlagSet(Keyword, flag.ContinueOnError)

	// Add the app name flag. It is required, but the launcher already validated it, so the default is safe.
	fs.StringVar(&l.appName, "app_name", l.appName, "ADK app name to run for inbound WhatsApp messages")

	// Add flag set on the launcher
	l.flags = fs

	// Return the launcher.
	// (It is ready to serve traffic, but the runtime and catalog are not set up until SetupHostRoutes is called.)
	return l
}

// SetupHostRoutes registers the webhook and task handler on go.alis.build/mux.
// Safe to call more than once; mounting happens once per launcher.
func (l *launcher) SetupHostRoutes(config *adklauncher.Config) error {

	// Setup the runtime once:
	// 1. validates the config and app name,
	// 2. resolves the catalog, and
	// 3. mounts the routes.
	l.setupOnce.Do(func() {
		// Create the runtime that runs the agent in-process. It needs the launcher
		// config and the app name, and validates both.
		{
			switch {
			case config == nil:
				l.setupErr = fmt.Errorf("whatsapp: launcher config is required")
			case config.AgentLoader == nil:
				l.setupErr = fmt.Errorf("whatsapp: launcher config has no AgentLoader")
			case config.SessionService == nil:
				l.setupErr = fmt.Errorf("whatsapp: launcher config has no SessionService")
			case strings.TrimSpace(l.appName) == "":
				l.setupErr = fmt.Errorf("whatsapp: app name is required")
			}
			if l.setupErr != nil {
				return
			}
			l.runtime = &runtime{cfg: config, appName: strings.TrimSpace(l.appName)}
		}
		// Resolve the catalog once at startup, reading each template from Twilio so
		// the schema the model sees matches what the template actually declares.
		// The launcher refuses to start on a template it cannot read or render: a
		// component whose schema disagrees with its template produces sends that
		// fail, and a failed send reaches the user as silence.
		if len(l.cfg.Catalog) > 0 {
			if l.templates == nil {
				l.setupErr = fmt.Errorf("whatsapp: a catalog was configured but the sender cannot fetch templates")
				return
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
					l.setupErr = err
					return
				}
				resolved, err := resolveTemplate(comp, tmpl)
				if err != nil {
					l.setupErr = err
					return
				}
				if len(resolved.Fields) == 0 {
					// Every value is fixed in the template, so the model has nothing
					// to supply. Allowed, and worth saying out loud: it is more often
					// a template built without placeholders than a deliberate choice.
					alog.Infof(ctx, "whatsapp: component %q (%s) declares no variables; the model can only trigger it",
						comp.ID, comp.ContentSid)
				}
				l.catalog = append(l.catalog, *resolved)
			}

			encoded, err := json.Marshal(l.catalog)
			if err != nil {
				l.setupErr = fmt.Errorf("whatsapp: marshal resolved catalog: %w", err)
				return
			}
			l.stateDelta = map[string]any{StateKey: string(encoded)}
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
