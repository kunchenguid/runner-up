package coordinator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/actions/scaleset"
)

// A session outlives the GitHub token its client was made with: the message
// session keeps its own token. When GitHub refuses the old one, the runner
// still starts and is removed, with a token read again through gh, and the
// session keeps the new client (issue #55: every start failed with 401 until
// a restart).
func TestRefusedTokenIsReadAgainAndTheCallRetried(t *testing.T) {
	var mu sync.Mutex
	valid := "first"
	var refused, removed int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/runners/registration-token"):
			if r.Header.Get("Authorization") != "Bearer "+valid {
				refused++
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"Bad credentials"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"registration-token"}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/tenant/1/", adminToken())
		case strings.HasSuffix(r.URL.Path, "/generatejitconfig"):
			fmt.Fprint(w, `{"runner":{"id":12,"name":"r"},"encodedJITConfig":"jit"}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/agents/12"):
			removed++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// gh hands out the token GitHub takes at the time it is asked.
	reads := 0
	token := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return valid, nil
	}
	clients := func(ctx context.Context, target string) (*scaleset.Client, error) {
		t, err := token(ctx)
		if err != nil {
			return nil, err
		}
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: srv.URL + "/" + target, PersonalAccessToken: t,
			SystemInfo: scaleset.SystemInfo{System: "runner-up", Subsystem: "test"},
		}, scaleset.WithRetry(scaleset.RetryConfig{Max: 0}))
	}
	ctx := context.Background()
	first, err := clients(ctx, "octo-org/app")
	if err != nil {
		t.Fatal(err)
	}
	sc := &setClient{client: first, id: 7, renew: func(ctx context.Context) (*scaleset.Client, error) {
		return clients(ctx, "octo-org/app")
	}}

	// The first token expires while the session is open.
	mu.Lock()
	valid = "second"
	mu.Unlock()
	jit, id, err := sc.GenerateJIT(ctx, "r", "_work")
	if err != nil {
		t.Fatalf("GenerateJIT after the token expired: %v", err)
	}
	if jit != "jit" || id != 12 {
		t.Fatalf("GenerateJIT = %q, %d", jit, id)
	}
	if err := sc.RemoveRunner(ctx, 12); err != nil {
		t.Fatalf("RemoveRunner: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refused != 1 || reads != 2 || removed != 1 {
		t.Fatalf("GitHub refused %d times, gh was asked %d times and %d runners were removed; want 1, 2 and 1",
			refused, reads, removed)
	}
	if sc.client == first {
		t.Fatal("the session kept the client GitHub refused")
	}
}

// A refusal the new token cannot fix is reported, with why the token could
// not be read again, and the call is not retried.
func TestRefusedTokenThatCannotBeReadAgainIsReported(t *testing.T) {
	calls := 0
	client := brokerClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	})
	sc := &setClient{client: client, id: 7, renew: func(context.Context) (*scaleset.Client, error) {
		return nil, fmt.Errorf("gh has no sign-in")
	}}
	err := sc.RemoveRunner(context.Background(), 12)
	if err == nil || !strings.Contains(err.Error(), "gh has no sign-in") {
		t.Fatalf("RemoveRunner = %v, want the refusal and why gh could not sign in", err)
	}
	if calls != 1 {
		t.Fatalf("the removal was sent %d times, want 1", calls)
	}
}
