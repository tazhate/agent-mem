package embed

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/text/unicode/norm"
)

// Static is a model2vec static embedding model (minishlab "potion" family) evaluated in
// process: Unigram tokenization, then the mean of the token vectors, normalized. There is no
// neural network at inference time, so a memory costs microseconds and no extra service runs.
// The weight matrix is memory-mapped; only rows of tokens actually seen are paged in.
type Static struct {
	name   string
	vocab  map[string]int32
	scores []float32
	unk    int32
	maxLen int // longest piece, in bytes
	dim    int
	rows   int
	data   []float32 // rows*dim, mmapped
	mapped []byte
}

// LoadStatic opens a model directory containing tokenizer.json and model.safetensors.
func LoadStatic(dir string) (*Static, error) {
	s := &Static{name: filepath.Base(dir)}
	if err := s.loadTokenizer(filepath.Join(dir, "tokenizer.json")); err != nil {
		return nil, err
	}
	if err := s.loadWeights(filepath.Join(dir, "model.safetensors")); err != nil {
		return nil, err
	}
	if s.rows < len(s.scores) {
		return nil, fmt.Errorf("model has %d rows for %d tokens", s.rows, len(s.scores))
	}
	return s, nil
}

func (s *Static) loadTokenizer(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var tok struct {
		Model struct {
			Type  string              `json:"type"`
			UnkID int32               `json:"unk_id"`
			Vocab [][]json.RawMessage `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &tok); err != nil {
		return fmt.Errorf("tokenizer.json: %w", err)
	}
	if tok.Model.Type != "Unigram" {
		return fmt.Errorf("tokenizer type %q not supported (Unigram only)", tok.Model.Type)
	}
	s.unk = tok.Model.UnkID
	s.vocab = make(map[string]int32, len(tok.Model.Vocab))
	s.scores = make([]float32, len(tok.Model.Vocab))
	for i, pair := range tok.Model.Vocab {
		if len(pair) != 2 {
			return fmt.Errorf("tokenizer.json: bad vocab entry %d", i)
		}
		var piece string
		var score float64
		if json.Unmarshal(pair[0], &piece) != nil || json.Unmarshal(pair[1], &score) != nil {
			return fmt.Errorf("tokenizer.json: bad vocab entry %d", i)
		}
		if _, dup := s.vocab[piece]; !dup {
			s.vocab[piece] = int32(i)
		}
		s.scores[i] = float32(score)
		s.maxLen = max(s.maxLen, len(piece))
	}
	s.maxLen = min(s.maxLen, 96) // pieces longer than this are vanishingly rare; bounds Viterbi work
	return nil
}

func (s *Static) loadWeights(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hlen uint64
	if err := binary.Read(f, binary.LittleEndian, &hlen); err != nil {
		return err
	}
	if hlen > 1<<20 {
		return errors.New("safetensors header too large")
	}
	hdr := make([]byte, hlen)
	if _, err := f.ReadAt(hdr, 8); err != nil {
		return err
	}
	var tensors map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &tensors); err != nil {
		return fmt.Errorf("safetensors header: %w", err)
	}
	var t struct {
		Dtype   string  `json:"dtype"`
		Shape   []int   `json:"shape"`
		Offsets []int64 `json:"data_offsets"`
	}
	raw, ok := tensors["embeddings"]
	if !ok {
		return errors.New(`safetensors: no "embeddings" tensor`)
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	if t.Dtype != "F32" || len(t.Shape) != 2 || len(t.Offsets) != 2 {
		return fmt.Errorf("embeddings tensor %s %v not supported (F32 [rows, dim] only)", t.Dtype, t.Shape)
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	start := 8 + int64(hlen) + t.Offsets[0]
	end := 8 + int64(hlen) + t.Offsets[1]
	if end > st.Size() || end-start != int64(t.Shape[0])*int64(t.Shape[1])*4 {
		return errors.New("safetensors: tensor size mismatch")
	}
	m, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap weights: %w", err)
	}
	if start%4 != 0 {
		syscall.Munmap(m)
		return errors.New("safetensors: unaligned tensor")
	}
	s.mapped = m
	s.rows, s.dim = t.Shape[0], t.Shape[1]
	s.data = unsafe.Slice((*float32)(unsafe.Pointer(&m[start])), s.rows*s.dim)
	return nil
}

// Close unmaps the weights.
func (s *Static) Close() error {
	if s.mapped == nil {
		return nil
	}
	err := syscall.Munmap(s.mapped)
	s.mapped, s.data = nil, nil
	return err
}

func (s *Static) Model() string { return s.name }
func (s *Static) Dim() int      { return s.dim }

func (s *Static) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out[i] = s.embedOne(t)
	}
	return out, nil
}

func (s *Static) embedOne(text string) []float32 {
	v := make([]float32, s.dim)
	ids := s.Tokenize(text)
	n := 0
	for _, id := range ids {
		if id == s.unk || int(id) >= s.rows {
			continue
		}
		row := s.data[int(id)*s.dim : int(id+1)*s.dim]
		for j, x := range row {
			v[j] += x
		}
		n++
	}
	if n == 0 {
		return v
	}
	var norm2 float64
	for j := range v {
		v[j] /= float32(n)
		norm2 += float64(v[j]) * float64(v[j])
	}
	if norm2 > 0 {
		inv := float32(1 / math.Sqrt(norm2))
		for j := range v {
			v[j] *= inv
		}
	}
	return v
}

var (
	multiSpace = regexp.MustCompile(` {2,}`)
	anySpace   = regexp.MustCompile(`\s+`)
)

// normalize mirrors the tokenizer.json normalizer of the bge-m3 based potion models:
// precompiled NFKC charsmap, spaces around ASCII punctuation, collapsed whitespace, trimmed.
func normalize(s string) string {
	s = norm.NFKC.String(s)
	s = multiSpace.ReplaceAllString(s, " ")
	var b strings.Builder
	b.Grow(len(s) + 16)
	for _, r := range s {
		if r < 128 && strings.ContainsRune("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(anySpace.ReplaceAllString(b.String(), " "))
}

// Tokenize runs Unigram Viterbi segmentation over the Metaspace-encoded text.
func (s *Static) Tokenize(text string) []int32 {
	t := normalize(text)
	if t == "" {
		return nil
	}
	t = "▁" + strings.ReplaceAll(t, " ", "▁")
	n := len(t)
	best := make([]float64, n+1)
	from := make([]int, n+1)
	tokOf := make([]int32, n+1)
	for i := 1; i <= n; i++ {
		best[i] = math.Inf(-1)
	}
	unkScore := float64(s.scores[s.unk]) - 10
	for i := 0; i < n; i++ {
		if math.IsInf(best[i], -1) || !utf8.RuneStart(t[i]) {
			continue
		}
		matched := false
		for j := i + 1; j <= n && j-i <= s.maxLen; j++ {
			if j < n && !utf8.RuneStart(t[j]) {
				continue
			}
			if id, ok := s.vocab[t[i:j]]; ok {
				matched = matched || j-i == runeLen(t[i:])
				if sc := best[i] + float64(s.scores[id]); sc > best[j] {
					best[j], from[j], tokOf[j] = sc, i, id
				}
			}
		}
		// A character no piece covers becomes one unknown token, as in the reference.
		_, size := utf8.DecodeRuneInString(t[i:])
		if sc := best[i] + unkScore; !matched && sc > best[i+size] {
			best[i+size], from[i+size], tokOf[i+size] = sc, i, s.unk
		}
	}
	var ids []int32
	for j := n; j > 0; j = from[j] {
		ids = append(ids, tokOf[j])
	}
	for l, r := 0, len(ids)-1; l < r; l, r = l+1, r-1 {
		ids[l], ids[r] = ids[r], ids[l]
	}
	return ids
}

func runeLen(s string) int {
	_, size := utf8.DecodeRuneInString(s)
	return size
}
