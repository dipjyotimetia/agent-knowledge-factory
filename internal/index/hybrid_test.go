package index

import "testing"

func TestHybridRerankPrefersSharedTokens(t *testing.T) {
	hits := []Hit{
		{Title: "unrelated note", Snippet: "nothing here"},
		{Title: "movement query", Snippet: "Do not copy rows from movement events"},
	}
	ranked := Rerank("where to query movement events", hits)
	if ranked[0].Title != "movement query" {
		t.Fatalf("first = %s", ranked[0].Title)
	}
	if Cosine(HashEmbed("eventId"), HashEmbed("eventId")) < 0.99 {
		t.Fatal("identical text was not similar")
	}
}
