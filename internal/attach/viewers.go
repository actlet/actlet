package attach

import (
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/view"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
)

// Kinds of viewers.
const (
	ViewerLocal        = "local"
	ViewerDirect       = "direct"
	ViewerOrchestrator = "orchestrator"
)

// maxViewerLabel bounds a viewer's name, in characters.
const maxViewerLabel = 64

// ViewerLabel cleans a viewer's untrusted name for others to see: no
// escape sequences, one line, at most 64 characters; def when it is
// empty.
func ViewerLabel(name, def string) string {
	s := strings.Join(strings.Fields(view.Sanitize(name)), " ")
	if r := []rune(s); len(r) > maxViewerLabel {
		s = string(r[:maxViewerLabel])
	}
	if s == "" {
		return def
	}
	return s
}

// Viewers are the viewers watching a session now: one per open Watch.
// They are only kept in memory, never recorded.
type Viewers struct {
	mu   sync.Mutex
	next int
	open map[int]*viewer
}

type viewer struct {
	kind, label, id string
	since           time.Time
}

// NewViewers returns an empty set.
func NewViewers() *Viewers { return &Viewers{open: map[int]*viewer{}} }

// Add adds a viewer, whose ID is the one it gave its Watch, and returns
// the function that removes it.
func (v *Viewers) Add(kind, label, id string) (remove func()) {
	if len(id) > maxViewerLabel {
		id = id[:maxViewerLabel]
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.next++
	key := v.next
	v.open[key] = &viewer{kind: kind, label: label, id: id, since: time.Now()}
	return func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		delete(v.open, key)
	}
}

// List returns the viewers, the earliest first, marking the one with the
// ID you.
func (v *Viewers) List(you string) []*attachv0.Viewer {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*attachv0.Viewer, 0, len(v.open))
	for _, w := range v.open {
		out = append(out, &attachv0.Viewer{Kind: w.kind, Label: w.label, Since: timestamppb.New(w.since), You: you != "" && w.id == you})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].GetSince().AsTime(), out[j].GetSince().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].GetLabel() < out[j].GetLabel()
	})
	return out
}
