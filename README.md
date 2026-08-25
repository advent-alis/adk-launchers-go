# adk-launchers-go

Go modules that extend [ADK](https://google.golang.org/adk) with pluggable launchers.

## Packages

- `whatsapp` — launches an ADK agent into WhatsApp over Twilio.

## whatsapp

An ADK web sublauncher that translates between WhatsApp's world (phone numbers,
message SIDs, pre-approved templates) and ADK's (apps, users, sessions, tools),
so the agent stays unaware that WhatsApp exists.

Two endpoints mount on `go.alis.build/mux`, in the same process as the agent:

| Route | Auth | Job |
| --- | --- | --- |
| `POST /webhooks/whatsapp` | Twilio signature | validate, enqueue a Cloud Task, ack |
| `POST /tasks/whatsapp` | Google ID token (`SystemPost`) | run the agent, deliver the reply |

### How a message reaches the agent

The launcher and the agent are one process: "running the agent" is a Go call
inside a handler, not a network hop. Nothing polls for work.

```
Twilio ──POST──▶ /webhooks/whatsapp ─┐
                                     │
                    ◀──── 200 OK ────┘   Twilio is done
                                     │
                                     └──▶ enqueues a Cloud Task
                                            │
        Google Cloud Tasks ─────────────────┘
                │
                └──POST──▶ /tasks/whatsapp ──▶ runner.Run ──▶ Twilio REST API
```

Twilio abandons a webhook that does not answer within seconds, and an agent turn
takes far longer. So the webhook only validates, parses, and enqueues; Cloud
Tasks then calls back with a fresh request and a long timeout to do the work in.
Cloud Tasks is the caller — the agent never looks for work.

`/tasks/whatsapp` runs an agent on behalf of a phone number, trusting the payload
for whose it is. Left public, anyone could post a payload and take over any
user's session — hence `SystemPost`, which validates the OIDC token Cloud Tasks
attaches as the environment service account.

### Agent side

Add the toolset to the agent this launcher serves. Each catalog entry becomes a
tool, so "the agent populates a UI component" is a function call — the
function-calling API validates the arguments, and the launcher renders the call
to its Twilio template.

```go
agent, _ := llmagent.New(llmagent.Config{
    Name:     "my_agent",
    Toolsets: []tool.Toolset{whatsapp.NewToolset()},
})
```

The toolset returns zero tools when the catalog is absent from session state, so
the same agent still works unchanged over AG-UI, the console, or a cron.

### The catalog

`ContentSid` comes from Twilio. You build a Content Template (Console →
Messaging → Content Template Builder, or the Content API), Twilio assigns it an
SID like `HX2d1f8c48…`, and Meta approves it. WhatsApp accepts interactive
messages only as pre-approved templates — that is Meta policy, not Twilio's.

A catalog entry names a template and says when to use it:

```go
{ID: "confirm", ContentSid: "HX…", Description: "Ask the user yes or no."}
```

Only the description is authored. Twilio's friendly name is a slug, not an
instruction a model can act on — everything else is read from Twilio at startup,
so nothing about the template is declared twice.

For each entry the launcher fetches the template, derives its content type, and
scans its slots for `{{n}}` placeholders. Each placeholder becomes one `Field`:
an argument name, the variable position it fills, and the limit its role carries.

**A slot the template hard-codes produces no field.** A quick reply with fixed
`Yes` / `No` labels asks the model only for the body and the two payloads:

```
template:  body "{{1}}"   button 1 {title:"Yes", id:"{{2}}"}   button 2 {title:"No", id:"{{3}}"}

derived:   text              → {{1}}
           button_1_payload  → {{2}}
           button_2_payload  → {{3}}
```

Deriving rather than declaring is what keeps the schema honest. A
`twilio/quick-reply` template fixes its button count at creation — the actions
array is part of the approved template, and only the strings inside it vary:

```go
type TwilioQuickReply struct {
    Body    string             `json:"body"`
    Actions []QuickReplyAction `json:"actions"`   // fixed
}
```

A locally declared "one to three buttons" cannot express that, and a message with
too few values renders empty buttons.

The launcher **will not start** on a template it cannot read or render. A
component whose schema disagrees with its template produces sends that fail, and
a failed send reaches the user as silence.

How the catalog reaches the agent:

```
1. you write     Catalog{{ID: "confirm", ContentSid: "HX…", Description: "…"}}
2. at startup    FetchTemplate(HX…) → type, slots, {{n}} positions → []Resolved
3. each run      runner.WithStateDelta({"temp:_whatsapp_components": "[…]"})
4. Toolset.Tools reads that state key, builds a schema per Resolved
5. → Gemini      as function declarations
6. model emits   FunctionCall{Name: "confirm", Args: {text: "…", button_1_payload: "…"}}
7. deliver()     catalog.find("confirm") → variables(args) → {"1":…,"2":…}
8. → Twilio      CreateMessage{ContentSid: "HX…", ContentVariables: {…}}
```

Step 2 is the difference from a hand-declared catalog: the schema in step 4
matches the template exactly, because it was read from it.

### Launcher side

```go
wa := whatsapp.NewLauncher("my.agent", whatsapp.Config{
    AccountSid:  os.Getenv("TWILIO_ACCOUNT_SID"),
    AuthToken:   os.Getenv("TWILIO_AUTH_TOKEN"),
    PhoneNumber: "+17405307773",
    Queue:       "my-agent",
    Catalog: whatsapp.Catalog{
        {ID:          "confirm",
         ContentSid:  "HX2d1f8c480573d02b446da6eb2cc442c0",
         Description: "Ask the user a question they answer by tapping a button."},
    },
})
launcher := launchersweb.NewLauncher(webapi.NewLauncher(), wa)
```

```
adk web --port 8080 api whatsapp -app_name=my.agent
```

Two things are wired outside the code.

**The Twilio webhook.** In the Twilio console: Messaging → Senders → WhatsApp
senders → your sender → the **"When a message comes in"** field. Set it to
`/webhooks/whatsapp` on the agent's own Cloud Run URL, method `POST`:

```
https://my-agent-abc123-ew.a.run.app/webhooks/whatsapp
```

There is no separate host — the launcher adds its routes to the agent's service.
That URL is what the Twilio signature is computed over, so use `WithBaseURL` when
the origin the launcher reconstructs from the request differs from what is
configured here. For local testing, expose the agent with ngrok and paste the
ngrok URL into the same field.

**The Cloud Tasks queue.** GCP infra, so it belongs in the neuron's `infra/`
Terraform rather than any console:

```hcl
resource "google_cloud_tasks_queue" "main" {
  name     = var.ALIS_OS_NEURON
  location = var.ALIS_REGION == "africa-south1" ? "europe-west1" : var.ALIS_REGION
  retry_config { max_attempts = 1 }
}
```

`Config.Queue` must match `name`. `max_attempts = 1` is load-bearing: a retried
task re-runs the whole agent turn, and the user receives a second set of
messages.

### Supported content types

`twilio/text`, `twilio/quick-reply`, `twilio/list-picker`, and
`twilio/call-to-action`. A template of any other type fails at startup with an
error naming the type it found.

There is no layout convention to keep in step: variable positions are read from
the template, wherever its author put them. A template that uses `{{1}}` in a
button and `{{2}}` in the body works exactly as well as the reverse.

Plain text and media need no catalog entry: model text is sent directly, split
across messages at Twilio's 1600-character limit.

### Sessions

WhatsApp has no thread concept, so mapping a message to an ADK session is a
policy choice and lives behind `SessionResolver`. The default
`IdleWindowResolver` continues the sender's most recent session while it is
younger than 24 hours and starts a fresh one otherwise — no storage beyond the
ADK session service. `/new` starts a fresh session on demand.

Replace the policy with `WithSessionResolver`. A resolver receives the session
service, so it may read prior sessions and their events to decide.

Long-horizon recall is a separate concern with its own ADK answer: configure a
`MemoryService` on the launcher config.

### Not in scope

- **Sender display name and profile photo.** WhatsApp reads these from the
  WhatsApp Business Account sender, configured once in Meta Business Manager.
  There is no per-message override in the API.
- **Reviving an old session from a quote-reply.** The replied-to message SID
  reaches the resolver as `SessionRequest.RepliedToMessageSid`, but mapping a SID
  back to the session that produced it needs a store this package does not own.
- **Opening the 24-hour customer-service window.** Outside it WhatsApp permits
  only template messages, so an agent that speaks first must do so through a
  component.

See `whatsapp/doc.go` for the full package documentation.

## Testing

The test binary needs the Alis environment exported: `go.alis.build/mux` and
`go.alis.build/tasks` both initialise at process start (identity service, Cloud
Tasks client).

```bash
set -a
source <product_root>/.alis/.env
set +a
go test ./...
```
