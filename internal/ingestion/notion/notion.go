// Package notion reads one pinned Notion page. It never writes.
package notion

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

// Page is a pinned snapshot. Body is wrapped as untrusted evidence.
type Page struct {
	ID       string
	Title    string
	Revision string
	Body     string
}

// Fetch reads pageID from an allowlisted Notion host.
func Fetch(ctx context.Context, baseURL, token, pageID string, allowHosts []string, httpClient *http.Client) (Page, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return Page{}, err
	}
	if !allowed(parsed.Host, allowHosts) {
		return Page{}, fmt.Errorf("host %s is not allowlisted", parsed.Host)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/pages/"+url.PathEscape(pageID), nil)
	if err != nil {
		return Page{}, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Notion-Version", "2022-06-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Page{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Page{}, fmt.Errorf("notion status %d", resp.StatusCode)
	}
	var payload struct {
		ID             string `json:"id"`
		LastEditedTime string `json:"last_edited_time"`
		Properties     struct {
			Title struct {
				Title []struct {
					PlainText string `json:"plain_text"`
				} `json:"title"`
			} `json:"title"`
		} `json:"properties"`
		PlainText string `json:"plain_text"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Page{}, err
	}
	title := pageID
	if len(payload.Properties.Title.Title) > 0 {
		title = payload.Properties.Title.Title[0].PlainText
	}
	return Page{ID: payload.ID, Title: title, Revision: payload.LastEditedTime, Body: curator.Untrusted(payload.PlainText)}, nil
}

func allowed(host string, allow []string) bool {
	for _, item := range allow {
		if strings.EqualFold(item, host) {
			return true
		}
	}
	return false
}
