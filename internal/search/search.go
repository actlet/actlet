// Package search finds activity events: by kind, worker, tool and time,
// and by text, a phrase or a regular expression, anywhere in the event.
package search

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
)

// Filter is what an event must match. Zero fields match everything.
type Filter struct {
	// Text is a phrase, matched regardless of case, or with Regexp, a
	// regular expression, matched against the event's text.
	Text   string
	Regexp *regexp.Regexp
	// Kinds are event kinds, such as "tool_finished": the event matches
	// any.
	Kinds []string
	// Worker matches the event's worker, such as "root.1".
	Worker string
	// Tool matches tool calls of this tool: their start and end.
	Tool string
	// Since and Until bound the event's time.
	Since, Until time.Time
}

// Match reports whether the event matches the filter.
func (f *Filter) Match(ev *gatewayv0.ActivityEvent) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, Kind(ev)) {
		return false
	}
	if f.Worker != "" && ev.GetWorker() != f.Worker {
		return false
	}
	if f.Tool != "" && ev.GetToolStarted().GetTool() != f.Tool && ev.GetToolFinished().GetTool() != f.Tool {
		return false
	}
	if !f.Since.IsZero() || !f.Until.IsZero() {
		at := ev.GetTime().AsTime()
		if !f.Since.IsZero() && at.Before(f.Since) || !f.Until.IsZero() && at.After(f.Until) {
			return false
		}
	}
	switch {
	case f.Regexp != nil:
		return f.Regexp.MatchString(Text(ev))
	case f.Text != "":
		return strings.Contains(strings.ToLower(Text(ev)), strings.ToLower(f.Text))
	}
	return true
}

// Kind names an event's kind after its body's field in the protocol, such
// as "text" or "model_turn". A body this build doesn't know is "unknown".
func Kind(ev *gatewayv0.ActivityEvent) string {
	m := ev.ProtoReflect()
	if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("body")); fd != nil {
		return string(fd.Name())
	}
	return "unknown"
}

// Kinds are the event kinds this build knows.
func Kinds() []string {
	var out []string
	fields := (&gatewayv0.ActivityEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("body").Fields()
	for i := range fields.Len() {
		out = append(out, string(fields.Get(i).Name()))
	}
	return out
}

// Text is every string in the event's body, and its worker, one per line:
// what a text search looks at.
func Text(ev *gatewayv0.ActivityEvent) string {
	var sb strings.Builder
	sb.WriteString(ev.GetWorker())
	m := ev.ProtoReflect()
	if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("body")); fd != nil {
		strs(&sb, m.Get(fd).Message())
	}
	return sb.String()
}

// strs writes a message's strings, recursively.
func strs(sb *strings.Builder, m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				item(sb, fd, l.Get(i))
			}
		case fd.IsMap():
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				sb.WriteString("\n" + k.String())
				item(sb, fd.MapValue(), mv)
				return true
			})
		default:
			item(sb, fd, v)
		}
		return true
	})
}

func item(sb *strings.Builder, fd protoreflect.FieldDescriptor, v protoreflect.Value) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		sb.WriteString("\n" + v.String())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		strs(sb, v.Message())
	}
}
