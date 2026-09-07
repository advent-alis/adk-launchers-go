// Package whatsapp is an ADK web sublauncher that launches an agent into
// WhatsApp over Twilio.
//
// It is the adapter between two models of a conversation. WhatsApp has phone
// numbers, message SIDs, and pre-approved templates; ADK has apps, users,
// sessions, and tools. This package translates in both directions so the agent
// stays unaware that WhatsApp exists.
//
// # Routes
//
// Two endpoints are mounted on go.alis.build/mux, in the same process as the
// agent:
//
//   - [WebhookPath] — inbound messages from Twilio. Public, authenticated by the
//     X-Twilio-Signature header.
//   - [TaskPath] — the Cloud Task that runs the agent. Registered with
//     SystemPost, so it requires a Google-signed ID token belonging to the
//     environment service account.
//
// # How a message reaches the agent
//
// The launcher and the agent are one process: "running the agent" is a Go call
// inside a handler, not a network hop. Nothing polls for work.
//
//	Twilio ──POST──▶ WebhookPath ─┐
//	                              │
//	             ◀──── 200 OK ────┘  Twilio is done
//	                              │
//	                              └──▶ enqueues a Cloud Task
//	                                     │
//	          Google Cloud Tasks ────────┘
//	                  │
//	                  └──POST──▶ TaskPath ──▶ runner.Run ──▶ Twilio REST API
//
// Twilio abandons a webhook that does not answer within seconds, and an agent
// turn takes far longer. So the webhook only validates, parses, and enqueues;
// Cloud Tasks then calls back with a fresh request and a long timeout to do the
// work in. Cloud Tasks is the caller — the agent never looks for work.
//
// TaskPath runs an agent on behalf of a phone number, trusting the payload for
// whose it is. Left public, anyone could post a payload and take over any user's
// session, so it is registered with SystemPost: Cloud Tasks attaches an OIDC
// token as the environment service account, and SystemPost validates it.
//
// # Sessions
//
// WhatsApp has no thread concept: an inbound message carries a sender, a
// receiver, and — only for an explicit quote-reply — the SID of the message
// being replied to. Mapping that onto an ADK session is a policy choice, so it
// lives behind [SessionResolver].
//
// The default [IdleWindowResolver] continues the sender's most recent session
// while it is younger than [DefaultIdleWindow] and starts a fresh one otherwise,
// which needs no storage beyond the ADK session service. Sending [ResetCommand]
// starts a new session immediately. Replace the policy with
// [WithSessionResolver]; a resolver receives the session service and may read
// prior sessions and their events to decide.
//
// New sessions are created by this package, not by the runner, so a resolver is
// taken at its word: an ID it returns for a session that no longer exists is a
// not-found error rather than a silently empty conversation under that ID.
//
// Long-horizon recall is a separate concern with its own ADK answer: configure a
// MemoryService on the launcher config and the agent remembers across sessions
// without this package holding old conversations in context.
//
// A WhatsApp sender maps to the ADK user ID returned by [UserID], which is
// prefixed and therefore distinct from the same human signed into the console.
//
// Sessions are kept to this channel independently of that, by [SessionPrefix].
// The same agent is usually reachable from a web console or a cron as well, and
// those runs share its ADK app — so without a marker, a WhatsApp message could
// resume whatever the person was last doing on the web. Only sessions minted
// here carry the prefix, and only those are continued. The two mechanisms are
// deliberately separate: resolving a phone number to a platform identity later
// merges memory across channels without also merging conversations.
//
// # Components
//
// WhatsApp gates interactive messages behind content templates that Meta must
// approve in advance. A [Catalog] names the templates an agent may send:
//
//	{ID: "confirm", ContentSid: "HX…", Description: "Ask the user yes or no."}
//
// Only the description is authored — Twilio's own friendly name is a slug, not
// an instruction a model can act on. The template's shape is read from Twilio at
// startup, so nothing about it is declared twice.
//
// For each entry the launcher fetches the template, derives which content type
// it is, and scans its slots for {{n}} placeholders. Each placeholder becomes one
// [Field] on a [Resolved] component: an argument name, the variable position it
// fills, and the limit its role carries. A slot the template hard-codes produces
// no field, so the model is never asked for a value it could not change — a
// template with fixed "Yes" and "No" labels asks only for the body and the two
// payloads.
//
// The resolved catalog is published into session state, where [Toolset] turns
// each entry into a tool. So "the agent populates a UI component" is literally a
// function call: the model calls confirm(text: …, button_1_payload: …), the
// function-calling API validates the arguments against the derived schema, and
// the launcher maps each one onto its variable position and sends the template.
// Component tools are long-running — the launcher owns delivery, and the user's
// answer arrives as a new inbound message rather than a tool result.
//
// Deriving rather than declaring is what keeps the schema honest. A
// twilio/quick-reply template fixes its button count at creation, since the
// actions array is part of the approved template and only the strings inside it
// vary. A locally declared "one to three buttons" could not express that, and a
// message with too few values renders empty buttons.
//
// The launcher will not start on a template it cannot read or render. A
// component whose schema disagrees with its template produces sends that fail,
// and a failed send reaches the user as silence.
//
// Plain text and media need no catalog entry. The agent's model text is sent
// directly, split across messages at [MaxTextRunes].
//
// # Agent setup
//
// Add [Toolset] to the agent this launcher serves. Without it the agent can
// reply in text but cannot reach any component:
//
//	agent, _ := llmagent.New(llmagent.Config{
//	    Name:     "my_agent",
//	    Toolsets: []tool.Toolset{whatsapp.NewToolset()},
//	})
//
// The toolset returns zero tools when the catalog is absent from state, so the
// same agent still works unchanged over AG-UI, the console, or a cron.
//
// # Launcher setup
//
//	wa := whatsapp.NewLauncher("my.agent", whatsapp.Config{
//	    AccountSid:  os.Getenv("TWILIO_ACCOUNT_SID"),
//	    AuthToken:   os.Getenv("TWILIO_AUTH_TOKEN"),
//	    PhoneNumber: "+17405307773",
//	    Queue:       "my-agent",
//	    Catalog: whatsapp.Catalog{
//	        {ID:          "confirm",
//	         ContentSid:  "HX2d1f8c480573d02b446da6eb2cc442c0",
//	         Description: "Ask the user a question they answer by tapping a button."},
//	    },
//	})
//	launcher := launchersweb.NewLauncher(webapi.NewLauncher(), wa)
//
// At runtime, enable it by keyword:
//
//	adk web --port 8080 api whatsapp -app_name=my.agent
//
// Two things are wired outside the code. In the Twilio console, set the
// WhatsApp sender's "When a message comes in" field to [WebhookPath] on the
// agent's own Cloud Run URL, e.g.
// https://my-agent-abc123-ew.a.run.app/webhooks/whatsapp — the launcher adds its
// routes to the agent's service, so there is no separate host. That URL is what
// the Twilio signature is computed over; use [WithBaseURL] when the origin the
// launcher reconstructs from the request differs from what was configured there.
//
// In the neuron's infra/ Terraform, provision a google_cloud_tasks_queue whose
// name matches Config.Queue. Set retry_config.max_attempts to 1: a retried task
// re-runs the whole agent turn, and the user receives a second set of messages.
//
// # Source layout
//
// The package is an adapter, and the filenames say which side of it a file sits
// on. The launcher_ files are the wiring; twilio_ and adk_ are the two sides it
// translates between.
//
//	launcher.go            route paths, [Config], [Launcher], the launcher type,
//	                       [NewLauncher], and SetupHostRoutes — everything from
//	                       what a caller supplies to the server being ready
//	launcher_options.go    [Option] and the With… functions
//	launcher_contract.go   the [adkweb.Sublauncher] methods the web launcher calls
//	launcher_handlers.go   what runs when those routes are hit, and outbound delivery
//
//	catalog.go             [Component] and [Catalog] — the vocabulary both sides share
//
//	twilio_inbound.go      webhook form to [Inbound]
//	twilio_sender.go       sending messages, fetching media and templates
//	twilio_template.go     content template to [Resolved] fields
//
//	adk_runtime.go         running the agent in-process
//	adk_session.go         [UserID], [SessionPrefix], and [SessionResolver]
//	adk_toolset.go         resolved catalog to tools the model sees
//
// # What this package does not do
//
//   - It does not set the sender's display name or profile photo. WhatsApp reads
//     those from the WhatsApp Business Account sender, configured once in Meta
//     Business Manager; there is no per-message override in the API.
//   - It does not revive an old session from a quote-reply. The SID of the
//     replied-to message reaches [SessionResolver] as
//     SessionRequest.RepliedToMessageSid, but mapping a SID back to the session
//     that produced it needs a store this package does not own.
//   - It does not open the 24-hour customer-service window. Outside that window
//     WhatsApp permits only template messages, so an agent that speaks first —
//     from a cron, say — must do so through a component.
package whatsapp
