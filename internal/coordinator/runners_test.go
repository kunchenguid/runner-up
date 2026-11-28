package coordinator

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// A runner that is already gone from its scale set counts as removed, and
// any other failure does not, so its place keeps it until GitHub answers.
// The answer is read by its error type from the scale set client, not by
// searching its text: a refused removal of runner 404 names 404 in its URL.
func TestOnlyAMissingRunnerCountsAsRemoved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		gone   bool
	}{
		{"GitHub names the runner missing", http.StatusNotFound,
			`{"$id":"1","message":"Agent 404 does not exist in pool 1.","typeName":"GitHub.DistributedTask.WebApi.TaskAgentNotFoundException, Sdk"}`, true},
		{"404 Not Found", http.StatusNotFound, `{"message":"Not Found"}`, true},
		{"GitHub refuses", http.StatusForbidden, `{"message":"Forbidden"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := brokerClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || !strings.HasSuffix(r.URL.Path, "/agents/404") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			err := client.RemoveRunner(context.Background(), 404)
			if err == nil {
				t.Fatal("the removal succeeded")
			}
			if got := runnerGone(err); got != tc.gone {
				t.Fatalf("runnerGone(%v) = %v, want %v", err, got, tc.gone)
			}
		})
	}
}

// brokerClient is a scale set client whose GitHub and Actions service are a
// local server: it signs in as the real client does, and broker answers the
// rest.
func brokerClient(t *testing.T, broker http.HandlerFunc) *scaleset.Client {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/runners/registration-token"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"registration-token"}`)
		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			fmt.Fprintf(w, `{"url":%q,"token":%q}`, srv.URL+"/tenant/1/", adminToken(time.Now().Add(time.Hour)))
		default:
			broker(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     srv.URL + "/octo-org/app",
		PersonalAccessToken: "token",
		SystemInfo:          scaleset.SystemInfo{System: "runner-up", Subsystem: "test"},
	}, scaleset.WithRetry(scaleset.RetryConfig{Max: 0}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// adminToken is an Actions service token: the client reads only when it
// expires, without checking its signature.
func adminToken(expires time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := fmt.Sprintf(`{"iss":"1","exp":%d}`, expires.Unix())
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + "." + enc([]byte("unsigned"))
}
