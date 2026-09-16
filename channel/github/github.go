// Package github is BONNIE's GitHub App channel (L3).
//
// A comment that mentions the bot on an issue or a pull request starts — or
// continues — a durable run bound to that thread, and the run's reply comes
// back as a comment. A review thread is its own conversation, separate from
// the PR's timeline, which is the mapping eve's GitHub channel uses.
//
// What the channel normalises, and where it puts it, is the rule every
// adapter shares (see channel/chat.Turn):
//
//   - Text is the comment with the invocation token removed. It is the one
//     thing that enters the conversation as a user message.
//   - Context is everything else the model should know for this turn: which
//     event fired, who sent it, whether the bot was mentioned, the pull
//     request's title, base, head, and the changed-file patches. It is
//     shown to the model in front of the comment and journalled as its own
//     record, never as history.
//   - Kind is "issue", "pull_request", or "review_thread".
//
// # Addresses
//
// The channel-local address of a conversation is
// "<owner>/<repo>/issues/<n>" for an issue or a PR timeline and
// "<owner>/<repo>/pulls/<n>/reviews/<root-comment-id>" for one review
// thread. The core prefixes the channel's name, so the durable form is
// "github/<owner>/<repo>/issues/42". A PR and its review threads are
// separate runs.
//
// # Credentials
//
// The channel is a GitHub App: it verifies the webhook signature, mints an
// installation token per event, and uses it only to post back and to fetch
// the PR context. The token never enters a run: the runner sees the comment
// text and the context, and nothing else, so a journal replay can never
// leak it. The credentials come from the environment (see the WithGitHub
// option) and a missing one is a startup error naming the variable.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/chat"
	"github.com/mark3labs/bonnie/runtime"
)

// DefaultPath is the webhook path the channel mounts.
const DefaultPath = "/github/events"

// DefaultAPIURL is the GitHub REST API root.
const DefaultAPIURL = "https://api.github.com"

// Limits of the PR context. A patch the model does not need — generated
// files, lock files — is dropped by name, and whatever survives is capped,
// because a comment is a message, not a repository export.
const (
	// DefaultMaxPatchBytes caps the whole PR-context block.
	DefaultMaxPatchBytes = 32 << 10
	// maxDeliveryBody caps a webhook body, the way every adapter does.
	maxDeliveryBody = 1 << 20
	// maxParts is the cap chat.SplitText takes for a reply.
	maxParts = 5
)

// issueLimit is GitHub's comment length. Replies are split to it; a longer
// comment is truncated by GitHub itself with no notice.
const issueLimit = 65536 - 2048

// generatedFiles are file names whose patch is dropped from the PR
// context. The list is the common ones; Config.ExcludedFiles adds more.
var generatedFiles = []string{
	"yarn.lock", "package-lock.json", "pnpm-lock.yaml", "go.sum", "Cargo.lock",
	"Podfile.lock", "composer.lock", "Gemfile.lock", "poetry.lock",
}

// Config configures the GitHub channel.
type Config struct {
	// BotName is the invocation token: a comment containing "@<BotName>"
	// reaches the agent. GitHub does not autocomplete it or render it as a
	// mention; it is a text token.
	BotName string
	// AppID, PrivateKey (PEM), and WebhookSecret are the GitHub App's
	// credentials. They come from the environment through the WithGitHub
	// option.
	AppID         string
	PrivateKey    string
	WebhookSecret string
	// APIURL overrides the REST API root; tests point it at a fake.
	APIURL string
	// ExcludedFiles adds file names whose patch is dropped from the PR
	// context. A name matches when the file's base name equals it or
	// carries it as a suffix (".min.js" and friends).
	ExcludedFiles []string
	// MaxPatchBytes caps the whole PR-context block. The default is
	// DefaultMaxPatchBytes.
	MaxPatchBytes int
	// InstallationID is the installation a proactive turn posts with. A
	// webhook carries its own installation; a hand-off from another
	// channel does not, so proactive use sets this one. It is not a
	// secret, but it comes from the environment like the rest
	// (GITHUB_INSTALLATION_ID) because it is deployment-specific.
	InstallationID int64

	// OnComment gates a comment before it becomes a run. When set, it
	// replaces the default gate: without it a comment reaches the agent
	// when it mentions the bot or continues a thread the agent already
	// joined. Return true to dispatch the turn, false to ignore the
	// comment. Use it to require a repository role, an allowlist, or any
	// other admission rule the platform does not enforce for you — the
	// webhook makes every commenter look the same. CommentCtx carries the
	// mention and boundness the default gate uses, so a hook can keep that
	// behaviour and add to it. The channel still owns the turn it builds:
	// the invocation token is stripped, the PR diff and the sender reach
	// the model as context, and the sender is the run's principal.
	OnComment func(CommentCtx) bool

	// OnIssue, OnPullRequest, and OnCheckSuite are the opt-in hooks for
	// events that are not a comment. Return a turn to dispatch one, or nil
	// to ignore the event. The channel fills Kind, the sender context, and
	// the PR anchor; the hook supplies the instruction, the address when
	// it is not the obvious one, and anything else the model should know.
	//
	// OnIssue and OnPullRequest fire for EVERY action GitHub sends —
	// opened, edited, closed, labeled, assigned, and the rest — so the
	// hook decides which ones matter by reading IssueCtx.Action and the
	// label set. A bot that acts on a triage label returns a turn only
	// when Action is "labeled" and Labels contains that label. The typed
	// fields cover the common case; Raw carries the whole event for
	// anything they omit.
	//
	// Every turn these hooks build also carries a checkout descriptor in
	// its context — the clone URL, the default branch, and a pull
	// request's base and head — so a coding agent can clone the repository
	// and branch from it. See CommentCtx and the package doc for the
	// invariant this respects: the descriptor is public metadata, never a
	// token.
	OnIssue       func(IssueCtx) *chat.Turn
	OnPullRequest func(PullRequestCtx) *chat.Turn
	OnCheckSuite  func(CheckSuiteCtx) *chat.Turn
}

// CommentCtx is what an OnComment gate sees: one comment that mentions the
// bot or lands on a thread the agent already joined, before the channel
// turns it into a run.
type CommentCtx struct {
	Owner, Repo string
	Number      int
	// Kind is "issue", "pull_request", or "review_thread".
	Kind string
	// Sender is the comment author's login.
	Sender string
	// Body is the raw comment, invocation token included.
	Body string
	// Mentioned reports whether the comment names the bot.
	Mentioned bool
	// Bound reports whether the agent already joined this thread.
	Bound bool
}

// IssueCtx is what an OnIssue hook sees. It fires for every issues webhook
// action, so the hook filters on Action and Labels.
type IssueCtx struct {
	Owner, Repo           string
	Number                int
	Title, Action, Sender string
	// State is "open" or "closed".
	State string
	// Body is the issue description, as authored.
	Body string
	// Labels is every label on the issue now.
	Labels []string
	// Label is the one added or removed on a "labeled"/"unlabeled" action,
	// empty on any other action.
	Label string
	// Assignees is every assignee's login.
	Assignees []string
	// Raw is the full webhook event JSON, for a field the typed layer
	// omits. Unmarshal it into a shape of your own.
	Raw json.RawMessage
}

// PullRequestCtx is what an OnPullRequest hook sees. It fires for every
// pull_request webhook action, so the hook filters on Action and Labels.
type PullRequestCtx struct {
	Owner, Repo           string
	Number                int
	Title, Action, Sender string
	// State is "open" or "closed".
	State string
	// Body is the pull request description, as authored.
	Body string
	// Draft reports whether the pull request is a draft.
	Draft bool
	// Labels is every label on the pull request now.
	Labels []string
	// Label is the one added or removed on a "labeled"/"unlabeled" action,
	// empty on any other action.
	Label string
	// BaseRef and HeadRef are the branch names the pull request merges into
	// and from; HeadSHA is the head commit. They name what an agent checks
	// out to work on the change.
	BaseRef, HeadRef, HeadSHA string
	// Raw is the full webhook event JSON, for a field the typed layer
	// omits. Unmarshal it into a shape of your own.
	Raw json.RawMessage
}

// CheckSuiteCtx is what an OnCheckSuite hook sees.
type CheckSuiteCtx struct {
	Owner, Repo    string
	SuiteID        int64
	Conclusion     string
	HeadSHA        string
	PullRequests   []int
	AppSlug        string
	Action, Sender string
}

// Channel is the GitHub transport. It implements [channel.Channel] and
// [channel.Inbound].
type Channel struct {
	core *chat.Core
	cfg  Config
	api  string
	key  *rsa.PrivateKey
	http *http.Client

	seenMu sync.Mutex
	seen   map[string]bool
}

var (
	_ channel.Channel = (*Channel)(nil)
	_ channel.Inbound = (*Channel)(nil)
)

// New returns a GitHub channel over a runner. The config's credentials must
// be set; the WithGitHub option fills them from the environment and refuses
// a mount without them.
func New(r *runtime.Runner, cfg Config) (*Channel, error) {
	if cfg.BotName == "" {
		return nil, errors.New("bonnie: channel/github: BotName is required: it is the invocation token a comment must contain")
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultAPIURL
	}
	if cfg.MaxPatchBytes == 0 {
		cfg.MaxPatchBytes = DefaultMaxPatchBytes
	}
	key, err := parseKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("bonnie: channel/github: %w", err)
	}
	return &Channel{
		core: chat.NewCore(r, "github", channel.PolicySteer),
		cfg:  cfg,
		api:  strings.TrimSuffix(cfg.APIURL, "/"),
		key:  key,
		http: &http.Client{Timeout: 30 * time.Second},
		seen: make(map[string]bool),
	}, nil
}

// parseKey decodes a PEM-encoded PKCS#1 or PKCS#8 RSA private key.
func parseKey(pemKey string) (*rsa.PrivateKey, error) {
	if pemKey == "" {
		return nil, errors.New("the private key is empty")
	}
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return nil, errors.New("the private key is not PEM-encoded")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the private key is neither PKCS#1 nor PKCS#8 RSA")
	}
	rsaKey, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not RSA")
	}
	return rsaKey, nil
}

// Name implements [channel.Channel].
func (c *Channel) Name() string { return "github" }

// Routes implements [channel.Channel].
func (c *Channel) Routes() []channel.Route {
	return []channel.Route{
		{Method: http.MethodPost, Path: DefaultPath, Handler: c.handleEvent},
	}
}

// From implements [channel.Inbound].
func (c *Channel) From(address string) channel.SessionRef { return c.core.From(address) }

// Attach implements [channel.Inbound].
func (c *Channel) Attach(runID string) channel.SessionRef { return c.core.Attach(runID) }

// ---------------------------------------------------------------------------
// Webhook
// ---------------------------------------------------------------------------

// envelope is the part of every delivery the channel reads before it knows
// which event it is.
type envelope struct {
	Action       string        `json:"action"`
	Sender       actor         `json:"sender"`
	Repository   repo          `json:"repository"`
	Installation *installation `json:"installation"`
	Issue        *issue        `json:"issue"`
	PullRequest  *pullRequest  `json:"pull_request"`
	Comment      *comment      `json:"comment"`
	CheckSuite   *checkSuite   `json:"check_suite"`
	// Label is the label added or removed on a "labeled"/"unlabeled"
	// action. Issue.Labels and PullRequest.Labels carry the full set; this
	// is the one the event turned on.
	Label *label `json:"label"`

	// raw is the exact webhook body, handed to a hook as IssueCtx.Raw or
	// PullRequestCtx.Raw so it can read a field the typed layer omits. It
	// is not a JSON field: handleEvent sets it after unmarshalling, so a
	// hook can act on anything GitHub sends now or later without the
	// channel growing a field for it first.
	raw json.RawMessage
}

type actor struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type repo struct {
	Owner actor  `json:"owner"`
	Name  string `json:"name"`
	// FullName, DefaultBranch, CloneURL, and Private come straight off the
	// webhook's repository object. They feed the checkout descriptor the
	// channel puts in a turn's context, so a coding agent knows what to
	// clone and which branch to base work on without a second API call.
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	Private       bool   `json:"private"`
}

// label is one issue or pull-request label. Only the name reaches a hook.
type label struct {
	Name string `json:"name"`
}

// ref is one side of a pull request: a branch name and the commit it points
// at. The checkout descriptor names both so an agent can fetch the head.
type ref struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type installation struct {
	ID int64 `json:"id"`
}

type issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	Body        string    `json:"body"`
	Labels      []label   `json:"labels"`
	Assignees   []actor   `json:"assignees"`
	HTMLURL     string    `json:"html_url"`
	PullRequest *struct{} `json:"pull_request"`
}

type pullRequest struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	State   string  `json:"state"`
	Body    string  `json:"body"`
	Draft   bool    `json:"draft"`
	Labels  []label `json:"labels"`
	HTMLURL string  `json:"html_url"`
	Base    ref     `json:"base"`
	Head    ref     `json:"head"`
}

type comment struct {
	ID                  int64  `json:"id"`
	Body                string `json:"body"`
	PullRequestReviewID int64  `json:"pull_request_review_id"`
	InReplyToID         int64  `json:"in_reply_to_id"`
}

type checkSuite struct {
	ID           int64         `json:"id"`
	Conclusion   string        `json:"conclusion"`
	HeadSHA      string        `json:"head_sha"`
	App          actor         `json:"app"`
	PullRequests []pullRequest `json:"pull_requests"`
}

func (c *Channel) handleEvent(w http.ResponseWriter, r *http.Request, _ channel.Inbound, _ channel.Outbound) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDeliveryBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !c.verify(r.Header.Get("X-Hub-Signature-256"), body) {
		// Not GitHub. A probe learns only that the door did not open.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)

	// GitHub redelivers when an endpoint is slow to acknowledge, so the
	// same delivery can arrive twice. A retried delivery must not run the
	// turn again — and a retried /new must not retire the run that
	// replaced the one it meant.
	if !c.claim(r.Header.Get("X-GitHub-Delivery")) {
		return
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return
	}
	// Keep the exact body so a hook can read a field the typed layer omits.
	env.raw = body
	// Bots never trigger the agent: the channel's own replies are comments
	// too, and the loop must not close.
	if strings.HasSuffix(env.Sender.Login, "[bot]") || env.Sender.Type == "Bot" {
		return
	}

	turn, reaction := c.normalise(&env)
	if turn == nil {
		return
	}
	c.react(context.WithoutCancel(r.Context()), &env, reaction)
	// The envelope is captured by the delivery closure: the reply needs the
	// installation and the thread, and the webhook handler is the one place
	// both are known.
	chat.Dispatch(r.Context(), c.core, *turn, func(address string, run *runtime.Run, err error) {
		c.deliver(&env, address, run, err)
	})
}

// verify checks the X-Hub-Signature-256 header: HMAC-SHA256 over the raw
// body, keyed by the webhook secret, hex-encoded with a "sha256=" prefix.
func (c *Channel) verify(signature string, body []byte) bool {
	if c.cfg.WebhookSecret == "" {
		return true // unverified: the host accepts the risk by configuring so
	}
	mac := hmac.New(sha256.New, []byte(c.cfg.WebhookSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// claim records a delivery ID and reports whether this call is the first.
// The map is bounded and process-local: a restart forgets it, and a
// duplicate delivery after a restart is a steered message at worst, not a
// lost one.
func (c *Channel) claim(deliveryID string) bool {
	if deliveryID == "" {
		return true
	}
	c.seenMu.Lock()
	defer c.seenMu.Unlock()
	if c.seen[deliveryID] {
		return false
	}
	if len(c.seen) >= 4096 {
		c.seen = make(map[string]bool)
	}
	c.seen[deliveryID] = true
	return true
}

// normalise turns one delivery into a turn, or nil when the event is not
// for the agent. It also returns the comment ID to acknowledge with an
// eyes reaction, 0 when there is none.
func (c *Channel) normalise(env *envelope) (*chat.Turn, int64) {
	owner, repoName := env.Repository.Owner.Login, env.Repository.Name
	if owner == "" || repoName == "" {
		return nil, 0
	}

	switch {
	case env.Comment != nil && env.Issue != nil:
		return c.turnForComment(env, owner, repoName)

	case env.Comment != nil && env.PullRequest != nil:
		return c.turnForReviewComment(env, owner, repoName)

	case env.Issue != nil:
		if c.cfg.OnIssue == nil {
			return nil, 0
		}
		turn := c.cfg.OnIssue(IssueCtx{
			Owner: owner, Repo: repoName, Number: env.Issue.Number,
			Title: env.Issue.Title, Action: env.Action, Sender: env.Sender.Login,
			State: env.Issue.State, Body: env.Issue.Body,
			Labels: labelNames(env.Issue.Labels), Label: labelName(env.Label),
			Assignees: logins(env.Issue.Assignees), Raw: env.raw,
		})
		if turn != nil && turn.Address == "" {
			turn.Address = AddressIssue(owner, repoName, env.Issue.Number)
		}
		return c.finishHooked(turn, env, owner, repoName, chat.KindIssue), 0

	case env.PullRequest != nil:
		if c.cfg.OnPullRequest == nil {
			return nil, 0
		}
		turn := c.cfg.OnPullRequest(PullRequestCtx{
			Owner: owner, Repo: repoName, Number: env.PullRequest.Number,
			Title: env.PullRequest.Title, Action: env.Action, Sender: env.Sender.Login,
			State: env.PullRequest.State, Body: env.PullRequest.Body, Draft: env.PullRequest.Draft,
			Labels: labelNames(env.PullRequest.Labels), Label: labelName(env.Label),
			BaseRef: env.PullRequest.Base.Ref, HeadRef: env.PullRequest.Head.Ref,
			HeadSHA: env.PullRequest.Head.SHA, Raw: env.raw,
		})
		if turn != nil && turn.Address == "" {
			turn.Address = AddressPullRequest(owner, repoName, env.PullRequest.Number)
		}
		return c.finishHooked(turn, env, owner, repoName, chat.KindPullRequest), 0

	case env.CheckSuite != nil && env.Action == "completed":
		if c.cfg.OnCheckSuite == nil {
			return nil, 0
		}
		numbers := make([]int, len(env.CheckSuite.PullRequests))
		for i, pr := range env.CheckSuite.PullRequests {
			numbers[i] = pr.Number
		}
		turn := c.cfg.OnCheckSuite(CheckSuiteCtx{
			Owner: owner, Repo: repoName, SuiteID: env.CheckSuite.ID,
			Conclusion: env.CheckSuite.Conclusion, HeadSHA: env.CheckSuite.HeadSHA,
			PullRequests: numbers, AppSlug: env.CheckSuite.App.Login,
			Action: env.Action, Sender: env.Sender.Login,
		})
		if turn == nil {
			return nil, 0
		}
		if len(numbers) == 0 {
			// A suite with no pull request has no thread to anchor the
			// session to; dispatching into nowhere is worse than dropping.
			return nil, 0
		}
		if turn.Address == "" {
			turn.Address = AddressPullRequest(owner, repoName, numbers[0])
		}
		return c.finishHooked(turn, env, owner, repoName, chat.KindPullRequest), 0

	default:
		return nil, 0
	}
}

// finishHooked fills the parts a hook does not own: the kind, the event
// context, and the checkout descriptor a coding agent works from.
func (c *Channel) finishHooked(turn *chat.Turn, env *envelope, owner, repoName, kind string) *chat.Turn {
	if turn == nil {
		return nil
	}
	turn.Kind = kind
	turn.Context = append(turn.Context,
		"GitHub event "+env.Action+" on "+owner+"/"+repoName+", triggered by "+env.Sender.Login+".")
	if line := repoCheckoutLine(env); line != "" {
		turn.Context = append(turn.Context, line)
	}
	return turn
}

// labelNames projects a label list to its names, dropping empties.
func labelNames(ls []label) []string {
	if len(ls) == 0 {
		return nil
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

// labelName is the name of the label an event turned on, empty when the
// event carries none.
func labelName(l *label) string {
	if l == nil {
		return ""
	}
	return l.Name
}

// logins projects an actor list to their logins, dropping empties.
func logins(as []actor) []string {
	if len(as) == 0 {
		return nil
	}
	out := make([]string, 0, len(as))
	for _, a := range as {
		if a.Login != "" {
			out = append(out, a.Login)
		}
	}
	return out
}

// repoCheckoutLine is the checkout descriptor the channel adds to a turn's
// context: the clone URL, the default branch, and — for a pull request — its
// base and head. It is public repository metadata, never a token, so it is
// safe in a journalled record that a replay re-injects. A private repository
// still needs auth to clone; the descriptor names what to fetch, not how the
// egress is authenticated. It returns "" when the repository is unknown.
func repoCheckoutLine(env *envelope) string {
	r := env.Repository
	url := r.CloneURL
	if url == "" && r.Owner.Login != "" && r.Name != "" {
		url = "https://github.com/" + r.Owner.Login + "/" + r.Name + ".git"
	}
	if url == "" {
		return ""
	}
	line := "Repository checkout: clone " + url
	if r.DefaultBranch != "" {
		line += " (default branch " + r.DefaultBranch + ")"
	}
	if pr := env.PullRequest; pr != nil && (pr.Base.Ref != "" || pr.Head.Ref != "") {
		line += fmt.Sprintf(". Pull request base %s, head %s", pr.Base.Ref, pr.Head.Ref)
		if pr.Head.SHA != "" {
			line += " at " + pr.Head.SHA
		}
		line += "."
	}
	return line
}

// turnForComment normalises an issue or PR-timeline comment.
func (c *Channel) turnForComment(env *envelope, owner, repoName string) (*chat.Turn, int64) {
	body := env.Comment.Body
	mentioned := mentionsBot(body, c.cfg.BotName)
	isPR := env.Issue.PullRequest != nil

	kind := chat.KindIssue
	address := AddressIssue(owner, repoName, env.Issue.Number)
	if isPR {
		// A pull request is also an issue on GitHub, but its conversation
		// is the PR's: the address names the PR, so a pull_request.opened
		// hook and a comment land on the same run.
		kind = chat.KindPullRequest
		address = AddressPullRequest(owner, repoName, env.Issue.Number)
	}

	// Boundness is asked of the address this comment will use. Asking the
	// issue address for a PR comment says "not bound" however long the
	// agent has been in that PR, and the mention-free follow-up that
	// continues the conversation is then dropped.
	_, bound, _ := c.core.Lookup(context.Background(), address)
	if !c.admit(CommentCtx{
		Owner: owner, Repo: repoName, Number: env.Issue.Number, Kind: kind,
		Sender: env.Sender.Login, Body: body, Mentioned: mentioned, Bound: bound,
	}) {
		// A comment in a thread the agent never joined, with no mention,
		// is not for it. Everything in a repository is not its business.
		return nil, 0
	}
	return &chat.Turn{
		Address: address,
		Text:    stripInvocation(body, c.cfg.BotName),
		Kind:    kind,
		Title:   titleFor(env.Issue.Title, isPR),
		Context: c.contextFor(env, owner, repoName, mentioned, isPR),
		Auth: &channel.Principal{
			Authenticator: "github",
			Kind:          "user",
			ID:            env.Sender.Login,
			Attributes: map[string]any{
				"repository": owner + "/" + repoName,
				"number":     env.Issue.Number,
			},
		},
	}, env.Comment.ID
}

// turnForReviewComment normalises a comment in one review thread. The
// thread is its own conversation; the PR timeline is a different one.
func (c *Channel) turnForReviewComment(env *envelope, owner, repoName string) (*chat.Turn, int64) {
	root := env.Comment.InReplyToID
	if root == 0 {
		root = env.Comment.ID
	}
	address := AddressReview(owner, repoName, env.PullRequest.Number, root)
	mentioned := mentionsBot(env.Comment.Body, c.cfg.BotName)
	_, bound, _ := c.core.Lookup(context.Background(), address)
	if !c.admit(CommentCtx{
		Owner: owner, Repo: repoName, Number: env.PullRequest.Number, Kind: chat.KindReviewThread,
		Sender: env.Sender.Login, Body: env.Comment.Body, Mentioned: mentioned, Bound: bound,
	}) {
		return nil, 0
	}
	return &chat.Turn{
		Address: address,
		Text:    stripInvocation(env.Comment.Body, c.cfg.BotName),
		Kind:    chat.KindReviewThread,
		Title:   titleFor(env.PullRequest.Title, true),
		Context: c.contextFor(env, owner, repoName, mentioned, true),
		Auth: &channel.Principal{
			Authenticator: "github",
			Kind:          "user",
			ID:            env.Sender.Login,
			Attributes: map[string]any{
				"repository":    owner + "/" + repoName,
				"number":        env.PullRequest.Number,
				"review_thread": root,
			},
		},
	}, env.Comment.ID
}

// admit decides whether a comment becomes a run. OnComment, when set,
// replaces the default gate; otherwise a comment is admitted when it
// mentions the bot or continues a thread the agent already joined.
func (c *Channel) admit(cc CommentCtx) bool {
	if c.cfg.OnComment != nil {
		return c.cfg.OnComment(cc)
	}
	return cc.Mentioned || cc.Bound
}

// AddressIssue is the channel-local address of an issue or a PR timeline.
func AddressIssue(owner, repo string, number int) string {
	return owner + "/" + repo + "/issues/" + strconv.Itoa(number)
}

// AddressPullRequest is the channel-local address of a PR timeline.
func AddressPullRequest(owner, repo string, number int) string {
	return owner + "/" + repo + "/pulls/" + strconv.Itoa(number)
}

// AddressReview is the channel-local address of one review thread: the
// root comment's ID is the thread's identity.
func AddressReview(owner, repo string, number int, rootCommentID int64) string {
	return owner + "/" + repo + "/pulls/" + strconv.Itoa(number) + "/reviews/" + strconv.FormatInt(rootCommentID, 10)
}

// mentionsBot reports whether the comment contains "@<bot>" as a word. The
// token is not a GitHub mention: GitHub may not autocomplete or link it, so
// the match is textual and case-insensitive, and the token need not be
// first.
func mentionsBot(body, botName string) bool {
	return botName != "" && containsToken(body, "@"+botName)
}

// stripInvocation removes every "@<bot>" token from the comment, leaving
// what the person actually said.
func stripInvocation(body, botName string) string {
	return strings.TrimSpace(cutToken(body, "@"+botName))
}

// containsToken reports whether needle appears in haystack as a standalone
// word: not preceded or followed by a letter, digit, or underscore, and
// matched case-insensitively. strings has no case-insensitive Contains, so
// this walks the haystack.
func containsToken(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	for i := 0; i+len(n) <= len(h); i++ {
		if !equalFoldRunes(h[i:i+len(n)], n) {
			continue
		}
		if i > 0 && wordRune(h[i-1]) {
			continue
		}
		if i+len(n) < len(h) && wordRune(h[i+len(n)]) {
			continue
		}
		return true
	}
	return false
}

// cutToken removes every word-bounded occurrence of needle from haystack.
func cutToken(haystack, needle string) string {
	var b strings.Builder
	h, n := []rune(haystack), []rune(needle)
	i := 0
	for i < len(h) {
		if i+len(n) <= len(h) && equalFoldRunes(h[i:i+len(n)], n) &&
			(i == 0 || !wordRune(h[i-1])) &&
			(i+len(n) == len(h) || !wordRune(h[i+len(n)])) {
			i += len(n)
			continue
		}
		b.WriteRune(h[i])
		i++
	}
	return b.String()
}

func equalFoldRunes(a, b []rune) bool {
	for i := range a {
		if strings.EqualFold(string(a[i]), string(b[i])) {
			continue
		}
		return false
	}
	return true
}

func wordRune(r rune) bool {
	return r == '_' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')
}

// titleFor is the run title: the issue or PR title, named for what it is.
func titleFor(title string, isPR bool) string {
	if isPR {
		return "PR: " + title
	}
	return "Issue: " + title
}

// contextFor builds the per-turn context of a comment: the event, the
// sender, whether the bot was mentioned, the repository checkout, and — for
// a PR — the diff.
func (c *Channel) contextFor(env *envelope, owner, repoName string, mentioned, isPR bool) []string {
	lines := []string{
		fmt.Sprintf("GitHub %s comment on %s/%s#%d by %s; the agent was %s.",
			eventKind(env), owner, repoName, number(env), env.Sender.Login, mentionedOrNot(mentioned)),
	}
	if line := repoCheckoutLine(env); line != "" {
		lines = append(lines, line)
	}
	if isPR {
		lines = append(lines, c.pullRequestContext(env, owner, repoName, number(env))...)
	}
	return lines
}

func eventKind(env *envelope) string {
	switch {
	case env.PullRequest != nil && env.Comment != nil:
		return "pull_request_review_comment"
	case env.Comment != nil:
		return "issue_comment"
	default:
		return "event"
	}
}

func number(env *envelope) int {
	switch {
	case env.Issue != nil:
		return env.Issue.Number
	case env.PullRequest != nil:
		return env.PullRequest.Number
	default:
		return 0
	}
}

func mentionedOrNot(m bool) string {
	if m {
		return "mentioned"
	}
	return "already in the conversation"
}

// ---------------------------------------------------------------------------
// Pull-request context
// ---------------------------------------------------------------------------

// prDetails is the part of GET /pulls/{n} the context needs.
type prDetails struct {
	Title string `json:"title"`
	Base  struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	ChangedFiles int `json:"changed_files"`
}

// prFile is one entry of GET /pulls/{n}/files.
type prFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

// pullRequestContext fetches the PR's title, base, head, and changed-file
// patches. A file whose name is excluded contributes its name but not its
// patch — eve drops large generated files the same way — and the whole
// block is capped at Config.MaxPatchBytes, so a repository-sized diff
// cannot become a conversation-sized message. Failures degrade to nothing:
// a slow API must not fail a comment.
func (c *Channel) pullRequestContext(env *envelope, owner, repoName string, n int) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	token, err := c.installToken(ctx, env)
	if err != nil {
		return nil
	}
	var d prDetails
	if err := c.get(ctx, token, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repoName, n), &d); err != nil {
		return nil
	}
	var files []prFile
	if err := c.get(ctx, token, fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repoName, n), &files); err != nil {
		files = nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Pull request %q: %s -> %s, %d changed files.", d.Title, d.Base.Ref, d.Head.Ref, d.ChangedFiles)
	budget := c.cfg.MaxPatchBytes
	for _, f := range files {
		if budget <= 0 {
			fmt.Fprintf(&b, "\n(%d more files not shown: the context budget is spent.)", len(files))
			break
		}
		if c.excluded(f.Filename) || f.Patch == "" {
			fmt.Fprintf(&b, "\n%s (patch not shown)", f.Filename)
			continue
		}
		patch := f.Patch
		if len(patch) > budget {
			patch = patch[:budget]
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s", f.Filename, patch)
		budget -= len(patch)
	}
	return []string{b.String()}
}

// excluded reports whether a file's patch is dropped from the context.
func (c *Channel) excluded(filename string) bool {
	base := filename
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	for _, pattern := range append(append([]string{}, generatedFiles...), c.cfg.ExcludedFiles...) {
		if base == pattern || strings.HasSuffix(filename, "/"+pattern) || strings.HasSuffix(base, pattern) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Authentication and GitHub API
// ---------------------------------------------------------------------------

// installToken mints an installation access token for one event. The token
// lives for this call and is dropped with it; it never reaches a run, a
// journal record, or a log line.
func (c *Channel) installToken(ctx context.Context, env *envelope) (string, error) {
	if env.Installation == nil {
		return "", errors.New("the delivery carries no installation")
	}
	jwt, err := c.appJWT()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.api+fmt.Sprintf("/app/installations/%d/access_tokens", env.Installation.ID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("installation token: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// appJWT signs a GitHub App JWT: RS256, ten minutes of life, sixty seconds
// of clock-slack on the issued-at claim.
func (c *Channel) appJWT() (string, error) {
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`))
	now := time.Now().Unix()
	appID, err := strconv.ParseInt(strings.TrimSpace(c.cfg.AppID), 10, 64)
	if err != nil {
		return "", fmt.Errorf("the app ID %q is not a number", c.cfg.AppID)
	}
	claims, err := json.Marshal(map[string]int64{"iat": now - 60, "exp": now + 540, "iss": appID})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64URL(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64URL(sig), nil
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// postComment posts one comment on the surface the conversation lives on.
// A failed post is logged, never retried in a loop — the run's result is in
// the journal, and `bonnie runs show` reads it back.
func (c *Channel) postComment(ctx context.Context, env *envelope, path, text string) {
	token, err := c.installToken(ctx, env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: channel/github: deliver: %v\n", err)
		return
	}
	payload, _ := json.Marshal(map[string]string{"body": text})
	owner, repoName := env.Repository.Owner.Login, env.Repository.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.api+"/repos/"+owner+"/"+repoName+path, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: channel/github: deliver: %v\n", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bonnie: channel/github: deliver: %v\n", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		fmt.Fprintf(os.Stderr, "bonnie: channel/github: deliver: %s: %s\n", resp.Status, strings.TrimSpace(string(b)))
	}
}

// react acknowledges a triggering comment with an eyes reaction, so the
// person who typed knows the agent took it. Fire-and-log, like delivery.
func (c *Channel) react(ctx context.Context, env *envelope, commentID int64) {
	if commentID == 0 {
		return
	}
	token, err := c.installToken(ctx, env)
	if err != nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"content": "eyes"})
	owner, repoName := env.Repository.Owner.Login, env.Repository.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d/reactions", c.api, owner, repoName, commentID),
		bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// deliver posts a turn's outcome back to the thread it came from. The
// address encodes the target; the envelope carries the installation and
// the repository, both known when the webhook was handled.
func (c *Channel) deliver(env *envelope, address string, run *runtime.Run, err error) {
	text := chat.DeliveryText(run, err, "(Reply in this thread to answer.)")
	if text == "" {
		return
	}
	owner, repoName := env.Repository.Owner.Login, env.Repository.Name
	path, ok := deliveryPath(address, owner, repoName)
	if !ok {
		return
	}
	for _, part := range chat.SplitText(text, issueLimit, maxParts) {
		c.postComment(context.Background(), env, path, part)
	}
}

// deliveryPath maps an address to the API path its reply is posted to: the
// issue's or PR's timeline comment endpoint, or the review thread's replies
// endpoint.
func deliveryPath(address, owner, repoName string) (string, bool) {
	local := strings.TrimPrefix(address, "github/")
	rest := strings.TrimPrefix(local, owner+"/"+repoName+"/")
	switch {
	case strings.HasPrefix(rest, "pulls/"):
		body := strings.TrimPrefix(rest, "pulls/")
		nStr, tail, _ := strings.Cut(body, "/")
		n, err := strconv.Atoi(nStr)
		if err != nil {
			return "", false
		}
		if tail == "" {
			// Timeline comments on a pull request are issue comments on
			// GitHub's API; /pulls/{n}/comments is for review comments.
			return fmt.Sprintf("/issues/%d/comments", n), true
		}
		threadStr, ok := strings.CutPrefix(tail, "reviews/")
		if !ok {
			return "", false
		}
		thread, err := strconv.ParseInt(threadStr, 10, 64)
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("/pulls/%d/comments/%d/replies", n, thread), true
	case strings.HasPrefix(rest, "issues/"):
		n, err := strconv.Atoi(strings.TrimPrefix(rest, "issues/"))
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("/issues/%d/comments", n), true
	default:
		return "", false
	}
}

// Target is what a proactive GitHub turn anchors to: an issue's or a
// pull request's timeline. The reply is posted there as a comment.
type Target struct {
	Owner, Repo string
	Number      int
	// PullRequest says the number names a pull request, not an issue. It
	// decides the address, and the address decides whether a later comment
	// continues this run: an inbound PR comment is addressed
	// [AddressPullRequest], so a hand-off that bound [AddressIssue] for the
	// same number would be answered by a second, separate run. GitHub
	// numbers issues and PRs in one sequence and the API cannot tell them
	// apart from the number, so the caller says which.
	PullRequest bool
}

// Receive implements [channel.Receiver]. The target is a [Target]: the
// address binds to the issue's or PR's timeline before the turn runs, and
// the reply lands there as a comment. No root message is posted — the
// reply is the conversation's visible start. A webhook carries its own
// installation; a hand-off does not, so proactive use sets
// [Config.InstallationID], and its absence is an error, not a silent
// no-op.
func (c *Channel) Receive(ctx context.Context, target any, text string, opts channel.SendOptions) error {
	t, ok := target.(Target)
	if !ok || t.Owner == "" || t.Repo == "" || t.Number == 0 {
		return fmt.Errorf("bonnie: channel/github: the target of a hand-off is a github.Target{Owner, Repo, Number}, not %T", target)
	}
	if c.cfg.InstallationID == 0 {
		return errors.New("bonnie: channel/github: proactive turns need GITHUB_INSTALLATION_ID: a webhook carries its own, a hand-off does not")
	}
	// The address must be the one an inbound comment on this surface
	// resolves to, or the reply to the hand-off starts a second run.
	address, kind := AddressIssue(t.Owner, t.Repo, t.Number), chat.KindIssue
	if t.PullRequest {
		address, kind = AddressPullRequest(t.Owner, t.Repo, t.Number), chat.KindPullRequest
	}
	// The delivery needs an installation and a repository; a hand-off has
	// no webhook to take them from, so the config supplies both.
	env := &envelope{
		Repository:   repo{Owner: actor{Login: t.Owner}, Name: t.Repo},
		Installation: &installation{ID: c.cfg.InstallationID},
	}
	return c.core.Proactive(ctx, chat.Turn{
		Address:    address,
		Text:       text,
		Kind:       kind,
		Context:    opts.Context,
		Title:      opts.Title,
		TurnPolicy: opts.TurnPolicy,
		Auth:       opts.Auth,
	}, func(delivered string, run *runtime.Run, err error) {
		c.deliver(env, delivered, run, err)
	})
}

// get fetches a GitHub API resource with an installation token.
func (c *Channel) get(ctx context.Context, token, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
