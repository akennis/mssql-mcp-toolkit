package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The profile is how a model judges a result too large to read. A row count
// says whether something came back; the profile says whether it is the right
// something: which years, buildings and courses it spans, how many distinct
// students it holds against how many rows, the range its dates cover. A
// Homeroom roster whose Building line reads "Central HS (590), Central MS
// (22)" has picked up a middle-school course, and the model can see that
// before it spends the next call on it.
//
// It is deterministic (the same rows always give the same text) and bounded
// (profileBudget), so it costs the same whether the result is 60 rows or ten
// thousand.

const (
	// profileBreakdownMax is the most distinct values a column can have and
	// still be listed value by value.
	profileBreakdownMax = 12
	// profileBudget caps the rendered profile, in bytes.
	profileBudget = 2048
	// profileValueMax clips each listed value.
	profileValueMax = 40
)

// valueCount is one listed value and how many rows carry it.
type valueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// columnProfile summarizes one column.
type columnProfile struct {
	Column   string       `json:"column"`
	Distinct int          `json:"distinct"`
	Nulls    int          `json:"nulls,omitempty"`
	Values   []valueCount `json:"values,omitempty"`
	Min      string       `json:"min,omitempty"`
	Max      string       `json:"max,omitempty"`
	// Kind is how the column is summarized: constant, all_null, breakdown,
	// range, id or distinct.
	Kind string `json:"kind"`

	kind profileKind
}

type profileKind int

const (
	kindDistinct  profileKind = iota // just a distinct count
	kindBreakdown                    // every value with its count
	kindRange                        // min and max
	kindID                           // an id column: distinct count first
	kindConstant                     // one value in every row
	kindAllNull                      // NULL in every row
)

var profileKindNames = map[profileKind]string{
	kindDistinct: "distinct", kindBreakdown: "breakdown", kindRange: "range",
	kindID: "id", kindConstant: "constant", kindAllNull: "all_null",
}

// resultProfile is the whole summary.
type resultProfile struct {
	Rows    int             `json:"rows"`
	Columns []columnProfile `json:"columns"`
}

// isIDColumn reports whether a column name reads as an identifier:
// PersonID, SchoolID, SchCourseScheduleID, Id.
func isIDColumn(name string) bool {
	return strings.HasSuffix(name, "ID") || strings.HasSuffix(name, "Id") || strings.EqualFold(name, "id")
}

// isNumericType and isDateType classify SQL Server type names as the driver
// reports them, plus the synthetic ones derived results carry.
func isNumericType(t string) bool {
	switch t {
	case "INT", "BIGINT", "SMALLINT", "TINYINT", "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY", "FLOAT", "REAL":
		return true
	}
	return false
}

func isDateType(t string) bool {
	switch t {
	case "DATE", "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET":
		return true
	}
	return false
}

// profileText renders one value the way the profile lists it: NULL spelled
// out, a midnight timestamp shortened to its date, and clipped.
func profileText(v any) string {
	if v == nil {
		return "NULL"
	}
	s := scalarString(v)
	if strings.HasSuffix(s, "T00:00:00Z") && len(s) == len("2006-01-02T00:00:00Z") {
		s = s[:10]
	}
	return clip(formatCell(s), profileValueMax)
}

// buildProfile summarizes every column of r.
func buildProfile(r *storedResult) *resultProfile {
	p := &resultProfile{Rows: len(r.Rows)}
	for _, col := range r.Columns {
		typ := r.typeOf(col)
		counts := map[string]int{}
		cp := columnProfile{Column: col}
		var lo, hi string
		var loN, hiN float64
		haveRange := false
		for _, row := range r.Rows {
			v := row[col]
			if v == nil {
				cp.Nulls++
				continue
			}
			key := profileText(v)
			counts[key]++
			switch {
			case isNumericType(typ):
				f, err := strconv.ParseFloat(scalarString(v), 64)
				if err != nil {
					continue
				}
				if !haveRange || f < loN {
					loN, lo = f, key
				}
				if !haveRange || f > hiN {
					hiN, hi = f, key
				}
				haveRange = true
			case isDateType(typ):
				if !haveRange || key < lo {
					lo = key
				}
				if !haveRange || key > hi {
					hi = key
				}
				haveRange = true
			}
		}
		cp.Distinct = len(counts)
		switch {
		case len(r.Rows) > 0 && cp.Distinct == 0:
			cp.kind = kindAllNull
		case cp.Distinct == 1 && cp.Nulls == 0:
			// One value everywhere says one thing, however many rows: fold
			// these into a single line rather than a breakdown each.
			cp.kind = kindConstant
			for v, n := range counts {
				cp.Values = []valueCount{{Value: v, Count: n}}
			}
		case isIDColumn(col):
			cp.kind = kindID
		case cp.Distinct > 0 && cp.Distinct <= profileBreakdownMax:
			cp.kind = kindBreakdown
			for v, n := range counts {
				cp.Values = append(cp.Values, valueCount{Value: v, Count: n})
			}
			sort.Slice(cp.Values, func(i, j int) bool {
				if cp.Values[i].Count != cp.Values[j].Count {
					return cp.Values[i].Count > cp.Values[j].Count
				}
				return cp.Values[i].Value < cp.Values[j].Value
			})
		case haveRange:
			cp.kind = kindRange
			cp.Min, cp.Max = lo, hi
		default:
			cp.kind = kindDistinct
		}
		cp.Kind = profileKindNames[cp.kind]
		p.Columns = append(p.Columns, cp)
	}
	// What is the same in every row first, and named breakdowns next — they
	// are what tells a model whether the scope is right — then ranges, then
	// the id counts, then everything else, and last the columns that are
	// always NULL.
	sort.SliceStable(p.Columns, func(i, j int) bool {
		return profileRank(p.Columns[i].kind) < profileRank(p.Columns[j].kind)
	})
	return p
}

func profileRank(k profileKind) int {
	switch k {
	case kindConstant:
		return 0
	case kindBreakdown:
		return 1
	case kindRange:
		return 2
	case kindID:
		return 3
	case kindAllNull:
		return 5
	default:
		return 4
	}
}

// text renders the profile as a short bulleted list, within profileBudget.
func (p *resultProfile) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Profile (%d rows):\n", p.Rows)
	// The constant and all-NULL columns each collapse into one line.
	var same, null []string
	var rest []columnProfile
	for _, c := range p.Columns {
		switch c.kind {
		case kindConstant:
			same = append(same, c.Column+"="+c.Values[0].Value)
		case kindAllNull:
			null = append(null, c.Column)
		default:
			rest = append(rest, c)
		}
	}
	if len(same) > 0 {
		fmt.Fprintf(&b, "- Same in every row: %s\n", clip(strings.Join(same, ", "), profileBudget/2))
	}
	for i, c := range rest {
		var line strings.Builder
		fmt.Fprintf(&line, "- %s: ", c.Column)
		switch c.kind {
		case kindBreakdown:
			parts := make([]string, len(c.Values))
			for k, v := range c.Values {
				parts[k] = fmt.Sprintf("%s (%d)", v.Value, v.Count)
			}
			line.WriteString(strings.Join(parts, ", "))
		case kindRange:
			fmt.Fprintf(&line, "%s to %s", c.Min, c.Max)
		case kindID:
			fmt.Fprintf(&line, "%d distinct", c.Distinct)
			if c.Distinct < p.Rows-c.Nulls {
				fmt.Fprintf(&line, " (some repeat across the %d rows)", p.Rows)
			}
		default:
			fmt.Fprintf(&line, "%d distinct", c.Distinct)
		}
		if c.Nulls > 0 {
			fmt.Fprintf(&line, "; %d NULL", c.Nulls)
		}
		line.WriteByte('\n')
		if b.Len()+line.Len() > profileBudget {
			fmt.Fprintf(&b, "- … %d more column(s) not profiled\n", len(rest)-i)
			break
		}
		b.WriteString(line.String())
	}
	if len(null) > 0 {
		fmt.Fprintf(&b, "- Always NULL: %s\n", clip(strings.Join(null, ", "), 400))
	}
	return b.String()
}
