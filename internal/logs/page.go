package logs

import "time"

// Defaults and ceilings. The caps exist because the runtime will happily hand us the whole
// log of a long-running container: teranode writes ~30 lines/s, so an unbounded window is
// tens of megabytes within the hour.
const (
	DefaultTail  = 500
	DefaultLimit = 2000
	MaxLines     = 5000
)

// Request is one page fetch. Cursor and Since/Tail are alternatives: with a cursor the
// window continues the stream, without one it is seeded.
type Request struct {
	Cursor    Cursor
	HasCursor bool
	Since     time.Time // seed window start; ignored when HasCursor
	Tail      int       // seed line count; ignored when HasCursor or Since is set
	Limit     int       // max lines returned
	Filter    Filter
	Raw       bool
}

// Page is one fetch: the lines that survived the filter, the cursor that continues the
// stream, and honest counts of everything dropped on the way.
type Page struct {
	Node      string `json:"node"`
	Container string `json:"container"`
	Runtime   string `json:"runtime"`

	Lines      []Line `json:"lines"` // oldest first; never null
	NextCursor string `json:"nextCursor"`

	Reset       bool   `json:"reset"`                 // the cursor could not be honoured
	ResetReason string `json:"resetReason,omitempty"` // container_restarted | log_rotated

	Scanned   int            `json:"scanned"`   // raw lines examined this fetch, after the skip
	Matched   int            `json:"matched"`   // entries passing the filter
	Dropped   int            `json:"dropped"`   // matched entries discarded by the limit (oldest first)
	Truncated bool           `json:"truncated"` // the byte ceiling cut the window
	Levels    map[string]int `json:"levels"`    // facet counts over the scanned window, PRE-filter
	Services  map[string]int `json:"services"`  // facet counts over the scanned window, PRE-filter

	FetchedAt string `json:"fetchedAt"`
	Elapsed   string `json:"elapsed"`
}

// Build turns a raw window into a page: resume after whatever the cursor already
// delivered, count facets over everything scanned, apply the filter, cap to the newest
// Limit entries, and mint the cursor that continues the stream.
func Build(raw string, req Request) Page {
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLines {
		limit = MaxLines
	}

	window, rawTotal := Scan(raw, ScanOptions{Raw: req.Raw})
	p := Page{Lines: []Line{}, Levels: map[string]int{}, Services: map[string]int{}}

	fresh := window
	offset := int64(0) // added to every Seq so numbering continues across pages
	if req.HasCursor {
		switch i, ok := locate(window, req.Cursor); {
		case ok:
			consumed := 0
			for _, l := range window[:i+1] {
				consumed += l.rawCount()
			}
			fresh = window[i+1:]
			offset = req.Cursor.Scanned - int64(consumed)
		default:
			// The window no longer contains where we were: the container restarted or the
			// log rotated past it. Deliver the whole window renumbered from zero rather
			// than emitting Seq gaps that would misreport how many lines the filter hid.
			p.Reset = true
			p.ResetReason = resetReason(window, req.Cursor)
		}
	}

	for i := range fresh {
		fresh[i].Seq += offset
		p.Scanned += fresh[i].rawCount()
		if lv := fresh[i].Level; lv != "" {
			p.Levels[lv]++
		} else {
			p.Levels["RAW"]++
		}
		if sv := fresh[i].Service; sv != "" {
			p.Services[sv]++
		}
		if req.Filter.Match(&fresh[i]) {
			p.Matched++
			p.Lines = append(p.Lines, fresh[i])
		}
	}
	// Keep the NEWEST limit entries: when tailing, the recent end is the useful one.
	if len(p.Lines) > limit {
		p.Dropped = len(p.Lines) - limit
		p.Lines = p.Lines[len(p.Lines)-limit:]
	}

	p.NextCursor = advance(req.Cursor, window, fresh, offset, rawTotal, req.HasCursor && !p.Reset).Encode()
	return p
}

// advance mints the cursor for the next fetch. With no new lines the previous cursor is
// kept verbatim, so an idle container never moves the position and never re-delivers.
func advance(prev Cursor, window, fresh []Line, offset int64, rawTotal int, keepPrev bool) Cursor {
	last := lastStamped(fresh)
	if last == nil {
		if keepPrev {
			return prev
		}
		if last = lastStamped(window); last == nil {
			return prev
		}
	}
	return Cursor{TS: last.at, Mark: markOf(last), Scanned: offset + int64(rawTotal)}
}

func lastStamped(lines []Line) *Line {
	for i := len(lines) - 1; i >= 0; i-- {
		if !lines[i].at.IsZero() {
			return &lines[i]
		}
	}
	return nil
}

// resetReason distinguishes a container that was recreated from a log that rotated past
// our position; both invalidate the cursor but they mean different things operationally.
func resetReason(window []Line, c Cursor) string {
	if len(window) > 0 && !window[0].at.IsZero() && window[0].at.After(c.TS) {
		return "log_rotated"
	}
	return "container_restarted"
}
