package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"airview/internal/model"
)

// WebhookPayload is the JSON body POSTed to a rule's webhook URL.
type WebhookPayload struct {
	App     string `json:"app"`     // "AirView"
	Version string `json:"version"` // model.Version
	Type    string `json:"type"`    // "alert" (or "test")
	Alert   Alert  `json:"alert"`
}

type hookJob struct {
	url  string
	body []byte
}

type hookStats struct {
	sent, failed, dropped uint64
	lastErr               string
}

// enqueueWebhook queues a POST (e.mu held). Never blocks: when the queue is
// full or the global per-minute budget is spent, the post is dropped
// (there are no retries, so a dead endpoint cannot cause a storm).
func (e *Engine) enqueueWebhook(url string, al Alert, nowMs int64) bool {
	if nowMs-e.hookStart >= 60_000 {
		e.hookStart, e.hookCount = nowMs, 0
	}
	if e.hookCount >= e.opts.WebhookPerMinute {
		e.hookStats.dropped++
		return false
	}
	body, err := json.Marshal(WebhookPayload{App: model.AppName, Version: model.Version, Type: "alert", Alert: al})
	if err != nil {
		return false
	}
	select {
	case e.webhookCh <- hookJob{url: url, body: body}:
		e.hookCount++
		return true
	default:
		e.hookStats.dropped++
		return false
	}
}

// Run delivers webhooks until ctx is cancelled (one POST at a time, each
// with WebhookTimeout, no retries). Without Run, queued webhooks are not sent.
func (e *Engine) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-e.webhookCh:
			err := e.post(ctx, j.url, j.body)
			e.mu.Lock()
			if err != nil {
				e.hookStats.failed++
				e.hookStats.lastErr = err.Error()
			} else {
				e.hookStats.sent++
			}
			e.mu.Unlock()
			if err != nil {
				e.log.Warn("alerts: webhook failed", "err", err)
			}
		}
	}
}

func (e *Engine) post(ctx context.Context, url string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, e.opts.WebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: invalid URL", redactURL(url))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", e.opts.UserAgent)
	resp, err := e.opts.Client.Do(req)
	if err != nil {
		// *url.Error.Error() repeats the full URL, token and query included.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("webhook %s: %v", redactURL(url), ue.Err)
		}
		return fmt.Errorf("webhook %s: request failed", redactURL(url))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s: HTTP %s", redactURL(url), resp.Status)
	}
	return nil
}

// TestWebhook sends one test payload synchronously (for a "test" button).
func (e *Engine) TestWebhook(ctx context.Context, url string) error {
	if err := ValidateWebhookURL(url); err != nil {
		return err
	}
	body, _ := json.Marshal(WebhookPayload{App: model.AppName, Version: model.Version, Type: "test",
		Alert: Alert{RuleID: "test", RuleName: "Webhook test", Severity: SeverityInfo, Kind: "system", Event: SysCustom,
			Text: "AirView webhook test"}})
	return e.post(ctx, url, body)
}

// RedactRules returns copies of rules whose webhook URLs are cut to scheme
// and host ("https://hooks.example.com/..."), for callers that are not admin.
func RedactRules(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	for i := range out {
		if out[i].Actions.Webhook != "" {
			out[i].Actions.Webhook = redactURL(out[i].Actions.Webhook)
		}
	}
	return out
}

// redactURL keeps scheme and host only (webhook URLs often embed tokens).
func redactURL(s string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			rest := s[len(p):]
			host, tail := rest, ""
			for i := 0; i < len(rest); i++ {
				if rest[i] == '/' || rest[i] == '?' || rest[i] == '#' {
					host, tail = rest[:i], "/..."
					break
				}
			}
			if at := strings.LastIndexByte(host, '@'); at >= 0 {
				host = host[at+1:] // drop user:password
			}
			return p + host + tail
		}
	}
	return "?"
}
