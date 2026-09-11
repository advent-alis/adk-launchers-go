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

### Admission

The signature proves a message came from Twilio. It says nothing about who sent
it — a sender is a phone number, and a phone number is not an account. Closing
that gap needs an identity system this package knows nothing about, so it lives
behind `UserStore`.

Closing it is **required** — `NewLauncher` refuses to build without it — and it
comes in two parts, one of them optional.

**`Config.Users`** (required) — a `UserStore`. Find the user holding this number;
create one if nobody does. This is the package's one opinion about identity: a
user exists before a session does, and since the field can't be omitted, it holds
for every launcher.

```go
type UserStore interface {
    FindByPhoneNumber(ctx context.Context, e164 string) (userID string, err error)   // "" = not found
    CreateByPhoneNumber(ctx context.Context, e164 string) (userID string, err error)
}
```

Nothing here says what a user *is*. Whatever your definition, it needs an id and
it must be addressable by WhatsApp number — the launcher neither sees nor stores
anything else. Note the trade: possession of the number becomes the whole
credential, and nothing expires.

**`WithGate`** (optional) — a `Gate`. May this sender proceed at all? It runs
**between** the two halves of the store, so a sender it refuses is never created:

```
Find ──▶ Gate ──▶ Create (only if Find found nobody) ──▶ run the agent
             └──▶ refuse: send a message, no user, no session, no agent
```

`GateRequest.UserID` is what the store found, empty when this message would open
an account — which is what a closed beta is made of:

```go
func (g myGate) Admit(ctx context.Context, req *whatsapp.GateRequest) (*whatsapp.GateDecision, error) {
    if req.UserID == "" {
        return &whatsapp.GateDecision{
            Reply: &whatsapp.Outbound{Text: "We're not open to new users yet."},
        }, nil
    }
    return &whatsapp.GateDecision{Allow: true}, nil
}
```

`Allow` is false by default, so a decision a gate forgot to fill in refuses
rather than admits. An error is a fault — return one only for a check that could
not be performed.

All of it runs on the task, before any attachment is fetched, before a session is
resolved or created, and before the agent is loaded.

Nothing about a refused sender is recorded, which also means the gate has nothing
to remember with. An exchange spanning several messages is carried by the answers
instead: refuse with a quick-reply template, and the payload of the button the
sender taps arrives on the next call as `GateRequest.ButtonPayload` — enough to
ask a question on one message and act on the answer to the next.

What a gate cannot do is change the terms. Allowing a sender means the store
creates them as the holder of their number; there is no "refuse now, create
differently later". A flow that needs that — proving an email before the account
exists, say — belongs outside this package.

Neither half puts anything in front of the model. What reaches the agent is the
ADK user id the store returned — `ReadonlyContext.UserID()` on every turn — and
that is the handle it looks the sender up by when it needs more than an id.
Nothing about the sender is injected into session state, so an agent that wants
the number, a name, or an email reads its own user service rather than trusting a
field this package carried across for it.

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
    // Required. Finds the user holding the sender's number, and creates one
    // the first time a number is seen.
    Users: myUserStore{},
}, whatsapp.WithGate(myGate{}))   // optional
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

Sessions are also kept to this channel. The same agent is usually reachable from
a web console or a cron, and those runs share its ADK app — so without a marker a
WhatsApp message could resume whatever the person was last doing on the web.
Sessions minted here are prefixed (`SessionPrefix`), and only prefixed sessions
are continued. A bare UUID from another channel never matches, because the rest
of a minted id is hex and `w` is not a hex digit.

That is deliberately separate from identity. The launcher asks the store for the
ADK user on every message, so a store that returns the id the platform already
knows the sender by makes their WhatsApp turns and their console turns a single
ADK user that shares memory — and the session prefix still keeps the two
conversations apart. The launcher decides *that* a user is needed; the store
decides *who*, and performs the write.

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
- **Keeping any record of who a number belongs to.** The store or gate is asked
  on every message and the answer is used for that turn only. The account, and
  its binding to the number, live in your identity system.
- **Linking a number to the same person's profile on another surface.** That
  needs proof the person holds both — a browser or a verification code — which is
  outside a package whose entire input is one inbound message.
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
