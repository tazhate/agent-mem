// Package embed adds optional semantic search. One vector per memory, int8-quantized in SQLite;
// RAM holds only sign bits (dim/8 bytes per memory). Search is a Hamming prefilter over the bits
// followed by an exact int8 rescore of the best candidates read back from the database.
package embed

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/bits"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/store"
)

type Embedder interface {
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NewEmbedder returns nil for kind "none".
func NewEmbedder(ctx context.Context, c config.EmbedConfig) (Embedder, error) {
	switch c.Kind {
	case "", "none":
		return nil, nil
	case "openai":
		key, err := config.Resolve(ctx, c.Key)
		if err != nil {
			return nil, err
		}
		if c.Model == "" {
			return nil, errors.New("embed: model is required")
		}
		return &openAI{base: strings.TrimRight(cmp.Or(c.BaseURL, "http://127.0.0.1:11434/v1"), "/"), key: key, model: c.Model,
			client: &http.Client{Timeout: 60 * time.Second}}, nil
	}
	return nil, fmt.Errorf("embed: unknown kind %q (none, openai)", c.Kind)
}

type openAI struct {
	base, key, model string
	client           *http.Client
}

func (o *openAI) Model() string { return o.model }

func (o *openAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": o.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embeddings HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, fmt.Errorf("embeddings: bad index %d", d.Index)
		}
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

// Quantize L2-normalizes v and maps it to int8 with one scale per vector.
func Quantize(v []float32) ([]int8, float32) {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return make([]int8, len(v)), 0
	}
	var maxAbs float64
	for _, x := range v {
		maxAbs = max(maxAbs, math.Abs(float64(x)/norm))
	}
	scale := maxAbs / 127
	q := make([]int8, len(v))
	for i, x := range v {
		q[i] = int8(math.Round(float64(x) / norm / scale))
	}
	return q, float32(scale)
}

func signBits(q []int8) []uint64 {
	out := make([]uint64, (len(q)+63)/64)
	for i, x := range q {
		if x > 0 {
			out[i/64] |= 1 << (i % 64)
		}
	}
	return out
}

func encode(q []int8) []byte {
	b := make([]byte, len(q))
	for i, x := range q {
		b[i] = byte(x)
	}
	return b
}

// Index is the in-memory sign-bit index plus the background embedder.
type Index struct {
	st    *store.Store
	emb   Embedder
	cfg   config.EmbedConfig
	log   *slog.Logger
	idle  func() time.Time
	wake  chan struct{}
	ready chan struct{}

	mu       sync.RWMutex
	dim      int
	words    int
	ids      []int64
	projects []int64
	bits     []uint64 // len(ids) * words
}

// NewIndex wires an embedder; lastActivity lets backfill yield to live hooks.
func NewIndex(st *store.Store, emb Embedder, cfg config.EmbedConfig, log *slog.Logger, lastActivity func() time.Time) *Index {
	return &Index{st: st, emb: emb, cfg: cfg, log: log, idle: lastActivity, wake: make(chan struct{}, 1), ready: make(chan struct{})}
}

func (x *Index) Kick(string, event.Kind) {
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Ready is closed once stored vectors are loaded.
func (x *Index) Ready() <-chan struct{} { return x.ready }

func (x *Index) Run(ctx context.Context) {
	if err := x.load(ctx); err != nil {
		x.log.Error("load vectors", "err", err)
	}
	close(x.ready)
	for {
		n, err := x.backfill(ctx)
		if err != nil {
			x.log.Warn("embed backfill", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
		wait := 200 * time.Millisecond
		if n == 0 || err != nil {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-x.wake:
		case <-time.After(wait):
		}
	}
}

func (x *Index) load(ctx context.Context) error {
	rows, err := x.st.DB().QueryContext(ctx, `SELECT v.memory_id, coalesce(m.project_id, 0), v.dim, v.q FROM vectors v
		JOIN memories m ON m.id = v.memory_id WHERE v.model = ? AND m.private = 0 AND m.superseded_by IS NULL`, x.emb.Model())
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, pid int64
		var dim int
		var q []byte
		if err := rows.Scan(&id, &pid, &dim, &q); err != nil {
			return err
		}
		x.add(id, pid, bytesToInt8(q))
		n++
	}
	x.log.Info("vectors loaded", "count", n, "model", x.emb.Model(), "dim", x.dim)
	return rows.Err()
}

func bytesToInt8(b []byte) []int8 {
	q := make([]int8, len(b))
	for i, v := range b {
		q[i] = int8(v)
	}
	return q
}

func (x *Index) add(id, pid int64, q []int8) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.dim == 0 {
		x.dim, x.words = len(q), (len(q)+63)/64
	}
	if len(q) != x.dim {
		return
	}
	x.ids = append(x.ids, id)
	x.projects = append(x.projects, pid)
	x.bits = append(x.bits, signBits(q)...)
}

// backfill embeds memories that have no vector for the current model, newest first.
func (x *Index) backfill(ctx context.Context) (int, error) {
	if gap := x.cfg.IdleGap.Duration; gap > 0 && time.Since(x.idle()) < gap {
		return 1, nil // hooks are active: come back shortly
	}
	batch := max(x.cfg.BatchSize, 1)
	rows, err := x.st.DB().QueryContext(ctx, `SELECT m.id, coalesce(m.project_id, 0), m.title, m.body FROM memories m
		LEFT JOIN vectors v ON v.memory_id = m.id AND v.model = ?
		WHERE v.memory_id IS NULL AND m.private = 0 AND m.superseded_by IS NULL
		ORDER BY m.created_at DESC LIMIT ?`, x.emb.Model(), batch)
	if err != nil {
		return 0, err
	}
	var ids, pids []int64
	var texts []string
	for rows.Next() {
		var id, pid int64
		var title, body string
		if err := rows.Scan(&id, &pid, &title, &body); err != nil {
			rows.Close()
			return 0, err
		}
		ids, pids = append(ids, id), append(pids, pid)
		texts = append(texts, clip(title+"\n"+body, 2000))
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, rows.Err()
	}
	vecs, err := x.emb.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	qs := make([][]int8, len(vecs))
	scales := make([]float32, len(vecs))
	for i, v := range vecs {
		qs[i], scales[i] = Quantize(v)
	}
	if err := x.st.Write(ctx, func(tx *sql.Tx) error {
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO vectors(memory_id, model, dim, scale, q) VALUES (?, ?, ?, ?, ?)`,
				id, x.emb.Model(), len(qs[i]), scales[i], encode(qs[i])); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	for i, id := range ids {
		x.add(id, pids[i], qs[i])
	}
	return len(ids), nil
}

// Search implements recall.VectorSearcher.
func (x *Index) Search(ctx context.Context, text string, scope store.Scope, k int) ([]recall.VecHit, error) {
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	vecs, err := x.emb.Embed(ctx, []string{clip(text, 2000)})
	if err != nil {
		return nil, err
	}
	q, _ := Quantize(vecs[0])
	return x.searchVector(ctx, q, scope, k)
}

type cand struct {
	i    int
	dist int
}

func (x *Index) searchVector(ctx context.Context, q []int8, scope store.Scope, k int) ([]recall.VecHit, error) {
	x.mu.RLock()
	if len(q) != x.dim || len(x.ids) == 0 {
		x.mu.RUnlock()
		return nil, nil
	}
	qb := signBits(q)
	pre := max(k*8, 256)
	// Max-heap by distance keeps the pre best candidates.
	heap := make([]cand, 0, pre)
	for i := range x.ids {
		if !scope.All && x.projects[i] != scope.ProjectID && x.projects[i] != 0 {
			continue
		}
		row := x.bits[i*x.words : (i+1)*x.words]
		d := 0
		for w, v := range row {
			d += bits.OnesCount64(v ^ qb[w])
		}
		if len(heap) < pre {
			heap = append(heap, cand{i, d})
			up(heap, len(heap)-1)
		} else if d < heap[0].dist {
			heap[0] = cand{i, d}
			down(heap, 0)
		}
	}
	ids := make([]int64, len(heap))
	for j, c := range heap {
		ids[j] = x.ids[c.i]
	}
	x.mu.RUnlock()
	return x.rescore(ctx, q, ids, k)
}

func (x *Index) rescore(ctx context.Context, q []int8, ids []int64, k int) ([]recall.VecHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, x.emb.Model())
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := x.st.DB().QueryContext(ctx, `SELECT memory_id, scale, q FROM vectors WHERE model = ? AND memory_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var qnorm float64
	for _, v := range q {
		qnorm += float64(v) * float64(v)
	}
	qnorm = math.Sqrt(qnorm)
	var out []recall.VecHit
	for rows.Next() {
		var id int64
		var scale float32
		var b []byte
		if err := rows.Scan(&id, &scale, &b); err != nil {
			return nil, err
		}
		if len(b) != len(q) {
			continue
		}
		var dot int64
		var vnorm int64
		for i, v := range b {
			s := int64(int8(v))
			dot += s * int64(q[i])
			vnorm += s * s
		}
		if vnorm == 0 || qnorm == 0 {
			continue
		}
		sim := float64(dot) / (math.Sqrt(float64(vnorm)) * qnorm)
		out = append(out, recall.VecHit{ID: id, Sim: float32(sim)})
	}
	slices.SortFunc(out, func(a, b recall.VecHit) int { return cmp.Compare(b.Sim, a.Sim) })
	return out[:min(len(out), k)], rows.Err()
}

func up(h []cand, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if h[p].dist >= h[i].dist {
			return
		}
		h[p], h[i] = h[i], h[p]
		i = p
	}
}

func down(h []cand, i int) {
	for {
		l, r, big := 2*i+1, 2*i+2, i
		if l < len(h) && h[l].dist > h[big].dist {
			big = l
		}
		if r < len(h) && h[r].dist > h[big].dist {
			big = r
		}
		if big == i {
			return
		}
		h[i], h[big] = h[big], h[i]
		i = big
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
