package embed

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func cos(a, b []float32) float64 {
	var d, x, y float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		x += float64(a[i]) * float64(a[i])
		y += float64(b[i]) * float64(b[i])
	}
	return d / math.Sqrt(x*y)
}

// TestStaticQualityManual: AMEM_STATIC_MODEL=dir AMEM_QUALITY_DB=amem.db (read-only).
func TestStaticQualityManual(t *testing.T) {
	dir, dbp := os.Getenv("AMEM_STATIC_MODEL"), os.Getenv("AMEM_QUALITY_DB")
	if dir == "" || dbp == "" {
		t.Skip("set AMEM_STATIC_MODEL and AMEM_QUALITY_DB")
	}
	m, err := LoadStatic(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := sql.Open("sqlite", "file:"+dbp+"?mode=ro")
	defer db.Close()
	text := func(q string, args ...any) []string {
		rows, _ := db.Query(q, args...)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b string
			rows.Scan(&a, &b)
			out = append(out, clip(a+"\n"+b, 600))
		}
		return out
	}
	rel := text(`SELECT title, body FROM memories WHERE id IN (107840, 107848, 107870, 107867)`)
	random := text(`SELECT title, body FROM memories ORDER BY random() LIMIT 300`)
	q, _ := m.Embed(t.Context(), []string{"почему chroma жрет процессор"})
	rv, _ := m.Embed(t.Context(), rel)
	for i, v := range rv {
		fmt.Printf("relevant %d: %.2f  %.60q\n", i, cos(q[0], v), rel[i])
	}
	start := time.Now()
	vs, _ := m.Embed(t.Context(), random)
	el := time.Since(start)
	var sims []float64
	for _, v := range vs {
		sims = append(sims, cos(q[0], v))
	}
	slices.Sort(sims)
	fmt.Printf("random: median %.2f p95 %.2f max %.2f\n", sims[len(sims)/2], sims[len(sims)*95/100], sims[len(sims)-1])
	fmt.Printf("speed: %d texts in %s -> %.0f/s, 108k in %s\n", len(random), el, float64(len(random))/el.Seconds(),
		time.Duration(float64(108000)/(float64(len(random))/el.Seconds()))*time.Second/time.Second)
	// Cross-language pairs
	pairs := [][2]string{
		{"демон теряет события при kill -9", "daemon loses spooled events after SIGKILL during drain"},
		{"как определить проект по git worktree", "project key is the git common dir so worktrees share memory"},
		{"токен телеграм бота утек в лог", "React login form validation added"},
	}
	for _, p := range pairs {
		v, _ := m.Embed(t.Context(), []string{p[0], p[1]})
		fmt.Printf("pair %.2f  %q vs %q\n", cos(v[0], v[1]), p[0], p[1])
	}
}
