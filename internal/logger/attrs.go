package logger

import (
	"context"
	"log/slog"
)

// WithoutAttr returns a new logger with the given attribute key removed from the
// logger's currently attached attributes.
func WithoutAttr(l *slog.Logger, key string) *slog.Logger {
	if l == nil {
		return nil
	}

	if h, ok := l.Handler().(*attrFilterHandler); ok {
		return slog.New(h.withoutAttr(key))
	}

	return slog.New(&recordAttrFilterHandler{
		next:      l.Handler(),
		removeKey: key,
	})
}

// attrFilterHandler captures logger-level attrs and reapplies them to each record.
// It lets WithoutAttr remove already-attached attrs when this wrapper is present.
type attrFilterHandler struct {
	next  slog.Handler
	attrs []slog.Attr
}

func newAttrFilterHandler(next slog.Handler) *attrFilterHandler {
	return &attrFilterHandler{next: next}
}

func (h *attrFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *attrFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	filtered := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	filtered.AddAttrs(h.attrs...)
	appendFilteredRecordAttrs(&filtered, record, "")

	return h.next.Handle(ctx, filtered)
}

func (h *attrFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)

	return &attrFilterHandler{
		next:  h.next,
		attrs: merged,
	}
}

func (h *attrFilterHandler) WithGroup(name string) slog.Handler {
	attrs := make([]slog.Attr, len(h.attrs))
	copy(attrs, h.attrs)

	return &attrFilterHandler{
		next:  h.next.WithGroup(name),
		attrs: attrs,
	}
}

func (h *attrFilterHandler) withoutAttr(key string) *attrFilterHandler {
	return &attrFilterHandler{
		next:  h.next,
		attrs: filterAttrs(h.attrs, key),
	}
}

// recordAttrFilterHandler removes one attribute key from emitted records.
// It is used when the underlying handler is not attrFilterHandler.
type recordAttrFilterHandler struct {
	next      slog.Handler
	removeKey string
}

func (h *recordAttrFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *recordAttrFilterHandler) Handle(ctx context.Context, record slog.Record) error {
	filtered := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	appendFilteredRecordAttrs(&filtered, record, h.removeKey)

	return h.next.Handle(ctx, filtered)
}

func (h *recordAttrFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordAttrFilterHandler{
		next:      h.next.WithAttrs(filterAttrs(attrs, h.removeKey)),
		removeKey: h.removeKey,
	}
}

func (h *recordAttrFilterHandler) WithGroup(name string) slog.Handler {
	return &recordAttrFilterHandler{
		next:      h.next.WithGroup(name),
		removeKey: h.removeKey,
	}
}

func filterAttrs(attrs []slog.Attr, removeKey string) []slog.Attr {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, attr := range attrs {
		if attr.Key == removeKey {
			continue
		}

		filtered = append(filtered, attr)
	}

	return filtered
}

func appendFilteredRecordAttrs(dst *slog.Record, src slog.Record, removeKey string) {
	src.Attrs(func(attr slog.Attr) bool {
		if removeKey != "" && attr.Key == removeKey {
			return true
		}

		dst.AddAttrs(attr)

		return true
	})
}
