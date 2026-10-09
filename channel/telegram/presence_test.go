package telegram

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
		address  string
		delivery bool
	}{
		{name: "default path without delivery credentials", address: DefaultPath},
		{name: "custom path with delivery credentials", cfg: Config{Path: "/hooks/telegram", Token: "bot-secret", Secret: "webhook-secret"}, address: "/hooks/telegram", delivery: true},
		{name: "partial config has no delivery", cfg: Config{Path: "/custom", Secret: "webhook-secret"}, address: "/custom"},
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
			if ep.Channel != c.Name() || ep.Address != tt.address || !ep.Input || !ep.Ready || ep.Delivery != tt.delivery {
				t.Fatalf("unexpected endpoint: %+v", ep)
			}
			data, err := json.Marshal(endpoints)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"bot-secret", "webhook-secret"} {
				if strings.Contains(string(data), secret) {
					t.Fatalf("serialized endpoints leak credential %q: %s", secret, data)
				}
			}
		})
	}
}
