package nats

import "testing"

func TestPresenceCapabilities(t *testing.T) {
	t.Parallel()
	c := &Channel{}
	endpoints := c.PresenceEndpoints()
	if len(endpoints) == 0 {
		t.Fatal("missing endpoint")
	}
	for _, ep := range endpoints {
		if ep.Channel != c.Name() || !ep.Input {
			t.Fatalf("invalid endpoint: %+v", ep)
		}
		if ep.Ready {
			t.Fatal("unstarted NATS channel reports ready")
		}
	}
}
