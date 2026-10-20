package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/actlet/actlet/internal/agent"
	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/session/sessionpb"
	"github.com/actlet/actlet/internal/sessionfs"
	attachv0 "github.com/actlet/actlet/proto/actlet/attach/v0"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// maxInfoText bounds the task and manifest in a run's session_info event:
// activity carries only small content.
const maxInfoText = 8 << 10

// sessionInfo describes the run for viewers.
func sessionInfo(cfg Config, folder sessionfs.Folder, task *sessionfs.Task, p *gatewayv0.Profile,
	effective *sessionpb.Config, settings agent.Settings, commands []string, log *slog.Logger) *gatewayv0.SessionInfo {
	info := &gatewayv0.SessionInfo{
		TaskId: task.ID, TaskKind: task.Kind, Budgets: p.GetBudgets(), Models: p.GetModels(), Grants: p.GetTools(),
		WorkerId: cfg.WorkerID, ActletVersion: cfg.Version, LocalCommands: commands,
	}
	if data, err := folder.TaskJSON(); err == nil {
		info.Task = cutText(string(data), maxInfoText)
	}
	if data, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(effective); err == nil {
		// protojson varies its spacing on purpose; indent it steadily.
		var buf bytes.Buffer
		if json.Indent(&buf, data, "", "  ") == nil {
			info.Config = buf.String()
		}
	}
	var notes []string
	for _, m := range settings.Prompt.Manifests {
		if s := strings.TrimSpace(m); s != "" {
			notes = append(notes, s)
		}
	}
	if task.Kind == agent.Kind {
		tools, toolNotes, err := agent.Tools(settings, folder)
		if err != nil {
			log.Warn("describing the tools", "err", err)
		}
		info.Tools = tools
		notes = append(notes, toolNotes...)
	}
	info.ToolsManifest = cutText(strings.Join(notes, "\n\n"), maxInfoText)
	return info
}

func cutText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return s[:limit] + "\n[cut]"
}

// box is the running session as local viewers see it.
type box struct {
	sess      *session.Session
	sessionID string
	epoch     uint64
	cfg       Config
	folder    sessionfs.Folder
	client    *gwclient.Client
	started   time.Time
}

func (b *box) Info() *attachv0.InfoResponse {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return &attachv0.InfoResponse{
		SessionId: b.sessionID, Epoch: b.epoch, WorkerId: b.cfg.WorkerID, ActletVersion: b.cfg.Version, State: b.sess.State(),
		Stats: &attachv0.Stats{
			Started:          timestamppb.New(b.started),
			FolderBytes:      dirSize(b.folder.Dir),
			OutputBytes:      dirSize(b.folder.Output()),
			MemoryBytes:      mem.Sys,
			Cpu:              durationpb.New(cpuTime()),
			Gateway:          b.client.Addr(),
			GatewayConnected: b.client.Connected(),
		},
	}
}

func (b *box) Apply(ctx context.Context, cmd *gatewayv0.Command) (*gatewayv0.CommandAck, error) {
	return b.sess.Apply(ctx, cmd)
}

// dirSize adds up the sizes of the files under dir, looking at most at
// 100000 of them.
func dirSize(dir string) int64 {
	var total int64
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > 100000 {
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}
