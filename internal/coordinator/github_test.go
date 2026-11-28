package coordinator

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A scale set session outlives the gh token it opened with: a GitHub App
// installation token lasts an hour. Each time the scale set client signs in
// to GitHub again, to renew its Actions service token, it signs in with the
// token gh gives then, so acquiring a job and starting and removing its
// runner keep working on the same session after gh's token changes (#55).
func TestASessionSignsInWithGhsCurrentToken(t *testing.T) {
	var mu sync.Mutex
	current := "ghs_first" // what gh auth token gives, and all GitHub accepts
	var signIns []string
	token := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/runners/registration-token"):
			mu.Lock()
			auth := r.Header.Get("Authorization")
			signIns = append(signIns, auth)
			accepted := auth == "Bearer "+current
			mu.Unlock()
			if !accepted {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"message":"Bad credentials"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"registration-token"}`)
		case strings.HasSuffix(path, "/actions/runner-registration"):
			// Inside the client's minute of margin, so each call signs in
			// again, as the first call after the hour does.
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/tenant/", adminToken(time.Now().Add(30*time.Second)))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/runnerscalesets/1/sessions"):
			fmt.Fprintf(w, `{"sessionId":"6b3c5a1e-8f2d-4c7a-9e1b-2d4f6a8c0e13","ownerName":"runner-up-test","messageQueueUrl":%q,"messageQueueAccessToken":"queue-token","statistics":{}}`,
				srv.URL+"/queue")
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/runnerscalesets/1/acquirejobs"):
			fmt.Fprint(w, `{"count":1,"value":[7]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/runnerscalesets/1/generatejitconfig"):
			fmt.Fprint(w, `{"encodedJITConfig":"jit-config","runner":{"id":42}}`)
		case r.Method == http.MethodDelete && strings.HasSuffix(path, "/pools/0/agents/42"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	client, err := ghClient(srv.URL+"/octo-org/app", token)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.MessageSessionClient(ctx, 1, "runner-up-test")
	if err != nil {
		t.Fatal(err)
	}
	set := &setClient{client: client, session: session, id: 1}

	mu.Lock()
	current = "ghs_second"
	opened := len(signIns)
	mu.Unlock()

	if got, err := set.AcquireJobs(ctx, []int64{7}); err != nil || !slices.Equal(got, []int64{7}) {
		t.Fatalf("acquire after gh's token changed = %v, %v; want [7]", got, err)
	}
	jit, runnerID, err := set.GenerateJIT(ctx, "runner-up-box-1-1", "_work")
	if err != nil || jit != "jit-config" || runnerID != 42 {
		t.Fatalf("runner start after gh's token changed = %q, %d, %v; want jit-config, 42", jit, runnerID, err)
	}
	if err := set.RemoveRunner(ctx, 42); err != nil {
		t.Fatalf("runner removal after gh's token changed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if opened == 0 || signIns[0] != "Bearer ghs_first" {
		t.Fatalf("the session opened with sign-ins %q, want gh's first token", signIns[:opened])
	}
	for _, auth := range signIns[opened:] {
		if auth != "Bearer ghs_second" {
			t.Fatalf("signed in with %q after gh's token changed, want gh's new token", auth)
		}
	}
}
