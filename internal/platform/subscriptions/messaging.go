package subscriptions

import (
	"context"
	"fmt"
	"net/url"
)

// MessagingDecision is subscriptions-api's answer to "may this tenant send on a paid channel".
type MessagingDecision struct {
	Allowed bool
	// Known is false when subscriptions-api could not answer; callers then let the send go and
	// leave the decision to notifications-api, which charges each message and fails closed.
	Known  bool
	Reason string
}

// MessagingAllowed asks whether the tenant has an SMS or WhatsApp plan, bundle or credit for one
// message (service "sms" or "whatsapp"), without charging anything.
func (c *Client) MessagingAllowed(ctx context.Context, tenantID, service string) MessagingDecision {
	if c.cfg.ServiceURL == "" || c.cfg.APIKey == "" {
		return MessagingDecision{Allowed: true}
	}
	q := url.Values{"tenant_id": {tenantID}, "units": {"1"}}
	resp, err := c.sc.Get(ctx, fmt.Sprintf("/api/v1/s2s/credits/%s/check?%s", url.PathEscape(service), q.Encode()), c.authHeaders(tenantID, ""))
	if err != nil || resp.StatusCode != 200 {
		return MessagingDecision{Allowed: true}
	}
	var d struct {
		Allowed bool   `json:"allowed"`
		Reason  string `json:"reason"`
	}
	if err := resp.DecodeJSON(&d); err != nil {
		return MessagingDecision{Allowed: true}
	}
	return MessagingDecision{Allowed: d.Allowed, Known: true, Reason: d.Reason}
}
