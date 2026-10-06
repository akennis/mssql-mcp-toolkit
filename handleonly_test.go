package main

import (
	"strings"
	"testing"
)

// An id parameter takes only @handle.Column: ids typed out, or sent as JSON
// numbers, are refused before the query runs, and a handle still works.
func TestIDParameterTakesOnlyHandles(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "schedules", nil))

	for _, v := range []any{"1,2", "10231", 5, float64(5)} {
		res := callQueryTool(t, cs, "schedules", map[string]any{"PersonID": v})
		if !res.IsError || !strings.Contains(contentText(res), "takes only a reference to a stored result") {
			t.Errorf("PersonID=%v: want a handle-only refusal, got error=%v %s", v, res.IsError, contentText(res))
		}
	}
	for _, v := range []string{"@" + h + ".PersonID", "@" + h} {
		if res := callQueryTool(t, cs, "schedules", map[string]any{"PersonID": v}); res.IsError {
			t.Errorf("PersonID=%s: %s", v, contentText(res))
		}
	}
	// A literal parameter still takes its list typed out.
	if res := callQueryTool(t, cs, "years", map[string]any{"SchoolYear": "2024-2025,2025-2026"}); res.IsError {
		t.Errorf("literal SchoolYear list: %s", contentText(res))
	}
}

// A resolver's single record is stored, so its id can be passed on.
func TestResolverRecordIsStoredUnderHandle(t *testing.T) {
	const tools = `
- name: find
  description: Find one person.
  query: SELECT OnePerson
  outputFormat: csv
  pickRecord: optional
- name: schedules
  description: Schedules.
  query: SELECT Schedules WHERE PersonID IN (SELECT value FROM STRING_SPLIT(@PersonID, ','))
  outputFormat: csv
  parameters:
  - {name: PersonID, type: string}
`
	cs, _ := handleClient(t, tools, nil)
	for _, args := range []map[string]any{nil, {"RequireSingle": true}} {
		h := handleOf(t, callQueryTool(t, cs, "find", args))
		res := callQueryTool(t, cs, "schedules", map[string]any{"PersonID": "@" + h + ".PersonID"})
		if res.IsError {
			t.Errorf("args %v: handle %s not usable: %s", args, h, contentText(res))
		}
	}
}

const hiddenTools = `
- name: sched
  description: Schedules; the PersonID is stored, not shown.
  query: SELECT Schedules
  outputFormat: csv
  columns: [CourseName, Credits]
- name: find
  description: Find one person; the PersonID is stored, not shown.
  query: SELECT OnePerson
  outputFormat: csv
  pickRecord: optional
  columns: [first, last]
- name: use
  description: Uses stored ids.
  query: SELECT Schedules WHERE PersonID IN (SELECT value FROM STRING_SPLIT(@PersonID, ','))
  outputFormat: csv
  parameters:
  - {name: PersonID, type: string}
  columns: [CourseName]
`

// A column left out of a tool's columns menu is stored but never shown; the
// handle line names it, and it still passes on as @handle.Column.
func TestHiddenIDIsStoredNotShown(t *testing.T) {
	cs, _ := handleClient(t, hiddenTools, nil)
	res := callQueryTool(t, cs, "sched", nil)
	text := contentText(res)
	if header := tableHeader(text); header != "CourseName,Credits" {
		t.Errorf("table header = %q, want the menu columns only:\n%s", header, text)
	}
	for _, want := range []string{"also stored: PersonID", "pass on as "} {
		if !strings.Contains(text, want) {
			t.Errorf("reply lacks %q:\n%s", want, text)
		}
	}
	h := handleOf(t, res)
	if r := callQueryTool(t, cs, "use", map[string]any{"PersonID": "@" + h + ".PersonID"}); r.IsError {
		t.Errorf("hidden id not usable: %s", contentText(r))
	}
}

// A resolver's chosen record keeps its hidden id, and a derived result stays
// shielded: an operator's output neither shows the id nor lets show ask for it.
func TestHiddenIDSurvivesResolverOperatorsAndShow(t *testing.T) {
	cs, _ := handleClient(t, hiddenTools, nil)
	fh := handleOf(t, callQueryTool(t, cs, "find", map[string]any{"RequireSingle": true}))
	if r := callQueryTool(t, cs, "use", map[string]any{"PersonID": "@" + fh + ".PersonID"}); r.IsError {
		t.Fatalf("resolver's hidden id lost: %s", contentText(r))
	}

	sh := handleOf(t, callQueryTool(t, cs, "sched", nil))
	// A filter keeps the columns of its input, ids hidden as before.
	res := callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": sh, "Where": "Credits >= 0"})
	if res.IsError {
		t.Fatalf("filter: %s", contentText(res))
	}
	text := contentText(res)
	if strings.Contains(tableHeader(text), "PersonID") || !strings.Contains(text, "also stored: PersonID") {
		t.Errorf("derived result shows or omits its hidden id:\n%s", text)
	}
	oh := handleOf(t, res)
	if r := callQueryTool(t, cs, "use", map[string]any{"PersonID": "@" + oh + ".PersonID"}); r.IsError {
		t.Errorf("derived result lost its id: %s", contentText(r))
	}
	// show takes only visible columns, so it cannot be made to display it either.
	r := callQueryTool(t, cs, "sales_show", map[string]any{"Handle": sh, "Columns": "CourseName"})
	if r2 := callQueryTool(t, cs, "sales_show", map[string]any{"Handle": sh, "Columns": "PersonID"}); !r2.IsError {
		t.Errorf("show accepted a hidden column:\n%s", contentText(r2))
	}
	if strings.Contains(tableHeader(contentText(r)), "PersonID") || !strings.Contains(contentText(r), "also stored: PersonID") {
		t.Errorf("show displayed a hidden id, or did not say where it is:\n%s", contentText(r))
	}
}

// tableHeader is the first line after the handle line's blank line.
func tableHeader(text string) string {
	_, table, _ := strings.Cut(text, "\n\n")
	line, _, _ := strings.Cut(table, "\n")
	return line
}
