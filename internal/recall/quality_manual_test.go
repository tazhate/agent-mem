package recall

import (
	"fmt"
	"os"
	"testing"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/store"
)

// TestManualQuality prints recall for real prompts; run with AMEM_QUALITY_DB=/path/amem.db.
func TestManualQuality(t *testing.T) {
	path := os.Getenv("AMEM_QUALITY_DB")
	if path == "" {
		t.Skip("AMEM_QUALITY_DB not set")
	}
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := t.Context()
	rows, err := st.DB().QueryContext(ctx, `SELECT e.input, coalesce(s.project_id,0) FROM events e JOIN sessions s ON s.id=e.session_id
		WHERE e.kind='prompt' AND length(e.input) BETWEEN 30 AND 300 ORDER BY random() LIMIT 12`)
	if err != nil {
		t.Fatal(err)
	}
	type q struct {
		text string
		pid  int64
	}
	var qs []q
	for rows.Next() {
		var x q
		rows.Scan(&x.text, &x.pid)
		qs = append(qs, x)
	}
	rows.Close()
	eng := New(st, config.Default().Recall, nil)
	for _, x := range qs {
		hits, _ := eng.Search(ctx, x.text, store.Scope{ProjectID: x.pid}, 3)
		fmt.Printf("\nQ: %s\n", clip(oneLine(x.text), 160))
		for _, h := range hits {
			fmt.Printf("   %.2f rel=%.2f %s\n", h.Score, h.Rel, Line(h.Mem))
		}
	}
}
