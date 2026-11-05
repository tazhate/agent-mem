// Package daemon runs amemd: one process per user that owns the database, answers hooks over a
// unix socket and runs background work (observer, embeddings, pruning).
package daemon

import (
	"encoding/json"
	"runtime/debug"

	"agent-mem/internal/event"
)

// Version identifies the build (release + git revision); a hook from a different build makes
// the daemon exit after answering, so the next hook starts the new binary.
var Version = buildVersion()

const release = "0.1.0"

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return release
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	v := release
	if rev != "" {
		v += "+" + rev
	}
	if dirty {
		v += "-dirty"
	}
	return v
}

// ProtocolVersion is bumped only on incompatible wire changes.
const ProtocolVersion = 1

type Request struct {
	V       int             `json:"v"`
	Version string          `json:"version"`
	Op      string          `json:"op"`
	Event   *event.Event    `json:"event,omitempty"`
	// NoContext: the runtime cannot inject context for this hook, so skip recall (and do not
	// mark anything as shown).
	NoContext bool `json:"no_context,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	V       int             `json:"v"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Context string          `json:"context,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Ops served besides "event".
const (
	OpEvent    = "event"
	OpPing     = "ping"
	OpSearch   = "search"
	OpGet      = "get"
	OpTimeline = "timeline"
	OpSave     = "save"
	OpWorkW    = "work_write"
	OpWorkR    = "work_read"
	OpStats    = "stats"
	OpShadow   = "shadow"
)

type SearchParams struct {
	Query   string   `json:"query"`
	Project string   `json:"project,omitempty"` // project name or cwd; empty = all projects
	Cwd     string   `json:"cwd,omitempty"`
	Types   []string `json:"types,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	Session string   `json:"session,omitempty"`
}

type GetParams struct {
	IDs     []int64 `json:"ids"`
	Session string  `json:"session,omitempty"`
}

type TimelineParams struct {
	Anchor int64 `json:"anchor"`
	Before int   `json:"before,omitempty"`
	After  int   `json:"after,omitempty"`
}

type SaveParams struct {
	Text   string   `json:"text"`
	Title  string   `json:"title,omitempty"`
	Type   string   `json:"type,omitempty"`
	Global bool     `json:"global,omitempty"`
	Cwd    string   `json:"cwd,omitempty"`
	Files  []string `json:"files,omitempty"`
}

type WorkParams struct {
	List          string         `json:"list,omitempty"`
	Fields        map[string]any `json:"fields,omitempty"`
	Cwd           string         `json:"cwd,omitempty"`
	IncludeClosed bool           `json:"include_closed,omitempty"`
}
