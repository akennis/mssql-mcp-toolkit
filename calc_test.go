package main

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// The motivating question, end to end: which students are scheduled into a
// homeroom and nothing else?
func TestHomeroomOnlyChain(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))
	res := callQueryTool(t, cs, "sales_group_aggregate", map[string]any{
		"Handle":     sched,
		"By":         "PersonID",
		"Aggregates": "count() AS courses, count_if(CourseName LIKE '%homeroom%') AS homerooms",
		"Having":     "courses = homerooms AND homerooms > 0",
	})
	text := contentText(res)
	if res.IsError {
		t.Fatal(text)
	}
	// 1 and 4 (HOMEROOM 10: LIKE is case-insensitive, as in SQL Server); not 2
	// (also English), 3 (Algebra only) or 5 (a course with no name).
	if !strings.Contains(text, "PersonID,courses,homerooms\n1,1,1\n4,1,1\n") || strings.Contains(text, "\n2,") {
		t.Errorf("homeroom-only students:\n%s", text)
	}
	if !strings.Contains(text, "from: "+sched+" = schedules()") {
		t.Errorf("lineage missing:\n%s", text)
	}
}

func TestSetOperators(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))                             // ids 1,2,3
	four := handleOf(t, callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3,4"})) // ids 1-4
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))                           // PersonID 1-5

	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"sales_set_union", map[string]any{"Handle": people + "," + four, "Key": "id"}, "id\n1\n2\n3\n4\n"},
		{"sales_set_intersect", map[string]any{"Handle": four + "," + people, "Key": "id"}, "id\n1\n2\n3\n"},
		{"sales_set_difference", map[string]any{"Handle": four + "," + people, "Key": "id"}, "id\n4\n"},
		// Carry returns the first handle's whole rows.
		{"sales_set_intersect", map[string]any{"Handle": people + "," + four, "Key": "id", "Carry": true}, "id,first,last,title\n1,ada,lovelace,analyst\n"},
	} {
		res := callQueryTool(t, cs, c.tool, c.args)
		if res.IsError || !strings.Contains(contentText(res), c.want) {
			t.Errorf("%s %v:\n%s\nwant %q", c.tool, c.args, contentText(res), c.want)
		}
	}
	// Different column names: rename first with project, then compare.
	renamed := handleOf(t, callQueryTool(t, cs, "sales_project", map[string]any{"Handle": sched, "Columns": "PersonID AS id", "Distinct": true}))
	res := callQueryTool(t, cs, "sales_set_difference", map[string]any{"Handle": renamed + "," + four, "Key": "id"})
	if text := contentText(res); !strings.Contains(text, "id\n5\n") {
		t.Errorf("difference over a renamed column:\n%s", text)
	}
	// A key missing from one input is an error that lists its columns.
	res = callQueryTool(t, cs, "sales_set_union", map[string]any{"Handle": people + "," + sched, "Key": "id"})
	if !res.IsError || !strings.Contains(contentText(res), "has no column \"id\"") {
		t.Errorf("missing key: %s", contentText(res))
	}
}

func TestFilterSortProjectJoin(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))

	check := func(tool string, args map[string]any, want ...string) string {
		t.Helper()
		res := callQueryTool(t, cs, tool, args)
		text := contentText(res)
		if res.IsError {
			t.Errorf("%s %v: %s", tool, args, text)
			return ""
		}
		for _, w := range want {
			if !strings.Contains(text, w) {
				t.Errorf("%s %v:\n%s\nwant %q", tool, args, text, w)
			}
		}
		return text
	}
	// Decimals compare as numbers, dates as dates, text without case.
	check("sales_filter", map[string]any{"Handle": sched, "Where": "Credits >= 0.5 AND StartDate < '2026-01-01'"},
		"2,English 9,1,2025-09-03", "3,Algebra,1,2025-09-04")
	check("sales_filter", map[string]any{"Handle": sched, "Where": "CourseName = 'homeroom 9' OR CourseName IS NULL"},
		"\n1,Homeroom 9,", "\n2,Homeroom 9,", "\n5,,0.5,2026-01-20")
	check("sales_filter", map[string]any{"Handle": sched, "Where": "PersonID NOT IN (1, 2) AND NOT CourseName LIKE 'Alg%'"},
		"PersonID,CourseName,Credits,StartDate\n4,HOMEROOM 10,")
	check("sales_filter", map[string]any{"Handle": sched, "Where": "Credits BETWEEN 0.1 AND 0.9"}, "\n5,,0.5,")
	check("sales_sort_limit", map[string]any{"Handle": sched, "OrderBy": "StartDate DESC, PersonID", "Limit": 2},
		"PersonID,CourseName,Credits,StartDate\n5,,0.5,2026-01-20\n4,HOMEROOM 10,0,2025-09-05\n")
	check("sales_project", map[string]any{"Handle": sched, "Columns": "CourseName AS Course", "Distinct": true},
		"Course\nHomeroom 9\nEnglish 9\nAlgebra\nHOMEROOM 10\n\n")
	// Joins: inner adds the right side's columns, anti keeps the unmatched.
	check("sales_join", map[string]any{"Left": sched, "Right": people, "On": "PersonID = id"},
		"PersonID,CourseName,Credits,StartDate,id,first,last,title\n1,Homeroom 9,0,2025-09-03,1,ada,lovelace,analyst\n")
	check("sales_join", map[string]any{"Left": sched, "Right": people, "On": "PersonID = id", "Type": "anti"},
		"PersonID,CourseName,Credits,StartDate\n4,HOMEROOM 10,0,2025-09-05\n5,,0.5,2026-01-20\n")
	check("sales_join", map[string]any{"Left": people, "Right": sched, "On": "id = PersonID", "Type": "semi"},
		"id,first,last,title\n1,ada,lovelace,analyst\n2,alan,turing,fellow\n3,grace,hopper,admiral\n")
	// Totals with no By.
	check("sales_group_aggregate", map[string]any{"Handle": sched, "Aggregates": "count(), count_distinct(PersonID) AS students, sum(Credits), count(CourseName)"},
		"count,students,sum_Credits,count_CourseName\n6,5,2.5,5\n")
}

// A date column compares with the dates a date parameter takes, offsets from
// today included, and a timestamp column compares by day: the student born at
// midnight exactly fifteen years ago is 15 today.
func TestFilterDateColumns(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	bd := handleOf(t, callQueryTool(t, cs, "birthdays", nil))
	ids := func(tool string, args map[string]any) string {
		t.Helper()
		res := callQueryTool(t, cs, tool, args)
		text := contentText(res)
		if res.IsError {
			t.Fatalf("%s %v: %s", tool, args, text)
		}
		var got []string
		for _, line := range strings.Split(text, "\n") {
			if f := strings.SplitN(line, ",", 2); len(f) == 2 && len(f[0]) == 1 && f[0] >= "1" && f[0] <= "3" {
				got = append(got, f[0])
			}
		}
		return strings.Join(got, ",")
	}
	for _, c := range []struct{ where, want string }{
		{"Birthdate <= '-15y'", "1,3"},
		{"Birthdate > '-15y'", "2"},
		{"'-15y' >= Birthdate", "1,3"},
		{"Birthdate BETWEEN '-16y' AND '-15y'", "1,3"},
		{"Birthdate <= " + "'" + addMonthsClamped(stubToday, -15*12).Format("2006-01-02") + "'", "1,3"},
		{"Birthdate < '" + addMonthsClamped(stubToday, -15*12).Format("2006-01-02") + " 00:00:01'", "1,3"},
		{"Seen = 'today'", "1"},
		{"Seen IN ('today', 'yesterday')", "1,2"},
		{"Seen BETWEEN '2025-09-01' AND 'today'", "1,2,3"},
		{"Seen <> '2025-09-03'", "1,2"},
	} {
		if got := ids("sales_filter", map[string]any{"Handle": bd, "Where": c.where}); got != c.want {
			t.Errorf("Where %s: got %q, want %q", c.where, got, c.want)
		}
	}
	got := ids("sales_group_aggregate", map[string]any{"Handle": bd, "By": "PersonID", "Aggregates": "max(Seen) AS last, count_if(Birthdate <= '-15y') AS old", "Having": "last >= 'yesterday'"})
	if got != "1,2" {
		t.Errorf("Having over a date aggregate: got %q, want 1,2", got)
	}
	res := callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": bd, "Where": "Birthdate <= '15 years ago'"})
	if !res.IsError || !strings.Contains(contentText(res), "is not a date") {
		t.Errorf("a non-date against a date column: %s", contentText(res))
	}
}

func TestOperatorErrorsNameTheProblem(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"sales_filter", map[string]any{"Handle": sched, "Where": "Nope = 1"}, `no column "Nope"`},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "PersonID = 1; DROP TABLE x"}, "unexpected"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "PersonID = 1 -- x"}, "unexpected"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "PersonID IN (SELECT 1)"}, "expected a value"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "load_extension('x') = 1"}, "no column"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "CourseName = NULL"}, "IS NULL"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "CourseName = 'x"}, "unterminated"},
		{"sales_group_aggregate", map[string]any{"Handle": sched, "Aggregates": "median(Credits)"}, "unknown aggregate"},
		{"sales_group_aggregate", map[string]any{"Handle": sched, "By": "PersonID", "Aggregates": "count() AS PersonID"}, "appears twice"},
		{"sales_join", map[string]any{"Left": sched, "Right": sched, "On": "PersonID", "Type": "outer"}, "Type must be"},
		{"sales_filter", map[string]any{"Handle": sched, "Where": "PersonID = 1", "Bogus": 1}, "unrecognized parameter"},
		{"sales_filter", map[string]any{"Handle": "zz1", "Where": "PersonID = 1"}, "unknown handle"},
	} {
		res := callQueryTool(t, cs, c.tool, c.args)
		if !res.IsError || !strings.Contains(contentText(res), c.want) {
			t.Errorf("%s %v: %v %s, want an error mentioning %q", c.tool, c.args, res.IsError, contentText(res), c.want)
		}
	}
}

// A join that would multiply past the store's limit is refused before it
// runs.
func TestJoinExplosionGuard(t *testing.T) {
	cs, _ := handleClient(t, handleTools, func(c *config) { c.storeMaxRows = 20 })
	// 20 rows that all share one body: joined to itself on body, 20 × 20.
	many := handleOf(t, callQueryTool(t, cs, "many", map[string]any{"PageSize": 5}))
	res := callQueryTool(t, cs, "sales_join", map[string]any{"Left": many, "Right": many, "On": "body", "AllowPartial": true})
	if !res.IsError || !strings.Contains(contentText(res), "would produce 400 rows") {
		t.Errorf("explosion not refused: %s", contentText(res))
	}
}

func TestCalc(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	scores := handleOf(t, callQueryTool(t, cs, "scores", nil))
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))
	res := callQueryTool(t, cs, "sales_calc", map[string]any{"Expression": "percent(@" + people + ".row_count, @" + scores + ".row_count); @" + scores + ".median(Score); round(@" + scores + ".stdev(Score), 3); @" + scores + ".percentile(Score, 90); (1 + 2) * 3 / 4; days_between('2025-09-01', '2026-06-20')"})
	text := contentText(res)
	for _, want := range []string{
		"= 75\n",    // 3 of 4
		"= 25\n",    // median of 10,20,30,40
		"= 12.91\n", // sample stdev 12.9099…
		"= 37\n",    // 90th percentile, interpolated
		"= 2.25\n",  // arithmetic
		"= 292\n",   // days
	} {
		if !strings.Contains(text, want) {
			t.Errorf("calc output missing %q:\n%s", want, text)
		}
	}
	for _, c := range []struct{ expr, want string }{
		{"1 / 0", "division by zero"},
		{"percent(1, 0)", "division by zero"},
		{"@" + people + ".sum(first)", "not a number"},
		{"@" + people + ".sum(nope)", "no column"},
		{"sqrt(4)", "unknown function"},
		{"1 +", "expected a number"},
	} {
		res := callQueryTool(t, cs, "sales_calc", map[string]any{"Expression": c.expr})
		if !res.IsError || !strings.Contains(contentText(res), c.want) {
			t.Errorf("calc %q: %s, want an error mentioning %q", c.expr, contentText(res), c.want)
		}
	}
}

func TestPercentileAndFormat(t *testing.T) {
	xs := []float64{10, 20, 30, 40}
	if got := percentileOf(xs, 50); got != 25 {
		t.Errorf("median = %v", got)
	}
	if got := percentileOf(xs, 100); got != 40 {
		t.Errorf("p100 = %v", got)
	}
	for v, want := range map[float64]string{0.1 + 0.2: "0.3", 3: "3", -2.5: "-2.5", 1e20: "100000000000000000000"} {
		if got := formatNumber(v); got != want {
			t.Errorf("formatNumber(%v) = %q, want %q", v, got, want)
		}
	}
	if math.IsNaN(percentileOf([]float64{5}, 90)) {
		t.Error("single-value percentile is NaN")
	}
}

func TestCalcSQL(t *testing.T) {
	cs, _ := handleClient(t, handleTools, func(c *config) { c.calcSQL = true })
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))
	res := callQueryTool(t, cs, "sales_calc_sql", map[string]any{"Query": "SELECT StartDate, COUNT(*) AS n FROM @" + sched + " WHERE CourseName <> '@zz1; DROP' GROUP BY StartDate ORDER BY StartDate"})
	if text := contentText(res); res.IsError || !strings.Contains(text, "StartDate,n\n2025-09-03,3\n2025-09-04,1\n2025-09-05,1\n") {
		t.Errorf("calc_sql:\n%s", text)
	}
	for _, c := range []struct{ q, want string }{
		{"DELETE FROM @" + sched, "SELECT or WITH"},
		{"SELECT * FROM @" + sched + "; SELECT 1", "single statement"},
		{"SELECT * FROM sqlite_master", "may not use"},
		{"WITH x AS (SELECT 1) SELECT * FROM x, pragma_table_info('h_1')", "references no stored result"},
		{"SELECT * FROM @" + sched + " JOIN h_1 ON 1=1", "no such table"},
		{"SELECT * FROM @zz9", "unknown handle"},
	} {
		res := callQueryTool(t, cs, "sales_calc_sql", map[string]any{"Query": c.q})
		if !res.IsError || !strings.Contains(contentText(res), c.want) {
			t.Errorf("calc_sql %q: %s, want an error mentioning %q", c.q, contentText(res), c.want)
		}
	}
}

func TestCalcSQLOffByDefault(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	tools, _ := cs.ListTools(context.Background(), nil)
	for _, tool := range tools.Tools {
		if tool.Name == "sales_calc_sql" {
			t.Error("calc_sql is served without --calc-sql")
		}
	}
}

// Stored values keep their meaning in the engine: decimals as numbers,
// dates normalized, bits as 0/1.
func TestSQLiteValueConversion(t *testing.T) {
	for _, c := range []struct {
		v    any
		typ  string
		want any
	}{
		{"19.99", "DECIMAL", 19.99},
		{"2025-09-03T00:00:00Z", "DATE", "2025-09-03"},
		{"2025-09-03T08:15:00Z", "DATETIME2", "2025-09-03 08:15:00"},
		{true, "BIT", int64(1)},
		{"42", "INT", int64(42)},
		{"abc", "NVARCHAR", "abc"},
		{nil, "INT", nil},
	} {
		if got := sqliteValue(c.v, c.typ); got != c.want {
			t.Errorf("sqliteValue(%v, %s) = %#v, want %#v", c.v, c.typ, got, c.want)
		}
	}
}

// When the store drops a result, the engine drops its table.
func TestEvictionDropsEngineTable(t *testing.T) {
	cfg := &config{resultStore: true, storeTTL: time.Hour}
	cfg.ensureRuntime()
	r := sampleResult(3)
	if _, err := cfg.results.put(localCaller, r); err != nil {
		t.Fatal(err)
	}
	if err := withTables(context.Background(), cfg, []*storedResult{r}, func([]string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(cfg.calc.tables) != 1 {
		t.Fatalf("engine holds %d tables, want 1", len(cfg.calc.tables))
	}
	cfg.results.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	cfg.results.get(localCaller, "anything") // sweeps
	if len(cfg.calc.tables) != 0 {
		t.Errorf("engine still holds %d tables after eviction", len(cfg.calc.tables))
	}
}

func TestCompilePredicateBindsEveryLiteral(t *testing.T) {
	sqlText, args, err := compilePredicate("[Course Name] LIKE '%x%' AND id IN (1, -2, 'a''b') OR NOT flag = TRUE", "Where", []string{"Course Name", "id", "flag"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sqlText, "x") || strings.Contains(sqlText, "a'b") {
		t.Errorf("a literal reached the SQL text: %s", sqlText)
	}
	if len(args) != 5 || args[3] != "a'b" || args[1] != int64(1) || args[2] != int64(-2) {
		t.Errorf("args = %#v", args)
	}
	if !strings.Contains(sqlText, `"Course Name" LIKE ?`) {
		t.Errorf("sql = %s", sqlText)
	}
}

// Every operator takes its input as Handle; the set operators' old Handles
// still works, and a call with too few handles says what it got.
func TestSetOperatorHandleParameter(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	a := handleOf(t, callQueryTool(t, cs, "roster", nil))
	b := handleOf(t, callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3,4"}))
	for _, key := range []string{"Handle", "Handles"} {
		res := callQueryTool(t, cs, "sales_set_intersect", map[string]any{key: a + "," + b, "Key": "id", "Carry": false})
		if res.IsError || !strings.Contains(contentText(res), "id\n1\n2\n3\n") {
			t.Errorf("%s: %s", key, contentText(res))
		}
	}
	for want, args := range map[string]map[string]any{
		"got none; pass Handle=qx3,qx5":    {"Key": "id"},
		"got only " + a + "; pass Handle=": {"Handle": a, "Key": "id"},
	} {
		res := callQueryTool(t, cs, "sales_set_union", args)
		if !res.IsError || !strings.Contains(contentText(res), want) {
			t.Errorf("%v: %s, want %q", args, contentText(res), want)
		}
	}
	tools, _ := cs.ListTools(context.Background(), nil)
	for _, tool := range tools.Tools {
		if tool.Name == "sales_set_union" {
			b, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(b), `"Handle"`) || strings.Contains(string(b), `"Handles"`) {
				t.Errorf("set_union schema advertises %s", b)
			}
		}
	}
}

// An intersection carries the first handle's full rows unless told not to, and
// says what a row is.
func TestSetOperatorCarriesByDefault(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))
	four := handleOf(t, callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3,4"}))
	res := callQueryTool(t, cs, "sales_set_intersect", map[string]any{"Handle": people + "," + four, "Key": "id"})
	text := contentText(res)
	if res.IsError || !strings.Contains(text, "id,first,last,title\n1,ada,lovelace,analyst\n") || !strings.Contains(text, "each row is a row of "+people) {
		t.Errorf("default carry:\n%s", text)
	}
	res = callQueryTool(t, cs, "sales_set_intersect", map[string]any{"Handle": people + "," + four, "Key": "id", "Carry": false})
	if text := contentText(res); !strings.Contains(text, "each row is one distinct id") {
		t.Errorf("Carry=false note:\n%s", text)
	}
}

// The same query-tool call twice reuses the first handle.
func TestIdenticalCallReusesHandle(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	first := handleOf(t, callQueryTool(t, cs, "roster", nil))
	res := callQueryTool(t, cs, "roster", nil)
	if again := handleOf(t, res); again != first || !strings.Contains(contentText(res), "same call as "+first) {
		t.Errorf("second call: handle %s, want %s:\n%s", again, first, contentText(res))
	}
}
