package whatsapp

import (
	"context"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

func TestIdleWindowResolver(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tests := []struct {
		name     string
		sessions []session.Session
		want     string
	}{
		{"no prior sessions starts fresh", nil, ""},
		{
			"recent session continues",
			[]session.Session{fakeSession{id: "s1", updated: now.Add(-2 * time.Hour)}},
			"s1",
		},
		{
			"stale session starts fresh",
			[]session.Session{fakeSession{id: "s1", updated: now.Add(-25 * time.Hour)}},
			"",
		},
		{
			"most recent wins regardless of list order",
			[]session.Session{
				fakeSession{id: "old", updated: now.Add(-10 * time.Hour)},
				fakeSession{id: "newest", updated: now.Add(-1 * time.Minute)},
				fakeSession{id: "middle", updated: now.Add(-5 * time.Hour)},
			},
			"newest",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := IdleWindowResolver{Now: clock}.Resolve(context.Background(), &SessionRequest{
				AppName:  "my.agent",
				UserID:   UserID("+27836566942"),
				Sessions: fakeSessionService{sessions: tt.sessions},
			})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Fatalf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIdleWindowResolver_BoundaryIsExclusive pins the edge: a session updated
// exactly one window ago is stale, not current.
func TestIdleWindowResolver_BoundaryIsExclusive(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	resolver := IdleWindowResolver{Window: time.Hour, Now: func() time.Time { return now }}

	got, err := resolver.Resolve(context.Background(), &SessionRequest{
		AppName:  "my.agent",
		UserID:   "u",
		Sessions: fakeSessionService{sessions: []session.Session{fakeSession{id: "s1", updated: now.Add(-time.Hour)}}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "" {
		t.Fatalf("Resolve() = %q, want a fresh session", got)
	}
}

func TestIdleWindowResolver_RequiresSessionService(t *testing.T) {
	_, err := IdleWindowResolver{}.Resolve(context.Background(), &SessionRequest{AppName: "a", UserID: "u"})
	if err == nil {
		t.Fatal("expected an error when no session service is supplied")
	}
}

func TestUserID(t *testing.T) {
	if got := UserID("+27836566942"); got != "whatsapp:+27836566942" {
		t.Fatalf("UserID() = %q", got)
	}
}

// fakeSessionService returns a fixed session list.
type fakeSessionService struct {
	session.Service
	sessions []session.Session
}

func (f fakeSessionService) List(context.Context, *session.ListRequest) (*session.ListResponse, error) {
	return &session.ListResponse{Sessions: f.sessions}, nil
}

// fakeSession is a session.Session with only the fields the resolver reads.
type fakeSession struct {
	id      string
	updated time.Time
}

func (f fakeSession) ID() string                { return f.id }
func (f fakeSession) AppName() string           { return "my.agent" }
func (f fakeSession) UserID() string            { return "u" }
func (f fakeSession) State() session.State      { return nil }
func (f fakeSession) Events() session.Events    { return noEvents{} }
func (f fakeSession) LastUpdateTime() time.Time { return f.updated }

type noEvents struct{}

func (noEvents) All() iter.Seq[*session.Event] { return func(func(*session.Event) bool) {} }
func (noEvents) Len() int                      { return 0 }
func (noEvents) At(int) *session.Event         { return nil }
