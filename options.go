package bonnie

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/discord"
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
		fill(&cfg.BotToken, "SLACK_BOT_TOKEN")
		fill(&cfg.SigningSecret, "SLACK_SIGNING_SECRET")
		fill(&cfg.APIURL, "SLACK_API_URL")
		if err := require("slack",
			named{"SLACK_BOT_TOKEN", cfg.BotToken},
			named{"SLACK_SIGNING_SECRET", cfg.SigningSecret},
		); err != nil {
			return nil, err
		}
		return slack.New(r, cfg), nil
	})
}

// WithDiscord mounts the Discord channel. DISCORD_BOT_TOKEN and
// DISCORD_PUBLIC_KEY come from the environment; see [WithSlack].
func WithDiscord(cfg discord.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		fill(&cfg.BotToken, "DISCORD_BOT_TOKEN")
		fill(&cfg.PublicKey, "DISCORD_PUBLIC_KEY")
		fill(&cfg.APIURL, "DISCORD_API_URL")
		if err := require("discord",
			named{"DISCORD_BOT_TOKEN", cfg.BotToken},
			named{"DISCORD_PUBLIC_KEY", cfg.PublicKey},
		); err != nil {
			return nil, err
		}
		return discord.New(r, cfg)
	})
}

// WithTelegram mounts the Telegram channel. TELEGRAM_BOT_TOKEN and
// TELEGRAM_WEBHOOK_SECRET come from the environment; see [WithSlack].
func WithTelegram(cfg telegram.Config) Option {
	return WithChannel(func(r *runtime.Runner) (Channel, error) {
		fill(&cfg.Token, "TELEGRAM_BOT_TOKEN")
		fill(&cfg.Secret, "TELEGRAM_WEBHOOK_SECRET")
		fill(&cfg.APIURL, "TELEGRAM_API_URL")
		if err := require("telegram",
			named{"TELEGRAM_BOT_TOKEN", cfg.Token},
			named{"TELEGRAM_WEBHOOK_SECRET", cfg.Secret},
		); err != nil {
			return nil, err
		}
		return telegram.New(r, cfg), nil
	})
}

// fill takes a value from the environment when the field is empty, so an
// authored value still wins and a secret never has to be written down.
func fill(field *string, env string) {
	if *field == "" {
		*field = os.Getenv(env)
	}
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
