package main

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// What one call is allowed to return.
//
// Every result of this server is read by a model with a finite context window,
// and a single unbounded SELECT is enough to fill it: a whole table at the row
// level, or one NVARCHAR(MAX) cell at the value level. Three limits are needed
// rather than one because they fail differently — a million small rows, a
// hundred rows carrying a blob each, and one row holding a megabyte all
// overflow the same window by different routes, and a cap on rows alone only
// catches the first.
type budget struct {
	// rows, bytes and cellBytes each use 0 for "no cap", matching --max-rows,
	// so that an operator who deliberately wants everything says so the same
	// way for all three.
	rows      int
	bytes     int
	cellBytes int
}

func budgetFor(cfg *config) budget {
	return budget{rows: cfg.maxRows, bytes: cfg.maxBytes, cellBytes: cfg.maxCellBytes}
}

// rowSize is what appending one row costs the payload: the length of its JSON
// encoding, plus a byte for the comma that will separate it from the next.
//
// Every value in the row has already been through convertValue, so Marshal
// cannot fail; if it somehow does, the row is counted as free rather than
// failing a result set over an accounting detail.
func rowSize(row map[string]any) int {
	data, err := json.Marshal(row)
	if err != nil {
		return 0
	}
	return len(data) + 1
}

// truncateCell cuts a single value down to the per-cell budget, reporting
// whether it had to.
//
// The cut lands on a rune boundary: half a UTF-8 sequence is not a string, and
// it would be the JSON encoder — not this function — that noticed. The marker
// gives the original size, because "this was cut" and "this was cut from
// 1.2 MiB" lead to different next queries.
func truncateCell(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	// Back off a byte at a time until the tail decodes: DecodeLastRuneInString
	// reports a one-byte RuneError only for an invalid sequence, so a string
	// that genuinely ends in U+FFFD is left alone.
	cut := s[:limit]
	for len(cut) > 0 {
		if r, size := utf8.DecodeLastRuneInString(cut); r == utf8.RuneError && size <= 1 {
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	// The marker pushes the cell slightly past the budget. That is deliberate:
	// a value silently shortened to exactly the limit is indistinguishable
	// from one that happened to be that long.
	return cut + fmt.Sprintf("…[truncated, %s total]", formatBytes(len(s))), true
}

// formatBytes writes a byte count the way the person reading a note thinks
// about it. Binary units, since the budgets themselves are set in them.
func formatBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 2 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", value, [...]string{"KiB", "MiB", "GiB"}[exp])
}
