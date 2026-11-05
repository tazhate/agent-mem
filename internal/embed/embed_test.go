package embed

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/stem"
	"github.com/tazhate/agent-mem/internal/store"
)

const dim = 256

// bow is a toy embedding: hashed bag of stems, so texts sharing words are close.
func bow(s string) []float32 {
	v := make([]float32, dim)
	for _, t := range stem.Tokens(s) {
		h := fnv.New32a()
		h.Write([]byte(stem.Word(t)))
		v[h.Sum32()%dim] += 1
	}
	return v
}

func fakeServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i, s := range req.Input {
			out.Data = append(out.Data, item{i, bow(s)})
		}
		json.NewEncoder(w).Encode(out)
	}))
}

func TestBackfillAndSearch(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()
	ctx := t.Context()
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	pid, _ := st.EnsureProject(ctx, "k", "p", "")
	other, _ := st.EnsureProject(ctx, "k2", "p2", "")
	target, _, _ := st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "gotcha", Title: "chroma mcp eats cpu on every sync", Body: "onnx embeddings per fact"})
	st.InsertMemory(ctx, &store.Memory{ProjectID: pid, Type: "change", Title: "added login form", Body: "react component"})
	st.InsertMemory(ctx, &store.Memory{ProjectID: other, Type: "gotcha", Title: "chroma mcp eats cpu on every sync too", Body: "other project"})

	cfg := config.EmbedConfig{Kind: "openai", BaseURL: srv.URL, Model: "toy", BatchSize: 2}
	emb, err := NewEmbedder(ctx, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(st, emb, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return time.Time{} })
	if err := idx.load(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		n, err := idx.backfill(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	hits, err := idx.Search(ctx, "why does chroma use so much cpu", store.Scope{ProjectID: pid}, 3)
	if err != nil || len(hits) == 0 || hits[0].ID != target {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	for _, h := range hits {
		if h.ID == 3 {
			t.Fatal("other project leaked into scoped search")
		}
	}
	// A restart reloads vectors from the database without re-embedding.
	idx2 := NewIndex(st, emb, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return time.Time{} })
	idx2.load(ctx)
	if n, _ := idx2.backfill(ctx); n != 0 || len(idx2.ids) != 3 {
		t.Fatalf("reload: backfilled %d, loaded %d", n, len(idx2.ids))
	}
}

// BenchmarkSearch110k measures the in-memory scan plus rescore on 110k random vectors.
func BenchmarkSearch110k(b *testing.B) {
	for _, d := range []int{384, 1024} {
		b.Run(itoa(d), func(b *testing.B) {
			st, _ := store.Open(filepath.Join(b.TempDir(), "b.db"))
			defer st.Close()
			ctx := context.Background()
			idx := &Index{st: st, emb: staticModel("m"), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			r := rand.New(rand.NewPCG(1, 2))
			const n = 110_000
			tx, _ := st.DB().Begin()
			for i := range n {
				v := make([]float32, d)
				for j := range v {
					v[j] = float32(r.NormFloat64())
				}
				q, s := Quantize(v)
				tx.Exec(`INSERT INTO vectors(memory_id, model, dim, scale, q) VALUES (?, 'm', ?, ?, ?)`, i+1, d, s, encode(q))
				idx.add(int64(i+1), 1, q)
			}
			tx.Commit()
			qv := make([]float32, d)
			for j := range qv {
				qv[j] = float32(r.NormFloat64())
			}
			q, _ := Quantize(qv)
			b.ReportMetric(float64(len(idx.bits)*8)/1e6, "MB_bits")
			b.ResetTimer()
			for b.Loop() {
				if _, err := idx.searchVector(ctx, q, store.Scope{ProjectID: 1}, 20); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type staticModel string

func (m staticModel) Model() string                                        { return string(m) }
func (m staticModel) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
