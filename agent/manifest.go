// Package agent implements BONNIE's L2 discovery: the authored agent tree.
//
// An agent is a directory of files with meaning from their paths. This
// package reads the manifest that anchors the tree, scaffolds new trees, and
// holds the rules that keep discovery honest:
//
//   - The manifest is strict. An unknown key, an unknown apiVersion, or two
//     manifests in one root is an error that names what is wrong.
//   - Discovery refuses what it cannot fully honor. A tree that needs a
//     build is refused by [runtime] hosts rather than partially served.
//   - Generated files are disposable; authored files are sacred.
//
// Data resolves at run time from the manifest; code resolves at build time
// by codegen. There is no run-time plugin loading anywhere in BONNIE.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// APIVersion is the only manifest schema version this build understands.
const APIVersion = "bonnie.dev/v0alpha"

// manifestNames are the file names discovery looks for, in order. The order
// matters only when a root holds more than one — and that is an error, so a
// silently preferred file can never hide a mistake.
var manifestNames = []string{"agent.yaml", "agent.yml", "agent.toml", "agent.json"}

// Sentinel errors. Test with [errors.Is].
var (
	// ErrNoManifest means the directory holds no manifest at all.
	ErrNoManifest = errors.New("bonnie: agent: no manifest found")

	// ErrAmbiguousManifest means the directory holds more than one manifest.
	// Discovery refuses to pick one, because a silently preferred file hides
	// mistakes.
	ErrAmbiguousManifest = errors.New("bonnie: agent: more than one manifest in the agent root")

	// ErrUnknownKey means the manifest carries a key the schema does not
	// define. A typo that fell back to a default would be a control nothing
	// applied.
	ErrUnknownKey = errors.New("bonnie: agent: unknown manifest key")

	// ErrUnknownAPIVersion means the manifest names a schema version this
	// build does not implement.
	ErrUnknownAPIVersion = errors.New("bonnie: agent: unknown manifest apiVersion")

	// ErrReservedKey means the manifest uses a key that is named in the
	// schema but not wired in this build. Refusing is the honest shape: a
	// key that silently does nothing is a control nothing applied.
	ErrReservedKey = errors.New("bonnie: agent: manifest key is not wired in this build")
)

// Manifest is the parsed agent manifest, schema bonnie.dev/v0alpha.
//
// All three accepted formats — YAML, TOML, JSON — decode into this one
// struct, and the key names are identical in every format: snake_case,
// lowercase.
type Manifest struct {
	// APIVersion names the schema version. It is required.
	APIVersion string `yaml:"apiVersion" toml:"apiVersion" json:"apiVersion"`

	// Title names the agent in operator-facing listings. When empty, hosts
	// fall back to the directory base name.
	Title string `yaml:"title" toml:"title" json:"title"`

	// Model selects the model, as "provider/name". When empty, the host's
	// default applies.
	Model string `yaml:"model" toml:"model" json:"model"`

	// Instructions is the path of the system prompt file, relative to the
	// agent root. When empty, "instructions.md" is the default.
	Instructions string `yaml:"instructions" toml:"instructions" json:"instructions"`

	// Sandbox configures tool isolation. A nil Sandbox means the host
	// default, which is no isolation and a loud warning about it.
	Sandbox *SandboxConfig `yaml:"sandbox" toml:"sandbox" json:"sandbox"`

	// Channels binds inbound transports. A nil Channels means the host
	// defaults.
	Channels *ChannelsConfig `yaml:"channels" toml:"channels" json:"channels"`

	// Workspace is a directory of seed files, relative to the agent root,
	// mirrored into every run's sandbox at open. When empty, nothing is
	// seeded. Files the model already wrote are never overwritten.
	Workspace string `yaml:"workspace" toml:"workspace" json:"workspace"`
}

// DefaultWorkspace is the workspace directory of a tree whose manifest names
// none.
const DefaultWorkspace = "workspace"

// WorkspaceDir returns the directory the agent's files live in, joined to
// root: the manifest's workspace key, or [DefaultWorkspace] when the key is
// absent. A nil manifest, which is what a tree with no manifest and a failed
// load both produce, takes the default too.
//
// It is one function because three callers must agree on the answer — the
// serve wiring that roots the agent's tools there, the dev loop that must
// not watch it, and codegen that embeds it. Three copies of the rule meant a
// renamed workspace could be honored by one and missed by another
// (docs/SPEC.md §4.9.1).
func (m *Manifest) WorkspaceDir(root string) string {
	rel := DefaultWorkspace
	if m != nil && m.Workspace != "" {
		rel = m.Workspace
	}
	return filepath.Join(root, filepath.Clean(rel))
}

// SandboxConfig selects the sandbox backend and its network policy.
type SandboxConfig struct {
	// Kind is one of: none, docker, microsandbox, local, auto. Empty means
	// the host default (none, with the warning that goes with it).
	Kind string `yaml:"kind" toml:"kind" json:"kind"`

	// Image overrides the backend's default image.
	Image string `yaml:"image" toml:"image" json:"image"`

	// Network constrains egress. A backend that cannot enforce the mode
	// refuses it rather than pretend; nil means allow-all, the default.
	Network *NetworkConfig `yaml:"network" toml:"network" json:"network"`
}

// NetworkConfig describes what a sandbox may reach.
type NetworkConfig struct {
	// Mode is one of: allow-all, deny-all, allow-list. Empty means
	// allow-all.
	Mode string `yaml:"mode" toml:"mode" json:"mode"`

	// Allow lists the hosts permit when Mode is allow-list.
	Allow []string `yaml:"allow" toml:"allow" json:"allow"`
}

// ChannelsConfig binds inbound transports. A nil value for a channel means
// it is not served; the presence of a key enables it.
type ChannelsConfig struct {
	// HTTP configures the HTTP channel.
	HTTP *HTTPConfig `yaml:"http" toml:"http" json:"http"`

	// Slack enables the Slack channel. Its credentials come from the
	// environment, never from the manifest: SLACK_BOT_TOKEN and
	// SLACK_SIGNING_SECRET are required, and their absence is a startup
	// error that names the variable.
	Slack *SlackChannelConfig `yaml:"slack" toml:"slack" json:"slack"`

	// Discord enables the Discord channel. Its credentials come from the
	// environment: DISCORD_BOT_TOKEN and DISCORD_PUBLIC_KEY are required.
	Discord *DiscordChannelConfig `yaml:"discord" toml:"discord" json:"discord"`

	// Telegram enables the Telegram channel. Its credentials come from the
	// environment: TELEGRAM_BOT_TOKEN and TELEGRAM_WEBHOOK_SECRET are
	// required.
	Telegram *TelegramChannelConfig `yaml:"telegram" toml:"telegram" json:"telegram"`
}

// HTTPConfig is the HTTP channel's binding.
type HTTPConfig struct {
	// Addr is the listen address, for example ":8080". Empty means the
	// host default.
	Addr string `yaml:"addr" toml:"addr" json:"addr"`
}

// SlackChannelConfig is the Slack channel's binding.
type SlackChannelConfig struct {
	// Path is the webhook route. Empty means the adapter default.
	Path string `yaml:"path" toml:"path" json:"path"`
}

// DiscordChannelConfig is the Discord channel's binding.
type DiscordChannelConfig struct {
	// Path is the webhook route. Empty means the adapter default.
	Path string `yaml:"path" toml:"path" json:"path"`

	// Command is the slash command the channel answers. Empty means the
	// adapter default ("ask").
	Command string `yaml:"command" toml:"command" json:"command"`
}

// TelegramChannelConfig is the Telegram channel's binding.
type TelegramChannelConfig struct {
	// Path is the webhook route. Empty means the adapter default.
	Path string `yaml:"path" toml:"path" json:"path"`

	// Command is the group command the channel answers. Empty means the
	// adapter default ("ask").
	Command string `yaml:"command" toml:"command" json:"command"`

	// Username is the bot's username without the @, for group mentions.
	Username string `yaml:"username" toml:"username" json:"username"`
}

// reservedKeys are named in the schema but refused when present, because
// nothing in this build acts on them. Each needs its loading story first.
var reservedKeys = map[string]string{
	"mcp": "MCP connections land when the Kit wiring is verified and the schema for them is written",
	// Skills are markdown procedure files, but no skill-loading runtime
	// exists yet — seeding them into a sandbox nobody reads would be a
	// dead key, and a dead key is a control nothing applied.
	"skills": "skill loading lands with a later L2 increment",
}

// sandboxKinds are the values SandboxConfig.Kind accepts.
var sandboxKinds = map[string]bool{
	"none": true, "docker": true, "microsandbox": true, "local": true, "auto": true,
}

// networkModes are the values NetworkConfig.Mode accepts.
var networkModes = map[string]bool{
	"allow-all": true, "deny-all": true, "allow-list": true,
}

// Load discovers and parses the manifest for the agent tree rooted at dir.
//
// It looks for agent.yaml, agent.yml, agent.toml, and agent.json, in that
// order. No manifest is [ErrNoManifest]. More than one is
// [ErrAmbiguousManifest] naming every path found, never a silent preference.
//
// The returned path is the file the manifest was read from; callers use it
// to name the winning source in a startup banner.
func Load(dir string) (*Manifest, string, error) {
	var found []string
	for _, name := range manifestNames {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 0:
		return nil, "", fmt.Errorf("%w: %s", ErrNoManifest, dir)
	case 1:
		m, err := LoadFile(found[0])
		return m, found[0], err
	default:
		return nil, "", fmt.Errorf("%w: %s carries %s — name one with --config",
			ErrAmbiguousManifest, dir, strings.Join(found, ", "))
	}
}

// LoadFile parses one named manifest file. The format comes from the
// extension: .yaml and .yml, .toml, or .json.
//
// Parsing is strict on one code path for all three formats: the file
// decodes into [Manifest] and into a generic key map, and the key sets are
// diffed against the schema. An unknown key is an error naming the key; the
// strictness cannot drift between formats because it is not written three
// times.
func LoadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("bonnie: agent: read manifest: %w", err)
	}
	return parse(path, data)
}

// ParseManifestData parses manifest bytes as if they were the file named
// filename. The format comes from the extension, exactly as [LoadFile] — so
// a generated binary can parse an embedded manifest that is not on disk. It
// performs the same strict validation.
func ParseManifestData(filename string, data []byte) (*Manifest, error) {
	return parse(filename, data)
}

// parse decodes and validates a manifest. It is the single entry point for
// every format, so the strictness below runs exactly once.
func parse(path string, data []byte) (*Manifest, error) {
	name := filepath.Base(path)

	var raw map[string]any
	var m Manifest
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
	case ".toml":
		if err := toml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
		if err := toml.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
	case ".json":
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("bonnie: agent: %s: %w", name, err)
		}
	default:
		return nil, fmt.Errorf("bonnie: agent: %s: unsupported manifest format %q: want .yaml, .yml, .toml, or .json", name, ext)
	}

	// Reserved keys first, so their message is about the future and not a
	// generic unknown-key complaint. Presence is enough: a key set to null
	// in a manifest is still an authoring decision about a reserved name.
	for _, key := range []string{"mcp", "skills"} {
		if _, ok := raw[key]; ok {
			return nil, fmt.Errorf("%w: %s: %s: %s", ErrReservedKey, name, key, reservedKeys[key])
		}
	}

	// The apiVersion check comes before the key diff, so a file written for
	// a different schema reports that, and not a list of keys it was never
	// meant to be measured against.
	if m.APIVersion == "" {
		return nil, fmt.Errorf("%w: %s: the manifest carries no apiVersion; want %q", ErrUnknownAPIVersion, name, APIVersion)
	}
	if m.APIVersion != APIVersion {
		return nil, fmt.Errorf("%w: %s: %q is not implemented here; want %q", ErrUnknownAPIVersion, name, m.APIVersion, APIVersion)
	}

	unknown := diffKeys(raw, keyTree(reflect.TypeOf(m)), "")
	if len(unknown) > 0 {
		return nil, fmt.Errorf("%w in %s: %s", ErrUnknownKey, name, strings.Join(quote(unknown), ", "))
	}

	if err := m.validate(name); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate checks the values the schema allows but the host cannot honor.
func (m *Manifest) validate(name string) error {
	if m.Sandbox != nil {
		if m.Sandbox.Kind != "" && !sandboxKinds[m.Sandbox.Kind] {
			return fmt.Errorf("bonnie: agent: %s: unknown sandbox kind %q: want none, docker, microsandbox, local, or auto", name, m.Sandbox.Kind)
		}
		if n := m.Sandbox.Network; n != nil {
			if n.Mode != "" && !networkModes[n.Mode] {
				return fmt.Errorf("bonnie: agent: %s: unknown network mode %q: want allow-all, deny-all, or allow-list", name, n.Mode)
			}
			switch {
			case n.Mode == "" && len(n.Allow) > 0:
				return fmt.Errorf("bonnie: agent: %s: sandbox.network.allow needs mode: allow-list", name)
			case n.Mode == "allow-list" && len(n.Allow) == 0:
				return fmt.Errorf("bonnie: agent: %s: network mode allow-list needs at least one host in sandbox.network.allow", name)
			}
		}
	}
	for _, p := range []struct{ key, val string }{
		{"instructions", m.Instructions},
		{"workspace", m.Workspace},
	} {
		if p.val == "" {
			continue
		}
		if err := containedPath(p.val); err != nil {
			return fmt.Errorf("bonnie: agent: %s: %s: %w", name, p.key, err)
		}
	}
	return nil
}

// containedPath rejects paths that leave the agent root: absolute paths
// and anything that climbs with "..". Discovery reads relative to the
// root, and a manifest key that points outside it is either a mistake or
// an attempt. A trailing slash is fine — directories are written that way.
func containedPath(p string) error {
	if filepath.IsAbs(p) {
		return fmt.Errorf("%q is absolute; want a path relative to the agent root", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q leaves the agent root", p)
	}
	return nil
}

// quote quotes every string, for error messages that list keys.
func quote(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// diffKeys walks a decoded manifest against the schema tree and returns
// every key path the schema does not define, dotted from the root —
// "sandbox.netwrok", not "netwrok".
//
// The schema tree is map[string]any: nil is a leaf, a map is a struct. Type
// mismatches (a string where a struct belongs) are left to the typed
// decode, which reports them with the format's own precision.
func diffKeys(node map[string]any, schema map[string]any, prefix string) []string {
	var unknown []string
	for key, val := range node {
		want, ok := schema[key]
		if !ok {
			unknown = append(unknown, dotted(prefix, key))
			continue
		}
		sub, isMap := want.(map[string]any)
		if !isMap {
			continue // leaf: the typed decode owns value errors
		}
		child, ok := val.(map[string]any)
		if !ok {
			continue // type mismatch: the typed decode reports it
		}
		unknown = append(unknown, diffKeys(child, sub, dotted(prefix, key))...)
	}
	return unknown
}

// dotted joins one key path element.
func dotted(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// keyTree builds the schema tree from the Manifest struct's own tags, so
// the schema has one definition and the strictness cannot drift from it.
// Pointers are followed; nested structs become nodes, everything else a
// leaf.
func keyTree(t reflect.Type) map[string]any {
	tree := make(map[string]any)
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			tree[name] = keyTree(ft)
			continue
		}
		tree[name] = nil
	}
	return tree
}
