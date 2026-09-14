// Package channel_test holds the tests the channel contract needs. It is an
// external test package so it can import concrete adapters — channel/http
// imports channel, and an in-package test file that imported it back would
// make the dependency direction ambiguous.
//
// What lives here:
//
//   - Compile-time proof that every adapter in the repository satisfies the
//     channel contract. Adding an interface method breaks the build here,
//     at the contract, before it breaks an adapter's own suite.
//   - Proof that each adapter joins the channeltest conformance suite.
package channel_test

import (
	"testing"

	"github.com/mark3labs/bonnie/channel"
	"github.com/mark3labs/bonnie/channel/http"
	"github.com/mark3labs/bonnie/channeltest"
	"github.com/mark3labs/bonnie/runtime"
)

// The contract assertions. One line per adapter; the type assertions in the
// adapters' own packages stay where they are, so a new adapter cannot forget
// which interfaces it promised.
var (
	_ channel.Channel = (*http.Channel)(nil)
	_ channel.Inbound = (*http.Channel)(nil)
)

// TestPolicyValuesAreTheWireFormat pins the string values, because they cross
// the wire: a request with "turn_policy":"steer" must mean the same thing to
// every adapter forever. Renaming them is a breaking change, not a refactor.
func TestPolicyValuesAreTheWireFormat(t *testing.T) {
	t.Parallel()
	if channel.PolicySteer != "steer" {
		t.Fatalf("PolicySteer = %q, want \"steer\"", channel.PolicySteer)
	}
	if channel.PolicyQueue != "queue" {
		t.Fatalf("PolicyQueue = %q, want \"queue\"", channel.PolicyQueue)
	}
}

// TestHTTPChannelJoinsConformance runs the whole channeltest suite against
// the HTTP adapter. A second adapter joins by adding its own test here with
// its own build function — and inherits every case.
func TestHTTPChannelJoinsConformance(t *testing.T) {
	channeltest.RunConformance(t, httpFixture)
}

// httpFixture wires the HTTP adapter the way the conformance suite wants it:
// a fresh journal, one script agent, and a fresh runner over both.
func httpFixture(t *testing.T) *channeltest.Fixture {
	t.Helper()

	agent := channeltest.NewScriptAgent()
	j := runtime.NewMemoryJournal()
	r := runtime.NewRunner(j, agent.Factory())
	inbound := http.New(r, http.WithIDGenerator(seededIDs()))
	return &channeltest.Fixture{Inbound: inbound, Agent: agent, Journal: j}
}

// seededIDs hands out stable unique run IDs, so a test can read the wire.
func seededIDs() func() string {
	n := 0
	return func() string {
		n++
		return "run-" + string(rune('a'+n))
	}
}
