package index

import (
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"unicode"
)

const embedDims = 64

// HashEmbed is a local bag-of-tokens embedding used when SEARCH_MODE=hybrid.
// It does not call a model. A Postgres pgvector column is optional and documented
// separately; full-text search remains the default.
func HashEmbed(text string) []float64 {
	vec := make([]float64, embedDims)
	for _, token := range tokens(text) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(token))
		vec[h.Sum32()%embedDims]++
	}
	return normalize(vec)
}

// Cosine is the similarity of two embeddings.
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot float64
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

// Rerank orders hits by hashed-token similarity to the query, then by the original order.
func Rerank(query string, hits []Hit) []Hit {
	if len(hits) < 2 {
		return hits
	}
	q := HashEmbed(query)
	type scored struct {
		hit   Hit
		score float64
		index int
	}
	ranked := make([]scored, len(hits))
	for i, hit := range hits {
		ranked[i] = scored{hit: hit, score: Cosine(q, HashEmbed(hit.Title+" "+hit.Snippet)), index: i}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].index < ranked[j].index
		}
		return ranked[i].score > ranked[j].score
	})
	out := make([]Hit, len(hits))
	for i := range ranked {
		out[i] = ranked[i].hit
	}
	return out
}

func tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func normalize(vec []float64) []float64 {
	var sum float64
	for _, v := range vec {
		sum += v * v
	}
	if sum == 0 {
		return vec
	}
	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}
