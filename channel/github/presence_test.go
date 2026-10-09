package github

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPresenceCapabilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		cfg      Config
		delivery bool
	}{
		{name: "default webhook without app credentials"},
		{name: "app credentials support webhook delivery", cfg: Config{AppID: "12345", PrivateKey: "private-key-secret", WebhookSecret: "webhook-secret"}, delivery: true},
		{name: "partial app config cannot deliver", cfg: Config{AppID: "12345", WebhookSecret: "webhook-secret"}},
		{name: "installation ID alone cannot deliver", cfg: Config{InstallationID: 42, WebhookSecret: "webhook-secret"}},
		{name: "complete app config supports proactive delivery", cfg: Config{AppID: "12345", PrivateKey: "private-key-secret", InstallationID: 42, WebhookSecret: "webhook-secret"}, delivery: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &Channel{cfg: tt.cfg}
			endpoints := c.PresenceEndpoints()
			if len(endpoints) != 1 {
				t.Fatalf("got %d endpoints, want 1", len(endpoints))
			}
			ep := endpoints[0]
			if ep.Channel != c.Name() || ep.Address != DefaultPath || !ep.Input || !ep.Ready || ep.Delivery != tt.delivery {
				t.Fatalf("unexpected endpoint: %+v", ep)
			}
			data, err := json.Marshal(endpoints)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-key-secret", "webhook-secret"} {
				if strings.Contains(string(data), secret) {
					t.Fatalf("serialized endpoints leak credential %q: %s", secret, data)
				}
			}
		})
	}
}
