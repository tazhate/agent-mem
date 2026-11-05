package embed

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestStaticMatchesReference compares tokenization and vectors with the Python model2vec
// output. Run with AMEM_STATIC_MODEL=/dir (model files plus ref.json from model2vec).
func TestStaticMatchesReference(t *testing.T) {
	dir := os.Getenv("AMEM_STATIC_MODEL")
	if dir == "" {
		t.Skip("AMEM_STATIC_MODEL not set")
	}
	m, err := LoadStatic(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	b, err := os.ReadFile(filepath.Join(dir, "ref.json"))
	if err != nil {
		t.Skip("no ref.json")
	}
	var refs []struct {
		Text string    `json:"text"`
		IDs  []int32   `json:"ids"`
		Vec  []float32 `json:"vec"`
	}
	json.Unmarshal(b, &refs)
	for _, r := range refs {
		got := m.Tokenize(r.Text)
		if !slices.Equal(got, r.IDs) && !(len(got) == 0 && len(r.IDs) == 0) {
			t.Errorf("tokens differ for %q:\n got %v\nwant %v", r.Text, got, r.IDs)
		}
		v, _ := m.Embed(t.Context(), []string{r.Text})
		var dot, a, c float64
		for i := range r.Vec {
			dot += float64(v[0][i]) * float64(r.Vec[i])
			a += float64(v[0][i]) * float64(v[0][i])
			c += float64(r.Vec[i]) * float64(r.Vec[i])
		}
		if a > 0 && c > 0 {
			if cos := dot / math.Sqrt(a*c); cos < 0.999 {
				t.Errorf("vector differs for %q: cos %.4f", r.Text, cos)
			}
		}
	}
}
