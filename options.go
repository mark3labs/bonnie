package bonnie

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/discord"
	"github.com/mark3labs/bonnie/channel/github"
	"github.com/mark3labs/bonnie/channel/slack"
	"github.com/mark3labs/bonnie/channel/telegram"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Channel is an inbound transport BONNIE can mount: the HTTP routes it needs
// and the inbound surface a run resolves through. Every adapter in
// channel/ is both.
type Channel interface {
	channel.Channel
	channel.Inbound
}

// ChannelFunc builds a channel for the runner that serves it. It is called
// once at start; an error stops the process before it listens.
type ChannelFunc func(*runtime.Runner) (Channel, error)

// config is the resolved configuration of one [Agent]. Every field has a
// default from the scaffolded layout; an [Option] replaces one.
type config struct {
	addr      string
	name      string
	journal   string
	model     string
	prompt    string
	instrPath string
	workspace string
	sandbox   sandbox.Provider
	network   *sandbox.NetworkPolicy
	tools     []kit.Tool
	kitOpts   []kit.Option
	channels  []ChannelFunc
	factory   runtime.AgentFactory
	shutdown  time.Duration
	listener  net.Listener
	quiet     bool
}

// Option configures [New]. This is where a setting that is not a
// file in the tree lives: the model, a sandbox, an extra channel. A setting
// that does not exist is a compile error, which is the point.
type Option func(*config)

// defaults returns the configuration of a scaffolded tree with no options.
func defaults() *config {
	return &config{
		addr:      DefaultAddr,
		journal:   DefaultJournal,
		instrPath: DefaultInstructions,
		workspace: DefaultWorkspace,
		shutdown:  30 * time.Second,
	}
}

// WithAddr binds the HTTP channel to addr instead of [DefaultAddr].
func WithAddr(addr string) Option {
	return func(c *config) {
		if addr != "" {
			c.addr = addr
		}
	}
}

// WithJournal writes the run journal to dir instead of [DefaultJournal].
func WithJournal(dir string) Option {
	return func(c *config) {
		if dir != "" {
			c.journal = dir
		}
	}
}

// WithModel selects the model, as "provider/name". Without it, Kit's default
// applies.
func WithModel(model string) Option {
	return func(c *config) { c.model = model }
}

// WithName names the agent. The name is reported by `GET /bonnie/v1/info`
// and nowhere else today; it is for a client that talks to several agents.
func WithName(name string) Option {
	return func(c *config) { c.name = name }
}

// WithSystemPrompt sets the system prompt directly, instead of reading the
// tree's instructions file. It wins over [WithInstructions].
func WithSystemPrompt(prompt string) Option {
	return func(c *config) { c.prompt = prompt }
}

// WithInstructions reads the system prompt from path instead of
// [DefaultInstructions]. An empty path means the agent has no instructions
// file, which is how a host with no tree runs.
func WithInstructions(path string) Option {
	return func(c *config) { c.instrPath = path }
}

// WithWorkspace roots the agent's files at dir instead of
// [DefaultWorkspace]. An empty dir means no workspace: the process's own
// directory stays the root, which is how a host with no tree runs.
//
// The workspace is what keeps a model's write off the tree itself — the
// instructions, the journal, and the source beside them.
func WithWorkspace(dir string) Option {
	return func(c *config) { c.workspace = dir }
}

// WithSandbox runs every tool call in p instead of in this process.
//
// Without it, a model-chosen tool call has this process's files, network, and
// credentials, and [Agent.Run] says so at startup. See docs/SANDBOX.md.
func WithSandbox(p sandbox.Provider) Option {
	return func(c *config) { c.sandbox = p }
}

// WithNetwork constrains what the sandbox may reach. It needs a sandbox: a
// policy with nothing to enforce it is refused at startup, never stored and
// ignored.
func WithNetwork(p sandbox.NetworkPolicy) Option {
	return func(c *config) { c.network = &p }
}

// WithTools adds tools to the set the model may call, beside the tools
// codegen discovered under tools/ and Kit's core set.
func WithTools(tools ...kit.Tool) Option {
	return func(c *config) { c.tools = append(c.tools, tools...) }
}

// WithKit passes Kit options through to the agent, for settings BONNIE does
// not name itself.
func WithKit(opts ...kit.Option) Option {
	return func(c *config) { c.kitOpts = append(c.kitOpts, opts...) }
}

// WithAgentFactory replaces the model-backed agent entirely with one the host
// builds itself. It is the escape hatch for a program that implements
// [runtime.Agent] — a test double, a router, a second framework — and wants
// BONNIE only for durability and transport.
//
// It cannot be combined with the options that configure the agent BONNIE
// would have built ([WithModel], [WithSystemPrompt], [WithSandbox],
// [WithNetwork], [WithTools], [WithKit]): the factory owns the agent, so those
// settings would be accepted and ignored. [Agent.Run] refuses instead, naming
// both.
//
// What the factory owns, it owns completely: the tree's instructions and the
// tools codegen discovered do not reach it either. They are available through
// [Registered] for a host that wants them. The journal, the workspace, the
// channels, and the shutdown behaviour are unaffected — those are BONNIE's
// side of the boundary.
func WithAgentFactory(f runtime.AgentFactory) Option {
	return func(c *config) { c.factory = f }
}

// WithChannel mounts another inbound transport beside the HTTP channel.
func WithChannel(f ChannelFunc) Option {
	return func(c *config) { c.channels = append(c.channels, f) }
}

// WithShutdownTimeout is how long [Agent.Run] waits for in-flight turns to reach a
// checkpoint after a signal. A turn that is cut short still keeps its
// finished steps — the journal is what survives — but a clean stop is
// cheaper.
func WithShutdownTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.shutdown = d
		}
	}
}

// WithListener serves on an already-bound listener instead of dialling the
// configured address. Tests bind :0 with it and learn the port.
func WithListener(ln net.Listener) Option {
	return func(c *config) { c.listener = ln }
}

// Quiet suppresses the startup banner. The no-sandbox warning is printed
// anyway: what runs unisolated must never be quieter than what does not.
func Quiet() Option {
	return func(c *config) { c.quiet = true }
}

// WithSlack mounts the Slack channel. Credentials come from the environment
// and never from code: SLACK_BOT_TOKEN and SLACK_SIGNING_SECRET fill the
// config's empty fields, and a missing one is a startup error naming the
// variable. A webhook that does not verify its caller is a door with no lock.
func WithSlack(cfg slack.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		// Copy before filling. The closure outlives the call, so writing the
		// environment into the captured config would make the option
		// single-use: a second agent built from it would reuse the first
		// call's credentials instead of reading the environment again.
		c := cfg
		fill(&c.BotToken, "SLACK_BOT_TOKEN")
		fill(&c.SigningSecret, "SLACK_SIGNING_SECRET")
		fill(&c.APIURL, "SLACK_API_URL")
		if err := require("slack",
			named{"SLACK_BOT_TOKEN", c.BotToken},
			named{"SLACK_SIGNING_SECRET", c.SigningSecret},
		); err != nil {
			return nil, err
		}
		return slack.New(r, c), nil
	})
}

// WithDiscord mounts the Discord channel. DISCORD_BOT_TOKEN and
// DISCORD_PUBLIC_KEY come from the environment; see [WithSlack].
func WithDiscord(cfg discord.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		c := cfg
		fill(&c.BotToken, "DISCORD_BOT_TOKEN")
		fill(&c.PublicKey, "DISCORD_PUBLIC_KEY")
		fill(&c.APIURL, "DISCORD_API_URL")
		if err := require("discord",
			named{"DISCORD_BOT_TOKEN", c.BotToken},
			named{"DISCORD_PUBLIC_KEY", c.PublicKey},
		); err != nil {
			return nil, err
		}
		return discord.New(r, c)
	})
}

// WithTelegram mounts the Telegram channel. TELEGRAM_BOT_TOKEN and
// TELEGRAM_WEBHOOK_SECRET come from the environment; see [WithSlack].
func WithTelegram(cfg telegram.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		c := cfg
		fill(&c.Token, "TELEGRAM_BOT_TOKEN")
		fill(&c.Secret, "TELEGRAM_WEBHOOK_SECRET")
		fill(&c.APIURL, "TELEGRAM_API_URL")
		if err := require("telegram",
			named{"TELEGRAM_BOT_TOKEN", c.Token},
			named{"TELEGRAM_WEBHOOK_SECRET", c.Secret},
		); err != nil {
			return nil, err
		}
		return telegram.New(r, c), nil
	})
}

// WithGitHub mounts the GitHub App channel. GITHUB_APP_ID,
// GITHUB_APP_PRIVATE_KEY, and GITHUB_WEBHOOK_SECRET come from the
// environment; see [WithSlack]. GITHUB_INSTALLATION_ID comes from there
// too, and only a hand-off needs it: a webhook carries its own
// installation. The bot name is configured, not environmental: it is a
// setting, not a secret.
func WithGitHub(cfg github.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		c := cfg
		fill(&c.AppID, "GITHUB_APP_ID")
		fill(&c.PrivateKey, "GITHUB_APP_PRIVATE_KEY")
		fill(&c.WebhookSecret, "GITHUB_WEBHOOK_SECRET")
		fill(&c.APIURL, "GITHUB_API_URL")
		if err := fillInt(&c.InstallationID, "GITHUB_INSTALLATION_ID"); err != nil {
			return nil, err
		}
		if err := require("github",
			named{"GITHUB_APP_ID", c.AppID},
			named{"GITHUB_APP_PRIVATE_KEY", c.PrivateKey},
			named{"GITHUB_WEBHOOK_SECRET", c.WebhookSecret},
		); err != nil {
			return nil, err
		}
		return github.New(r, c)
	})
}

// fill takes a value from the environment when the field is empty, so an
// authored value still wins and a secret never has to be written down. It
// writes through a pointer into a per-call copy, never into the config an
// option captured — see [WithSlack].
func fill(field *string, env string) {
	if *field == "" {
		*field = os.Getenv(env)
	}
}

// fillInt is [fill] for a numeric setting. A value that does not parse is
// an error that names the variable, not a silent zero: a typo here would
// otherwise surface much later, as a hand-off that says the setting is
// missing while the operator can see it in the environment.
func fillInt(field *int64, env string) error {
	raw := os.Getenv(env)
	if *field != 0 || raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("bonnie: %s must be a number, not %q", env, raw)
	}
	*field = n
	return nil
}

// named is one credential and the variable it comes from.
type named struct{ env, value string }

// require refuses to mount a channel whose verification credentials are
// missing. The variable name is the fix.
func require(name string, creds ...named) error {
	for _, c := range creds {
		if c.value == "" {
			return fmt.Errorf("bonnie: the %s channel needs %s in the environment; "+
				"a webhook that does not verify its caller is a door with no lock", name, c.env)
		}
	}
	return nil
}
