package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// maxDeliveryResponse caps what [Delivery.PostJSON] reads back. Every
// platform answer BONNIE reads is a short JSON object — a message timestamp,
// an error code — and a cap keeps a confused endpoint from handing an
// adapter a body it will hold in memory. It is generous on purpose: the
// point is a bound, not a budget.
const maxDeliveryResponse = 1 << 16

// Delivery posts JSON to a platform API on behalf of one adapter.
//
// Every chat adapter delivers the same way, and the shape is not incidental:
// a reply that cannot be posted must NOT take the process down, because the
// turn already ran and its result is in the journal. `bonnie runs show`
// reads the answer back whatever the platform did with it. So a failure is
// reported to stderr, once, and the caller carries on — fire-and-log.
//
// It exists because four adapters had the same twenty lines: marshal, build
// the request, set the content type, set the authorisation, Do, log the
// transport error, close the body, check the status, log that too. The only
// real differences are the URL, the headers, and what the caller does with
// the answer, so those are the parameters and the rest is here.
type Delivery struct {
	// Client sends the request. A nil Client uses [http.DefaultClient],
	// which no adapter should rely on: each one sets its own timeout.
	Client *http.Client
	// Prefix names the adapter in a log line — "slack", "channel/github".
	// It is what an operator greps for.
	Prefix string
}

// PostJSON marshals payload, posts it, and returns the response body.
//
// ok is false when the message did not land: the payload would not marshal,
// the request would not build, the transport failed, or the platform
// answered outside 2xx. Each of those is written to stderr with the
// adapter's prefix before it returns. The body is read (bounded by
// [maxDeliveryResponse]) and closed here, so a caller never holds a
// connection open and never forgets to release one.
//
// A non-2xx answer still returns its body, because that is where a platform
// puts the reason. A caller that wants to say more than "it failed" reads
// it; one that does not ignores it.
//
// The status check is NOT the whole story on every platform. Slack answers
// 200 with `"ok": false` for a revoked token or an unknown channel, so the
// caller must read the body and decide. PostJSON reports what HTTP said and
// leaves what the API said to the adapter that understands it.
func (d Delivery) PostJSON(ctx context.Context, url string, header http.Header, payload any) ([]byte, bool) {
	body, err := json.Marshal(payload)
	if err != nil {
		d.logf("deliver: encode: %v", err)
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		d.logf("deliver: %v", err)
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		d.logf("deliver: %v", err)
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxDeliveryResponse))
	if err != nil {
		d.logf("deliver: read answer: %v", err)
		return nil, false
	}
	// 2xx is success, not 200 exactly. A platform that answers 201 or 204
	// has accepted the message, and treating that as a failure would log a
	// delivery that plainly happened.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if detail := FirstLine(string(answer)); detail != "" {
			d.logf("deliver: %s: %s", resp.Status, detail)
		} else {
			d.logf("deliver: %s", resp.Status)
		}
		return answer, false
	}
	return answer, true
}

// logf writes one line to stderr under the adapter's prefix.
func (d Delivery) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bonnie: "+d.Prefix+": "+format+"\n", args...)
}

// BearerHeader is the Authorization header most platforms want: the scheme,
// a space, the token. Slack and GitHub use "Bearer", Discord uses "Bot".
func BearerHeader(scheme, token string) http.Header {
	return http.Header{"Authorization": {scheme + " " + token}}
}
