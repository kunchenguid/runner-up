package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kunchenguid/runner-up/internal/plan"
)

// tokenSource gives the GitHub token for one use. The token stays in memory:
// never in an argument, a log, a file or the environment of a job.
type tokenSource func(ctx context.Context) (string, error)

// GhToken asks the GitHub command line for its sign-in's token, so runner-up
// has no sign-in of its own. gh reads it from the system credential store, or
// from GH_TOKEN or its own file where there is none.
func GhToken(gh string) tokenSource {
	return func(ctx context.Context) (string, error) {
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, gh, "auth", "token", "--hostname", "github.com")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		token := strings.TrimSpace(string(out))
		if err != nil || token == "" {
			var notFound *exec.Error
			if errors.As(err, &notFound) {
				return "", fmt.Errorf("the GitHub command line (%s) is not installed: install it from https://cli.github.com and run gh auth login", gh)
			}
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return "", fmt.Errorf("the GitHub command line has no sign-in for github.com (%s): run gh auth login", msg)
		}
		return token, nil
	}
}

// GithubAPI reads GitHub's REST API with the sign-in's token.
type GithubAPI struct {
	base   string
	token  tokenSource
	client *http.Client
}

func NewGitHubAPI(token tokenSource) *GithubAPI {
	return &GithubAPI{base: "https://api.github.com", token: token,
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// get reads path into v and returns the response's headers.
func (a *GithubAPI) get(ctx context.Context, path string, v any) (http.Header, error) {
	token, err := a.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// GitHub's header names the permissions the endpoint accepts; quote
		// it rather than guess.
		if want := acceptedPermissions(resp.Header); want != "" {
			return resp.Header, fmt.Errorf("GET %s returned HTTP %d; the sign-in needs %s", path, resp.StatusCode, want)
		}
		return resp.Header, fmt.Errorf("GET %s returned HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(v); err != nil {
		return resp.Header, fmt.Errorf("GET %s returned an unreadable answer", path)
	}
	return resp.Header, nil
}

// PullLabels reads the labels of every open pull request whose head is in
// repo itself, by each ref its jobs run on: the pull request's merge and head
// refs and its head branch. A fork's pull request earns nothing.
func (a *GithubAPI) PullLabels(ctx context.Context, repo string) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	add := func(ref, label string) {
		if out[ref] == nil {
			out[ref] = map[string]bool{}
		}
		out[ref][label] = true
	}
	for page := 1; page <= 10; page++ {
		var pulls []struct {
			Number int    `json:"number"`
			State  string `json:"state"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
			Head struct {
				Ref  string `json:"ref"`
				Repo struct {
					FullName string `json:"full_name"`
				} `json:"repo"`
			} `json:"head"`
		}
		if _, err := a.get(ctx, fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100&page=%d", repo, page), &pulls); err != nil {
			return nil, err
		}
		for _, p := range pulls {
			if p.State != "open" || p.Head.Repo.FullName != repo || p.Number <= 0 {
				continue
			}
			pull := "refs/pull/" + strconv.Itoa(p.Number)
			for _, l := range p.Labels {
				add(pull+"/merge", l.Name)
				add(pull+"/head", l.Name)
				if p.Head.Ref != "" {
					add("refs/heads/"+p.Head.Ref, l.Name)
				}
			}
		}
		if len(pulls) < 100 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("pull requests of %s exceeded ten pages", repo)
}

// RunWorkflow reads a workflow run's top-level workflow file, such as
// .github/workflows/release.yml, checking it is the run asked for.
func (a *GithubAPI) RunWorkflow(ctx context.Context, repo string, id int64) (string, error) {
	var run struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if _, err := a.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, id), &run); err != nil {
		return "", err
	}
	p := plan.WorkflowPath(run.Path)
	file := strings.TrimPrefix(p, ".github/workflows/")
	if run.ID != id || run.Repository.FullName != repo || file == p || file == "" || strings.Contains(file, "/") ||
		!(strings.HasSuffix(file, ".yml") || strings.HasSuffix(file, ".yaml")) {
		return "", fmt.Errorf("run %d of %s has no matching workflow file", id, repo)
	}
	return p, nil
}

// SignIn reads who the token signs in as and the scopes GitHub granted it.
func (a *GithubAPI) SignIn(ctx context.Context) (login string, scopes []string, err error) {
	var user struct {
		Login string `json:"login"`
	}
	h, err := a.get(ctx, "/user", &user)
	if err != nil {
		return "", nil, err
	}
	for _, s := range strings.Split(h.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	return user.Login, scopes, nil
}

// RepoAdmin reports whether the sign-in may administer repo, which creating
// its runners needs.
func (a *GithubAPI) RepoAdmin(ctx context.Context, repo string) (bool, error) {
	var r struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if _, err := a.get(ctx, "/repos/"+repo, &r); err != nil {
		return false, err
	}
	return r.Permissions.Admin, nil
}

// acceptedPermissions reads GitHub's X-Accepted-GitHub-Permissions header,
// such as "pull_requests=read", keeping only permission-name characters so a
// response cannot write arbitrary text into the log.
func acceptedPermissions(h http.Header) string {
	v := strings.TrimSpace(h.Get("X-Accepted-GitHub-Permissions"))
	if v == "" || len(v) > 200 {
		return ""
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r == '_' || r == '=' || r == ',' || r == ';' || r == ' ') {
			return ""
		}
	}
	return v
}
