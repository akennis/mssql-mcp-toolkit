package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestProfileClassifiesAndOrdersColumns(t *testing.T) {
	r := &storedResult{
		Columns: []string{"PersonID", "LastName", "Building", "StartDate", "Average", "Room"},
		Types:   []string{"INT", "NVARCHAR", "NVARCHAR", "DATE", "DECIMAL", "NVARCHAR"},
	}
	for i := range 60 {
		building := "Central HS"
		if i >= 57 {
			building = "Central MS"
		}
		var room any = fmt.Sprintf("R%d", i%20)
		if i%10 == 0 {
			room = nil
		}
		r.Rows = append(r.Rows, map[string]any{
			"PersonID":  int64(i % 55), // 5 students appear twice
			"LastName":  fmt.Sprintf("Name%02d", i),
			"Building":  building,
			"StartDate": fmt.Sprintf("2025-09-%02dT00:00:00Z", 1+i%28),
			"Average":   fmt.Sprintf("%d.5", 50+i%40),
			"Room":      room,
		})
	}
	text := buildProfile(r).text()
	lines := strings.Split(strings.TrimSpace(text), "\n")
	want := []string{
		"Profile (60 rows):",
		"- Building: Central HS (57), Central MS (3)",
		"- StartDate: 2025-09-01 to 2025-09-28",
		"- Average: 50.5 to 89.5",
		"- PersonID: 55 distinct (some repeat across the 60 rows)",
		"- LastName: 60 distinct",
		"- Room: 18 distinct; 6 NULL",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("profile =\n%s\nwant\n%s", text, strings.Join(want, "\n"))
	}
}

func TestProfileStaysWithinBudget(t *testing.T) {
	r := &storedResult{}
	for c := range 200 {
		r.Columns = append(r.Columns, fmt.Sprintf("Column_%03d_with_a_long_name", c))
		r.Types = append(r.Types, "NVARCHAR")
	}
	for i := range 60 {
		row := map[string]any{}
		for _, name := range r.Columns {
			row[name] = fmt.Sprintf("%s-%d", strings.Repeat("v", 30), i%2) // two values each
		}
		r.Rows = append(r.Rows, row)
	}
	text := buildProfile(r).text()
	if len(text) > profileBudget+100 {
		t.Errorf("profile is %d bytes, budget %d", len(text), profileBudget)
	}
	if !strings.Contains(text, "more column(s) not profiled") {
		t.Errorf("a cut profile does not say so:\n%s", text)
	}
}

// Columns with one value everywhere, and columns that are always NULL, say
// one thing each however many rows there are: one line for all of them.
func TestProfileFoldsConstantAndNullColumns(t *testing.T) {
	r := &storedResult{
		Columns: []string{"SchCourseID", "IsCTE", "SchoolYear", "Department", "Subjects", "StateCourses"},
		Types:   []string{"INT", "BIT", "NVARCHAR", "NVARCHAR", "NVARCHAR", "NVARCHAR"},
	}
	for i := range 63 {
		dept := "HomeRoom"
		if i < 8 {
			dept = "Homeroom PM"
		}
		r.Rows = append(r.Rows, map[string]any{
			"SchCourseID": int64(i), "IsCTE": false, "SchoolYear": "2026-2027",
			"Department": dept, "Subjects": nil, "StateCourses": nil,
		})
	}
	want := strings.Join([]string{
		"Profile (63 rows):",
		"- Same in every row: IsCTE=false, SchoolYear=2026-2027",
		"- Department: HomeRoom (55), Homeroom PM (8)",
		"- SchCourseID: 63 distinct",
		"- Always NULL: Subjects, StateCourses",
	}, "\n")
	if got := strings.TrimSpace(buildProfile(r).text()); got != want {
		t.Errorf("profile =\n%s\nwant\n%s", got, want)
	}
}
