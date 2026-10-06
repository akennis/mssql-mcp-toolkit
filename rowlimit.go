package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// rowLimit is the optional per-call row cap sent by the MCP client.
//
// Clients disagree about how a numeric tool argument is rendered: some emit a
// JSON number, others quote the same value as a string ("500"). Both forms
// mean the same thing to a caller, so both are accepted here rather than
// failing the call over a quoting difference.
type rowLimit int

// maxRowLimit bounds what is accepted, so an absurd value cannot overflow the
// conversion to int. Any value at or above the server's own cap is lowered to
// it anyway, so the exact ceiling is not significant.
const maxRowLimit = math.MaxInt32

// UnmarshalJSON accepts a JSON number (500), a whole-valued float (500.0), or
// either of those quoted as a string ("500"). A JSON null or an empty string
// means "not supplied", which leaves the server's configured cap in force.
func (r *rowLimit) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		*r = 0
		return nil
	}

	// The string form: unquote it and treat the contents as the number.
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal([]byte(text), &s); err != nil {
			return fmt.Errorf("max_rows: %s is not valid JSON", text)
		}
		text = strings.TrimSpace(s)
		if text == "" {
			*r = 0
			return nil
		}
	}

	n, err := parseWholeNumber(text)
	if err != nil {
		return fmt.Errorf("max_rows: %w", err)
	}
	*r = rowLimit(n)
	return nil
}

// parseWholeNumber reads a whole number of rows written in either integer or
// floating point notation (500, 500.0, 5e2). Zero means "no cap"; a negative
// value is rejected rather than silently read as one, because the caller meant
// something by it and "no cap" is almost certainly not it.
func parseWholeNumber(text string) (int, error) {
	if n, err := strconv.Atoi(text); err == nil {
		return checkRange(float64(n), text)
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%q is not a number; pass a whole number of rows, such as 100", text)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("%s is not a whole number of rows", text)
	}
	return checkRange(f, text)
}

func checkRange(f float64, text string) (int, error) {
	if f < 0 {
		return 0, fmt.Errorf("%s is negative; pass 0 (or omit max_rows) for no cap", text)
	}
	if f > maxRowLimit {
		return 0, fmt.Errorf("%s is too large; the maximum is %d", text, maxRowLimit)
	}
	return int(f), nil
}
