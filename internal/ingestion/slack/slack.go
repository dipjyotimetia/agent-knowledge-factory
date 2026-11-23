// Package slack reads one pinned Slack message. It never writes.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
)

// Message is a pinned snapshot. Text is wrapped as untrusted evidence.
type Message struct {
	Channel  string
	TS       string
	Text     string
	Revision string
}

// Fetch reads one message timestamp from an allowlisted Slack host.
func Fetch(ctx context.Context, baseURL, token, channel, ts string, allowHosts []string, httpClient *http.Client) (Message, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return Message{}, err
	}
	if !hostOK(parsed.Host, allowHosts) {
		return Message{}, fmt.Errorf("host %s is not allowlisted", parsed.Host)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/conversations.history?channel=" + url.QueryEscape(channel) + "&latest=" + url.QueryEscape(ts) + "&inclusive=true&limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Message{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Message{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("slack status %d", resp.StatusCode)
	}
	var payload struct {
		OK       bool `json:"ok"`
		Messages []struct {
			Text string `json:"text"`
			TS   string `json:"ts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Message{}, err
	}
	if !payload.OK || len(payload.Messages) == 0 {
		return Message{}, fmt.Errorf("slack message not found")
	}
	msg := payload.Messages[0]
	return Message{Channel: channel, TS: msg.TS, Revision: msg.TS, Text: curator.Untrusted(msg.Text)}, nil
}

func hostOK(host string, allow []string) bool {
	for _, item := range allow {
		if strings.EqualFold(item, host) {
			return true
		}
	}
	return false
}
