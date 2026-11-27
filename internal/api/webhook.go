package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publication"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/workflows"
)

// Sign returns the GitHub X-Hub-Signature-256 value for body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SignatureOK compares a webhook signature in constant time.
func SignatureOK(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	expected := Sign(secret, body)
	return hmac.Equal([]byte(expected), []byte(header))
}

// DeliveryStore records a webhook id once. Seen reports a previous success.
// Remember is called only after the signal or reconcile succeeds, so a 502 leaves the id free for a retry.
type DeliveryStore interface {
	Seen(ctx context.Context, deliveryID string) (bool, error)
	Remember(ctx context.Context, deliveryID string) (bool, error)
}

// ReviewSignaler delivers a review or head-change signal to the curator workflow.
type ReviewSignaler interface {
	Signal(ctx context.Context, gapID string, signal workflows.ReviewSignal) error
	Reconcile(ctx context.Context) error
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body unreadable")
		return
	}
	if !SignatureOK(s.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusUnauthorized, "signature rejected")
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		writeError(w, http.StatusBadRequest, "delivery id is required")
		return
	}
	if s.Publication != nil {
		s.enqueueWebhook(w, r, delivery, body)
		return
	}
	if s.Deliveries == nil {
		writeError(w, http.StatusServiceUnavailable, "release queue unavailable")
		return
	}
	if s.Deliveries != nil {
		seen, err := s.Deliveries.Seen(r.Context(), delivery)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "delivery store failed")
			return
		}
		if seen {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}
	}
	var payload struct {
		Action      string `json:"action"`
		After       string `json:"after"`
		Ref         string `json:"ref"`
		PullRequest struct {
			Merged         bool   `json:"merged"`
			MergeCommitSHA string `json:"merge_commit_sha"`
			Head           struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "json rejected")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "pull_request":
		gapID := strings.TrimPrefix(payload.PullRequest.Head.Ref, "curator/")
		if s.Reviews == nil || gapID == "" || gapID == payload.PullRequest.Head.Ref {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		signal := workflows.ReviewSignal{HeadChanged: payload.Action == "synchronize", HeadSHA: payload.PullRequest.Head.SHA}
		if payload.Action == "closed" && payload.PullRequest.Merged {
			signal = workflows.ReviewSignal{Merged: true, MergeSHA: payload.PullRequest.MergeCommitSHA}
		} else if payload.Action == "closed" {
			signal = workflows.ReviewSignal{Rejected: true, Reason: "pull request closed"}
		} else if payload.Action != "synchronize" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		if err := s.Reviews.Signal(r.Context(), gapID, signal); err != nil {
			writeError(w, http.StatusBadGateway, "signal failed")
			return
		}
		if !s.remember(w, r, delivery) {
			return
		}
	case "push":
		if payload.Ref == "refs/heads/"+s.defaultBranch() && s.Reviews != nil {
			if err := s.Reviews.Reconcile(r.Context()); err != nil {
				writeError(w, http.StatusBadGateway, "reconcile failed")
				return
			}
			if !s.remember(w, r, delivery) {
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
}

func (s *Server) remember(w http.ResponseWriter, r *http.Request, delivery string) bool {
	if s.Deliveries == nil {
		return true
	}
	if _, err := s.Deliveries.Remember(r.Context(), delivery); err != nil {
		writeError(w, http.StatusInternalServerError, "delivery store failed")
		return false
	}
	return true
}

func (s *Server) defaultBranch() string {
	if s.DefaultBranch == "" {
		return "main"
	}
	return s.DefaultBranch
}

func (s *Server) enqueueWebhook(w http.ResponseWriter, r *http.Request, delivery string, raw []byte) {
	var event struct {
		Action     string `json:"action"`
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Number     int    `json:"number"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		PullRequest struct {
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		writeError(w, http.StatusBadRequest, "json rejected")
		return
	}
	identity := s.Publication.Identity
	if !strings.EqualFold(event.Repository.FullName, identity.Repository) || event.Installation.ID <= 0 ||
		strconv.FormatInt(event.Installation.ID, 10) != identity.Installation {
		writeError(w, http.StatusForbidden, "forge identity rejected")
		return
	}
	request := publication.Request{DeliveryID: delivery}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		if event.Ref == "refs/heads/"+identity.Branch {
			request.Trigger = "push"
			request.ObservedSHA = event.After
		}
	case "pull_request":
		if event.PullRequest.Base.Ref == identity.Branch && event.Number > 0 {
			request.PRNumber = event.Number
			switch event.Action {
			case "closed":
				request.Trigger = "pr-merge"
			case "synchronize":
				request.Trigger = "pr-head"
			}
		}
	}
	if request.Trigger == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	id, err := s.Publication.RequestRelease(r.Context(), request)
	if errors.Is(err, publication.ErrUnmapped) {
		writeError(w, http.StatusNotFound, "unmapped pull request")
		return
	}
	if errors.Is(err, publication.ErrIdentity) {
		writeError(w, http.StatusForbidden, "forge identity rejected")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "release queue unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending", "operation_id": id})
}
