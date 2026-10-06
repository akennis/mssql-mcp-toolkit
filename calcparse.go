package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The operators' small languages: a WHERE-style predicate, an aggregate list,
// and plain column lists. Each is parsed here into SQLite text in which every
// column is a quoted identifier resolved against the input's real columns and
// every literal is a bound parameter. Anything outside the grammar is an
// error naming the token that did not fit, so a caller cannot smuggle SQL
// through a filter.

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokString
	tokNumber
	tokOp     // = <> != < <= > >= + - * / %
	tokPunct  // ( ) , .
	tokHandle // @qx4
)

type token struct {
	kind tokKind
	text string // identifier name (unquoted), string value, number text, operator
	pos  int
	// quoted is set for a bracketed or double-quoted identifier, which is
	// never read as a keyword.
	quoted bool
}

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of input"
	case tokString:
		return "'" + t.text + "'"
	case tokHandle:
		return "@" + t.text
	default:
		return fmt.Sprintf("%q", t.text)
	}
}

// tokenize splits s into tokens.
func tokenize(s string) ([]token, error) {
	var out []token
	r := []rune(s)
	for i := 0; i < len(r); {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for {
				if j >= len(r) {
					return nil, fmt.Errorf("unterminated string starting at position %d", i+1)
				}
				if r[j] == '\'' {
					if j+1 < len(r) && r[j+1] == '\'' {
						b.WriteRune('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteRune(r[j])
				j++
			}
			out = append(out, token{kind: tokString, text: b.String(), pos: i})
			i = j + 1
		case c == '[' || c == '"':
			end := ']'
			if c == '"' {
				end = '"'
			}
			j := i + 1
			for j < len(r) && r[j] != end {
				j++
			}
			if j >= len(r) {
				return nil, fmt.Errorf("unterminated column name starting at position %d", i+1)
			}
			out = append(out, token{kind: tokIdent, text: string(r[i+1 : j]), pos: i, quoted: true})
			i = j + 1
		case c == '@':
			j := i + 1
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j])) {
				j++
			}
			if j == i+1 {
				return nil, fmt.Errorf("@ at position %d is not followed by a handle", i+1)
			}
			out = append(out, token{kind: tokHandle, text: strings.ToLower(string(r[i+1 : j])), pos: i})
			i = j
		case unicode.IsDigit(c) || (c == '.' && i+1 < len(r) && unicode.IsDigit(r[i+1])):
			j := i
			for j < len(r) && (unicode.IsDigit(r[j]) || r[j] == '.') {
				j++
			}
			if _, err := strconv.ParseFloat(string(r[i:j]), 64); err != nil {
				return nil, fmt.Errorf("%q at position %d is not a number", string(r[i:j]), i+1)
			}
			out = append(out, token{kind: tokNumber, text: string(r[i:j]), pos: i})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || r[j] == '_') {
				j++
			}
			out = append(out, token{kind: tokIdent, text: string(r[i:j]), pos: i})
			i = j
		case c == '<' || c == '>' || c == '!':
			if i+1 < len(r) && (r[i+1] == '=' || (c == '<' && r[i+1] == '>')) {
				out = append(out, token{kind: tokOp, text: string(r[i : i+2]), pos: i})
				i += 2
				continue
			}
			if c == '!' {
				return nil, fmt.Errorf("unexpected '!' at position %d", i+1)
			}
			out = append(out, token{kind: tokOp, text: string(c), pos: i})
			i++
		case strings.ContainsRune("=+-*/%", c):
			out = append(out, token{kind: tokOp, text: string(c), pos: i})
			i++
		case strings.ContainsRune("(),.", c):
			out = append(out, token{kind: tokPunct, text: string(c), pos: i})
			i++
		default:
			return nil, fmt.Errorf("unexpected %q at position %d", string(c), i+1)
		}
	}
	return append(out, token{kind: tokEOF, pos: len(r)}), nil
}

// parser walks a token list, compiling to SQLite text with bound arguments.
type parser struct {
	toks  []token
	i     int
	cols  []string          // the input's columns, for resolving names
	types map[string]string // SQL Server type of each column, where known
	args  []any
	what  string    // what is being parsed, for error messages
	now   time.Time // what today and -15y in a date comparison count from
}

func newParser(src, what string, cols []string, types map[string]string) (*parser, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &parser{toks: toks, cols: cols, types: types, what: what, now: time.Now()}, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

// keyword reports whether the next token is the bare word kw.
func (p *parser) keyword(kw string) bool {
	t := p.peek()
	return t.kind == tokIdent && !t.quoted && strings.EqualFold(t.text, kw)
}

func (p *parser) acceptKeyword(kw string) bool {
	if p.keyword(kw) {
		p.i++
		return true
	}
	return false
}

func (p *parser) punct(s string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.text == s
}

func (p *parser) expectPunct(s string) error {
	if !p.punct(s) {
		return p.errorf("expected %q, found %s", s, p.peek())
	}
	p.i++
	return nil
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%s: %s (at position %d)", p.what, fmt.Sprintf(format, args...), p.peek().pos+1)
}

func (p *parser) done() error {
	if p.peek().kind != tokEOF {
		return p.errorf("unexpected %s", p.peek())
	}
	return nil
}

// reserved words cannot stand as bare column names; bracket a column that
// happens to be called one.
var reservedWords = map[string]bool{
	"and": true, "or": true, "not": true, "in": true, "like": true, "is": true, "null": true,
	"between": true, "true": true, "false": true, "as": true, "asc": true, "desc": true,
}

// column reads a column name and resolves it against p.cols.
func (p *parser) column() (string, error) {
	t := p.peek()
	if t.kind != tokIdent || (!t.quoted && reservedWords[strings.ToLower(t.text)]) {
		return "", p.errorf("expected a column name, found %s", t)
	}
	p.i++
	for _, c := range p.cols {
		if strings.EqualFold(c, t.text) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s: no column %q (columns: %s)", p.what, t.text, strings.Join(p.cols, ", "))
}

// alias reads an output column name after AS.
func (p *parser) alias() (string, error) {
	t := p.peek()
	if t.kind != tokIdent || (!t.quoted && reservedWords[strings.ToLower(t.text)]) {
		return "", p.errorf("expected a name after AS, found %s", t)
	}
	p.i++
	return t.text, nil
}

// --- predicates ---

// predicate compiles a WHERE-style condition.
func (p *parser) predicate() (string, error) { return p.orExpr() }

func (p *parser) orExpr() (string, error) {
	left, err := p.andExpr()
	if err != nil {
		return "", err
	}
	for p.acceptKeyword("OR") {
		right, err := p.andExpr()
		if err != nil {
			return "", err
		}
		left = "(" + left + " OR " + right + ")"
	}
	return left, nil
}

func (p *parser) andExpr() (string, error) {
	left, err := p.notExpr()
	if err != nil {
		return "", err
	}
	for p.acceptKeyword("AND") {
		right, err := p.notExpr()
		if err != nil {
			return "", err
		}
		left = "(" + left + " AND " + right + ")"
	}
	return left, nil
}

func (p *parser) notExpr() (string, error) {
	if p.acceptKeyword("NOT") {
		inner, err := p.notExpr()
		if err != nil {
			return "", err
		}
		return "(NOT " + inner + ")", nil
	}
	return p.comparison()
}

func (p *parser) comparison() (string, error) {
	if p.punct("(") {
		p.i++
		inner, err := p.orExpr()
		if err != nil {
			return "", err
		}
		if err := p.expectPunct(")"); err != nil {
			return "", err
		}
		return "(" + inner + ")", nil
	}
	lt, err := p.operand()
	if err != nil {
		return "", err
	}
	left := lt.sql
	negate := p.acceptKeyword("NOT")
	switch {
	case p.acceptKeyword("IN"):
		if err := p.expectPunct("("); err != nil {
			return "", err
		}
		var items []term
		for {
			v, err := p.literalTerm()
			if err != nil {
				return "", err
			}
			items = append(items, v)
			if p.punct(",") {
				p.i++
				continue
			}
			break
		}
		if err := p.expectPunct(")"); err != nil {
			return "", err
		}
		if left, err = p.dateCompare(lt, items...); err != nil {
			return "", err
		}
		sqls := make([]string, len(items))
		for i, it := range items {
			sqls[i] = it.sql
		}
		op := " IN "
		if negate {
			op = " NOT IN "
		}
		return "(" + left + op + "(" + strings.Join(sqls, ", ") + "))", nil
	case p.acceptKeyword("LIKE"):
		if p.peek().kind != tokString {
			return "", p.errorf("LIKE needs a quoted pattern, found %s", p.peek())
		}
		p.args = append(p.args, p.next().text)
		op := " LIKE "
		if negate {
			op = " NOT LIKE "
		}
		return "(" + left + op + "?)", nil
	case p.acceptKeyword("BETWEEN"):
		lo, err := p.operand()
		if err != nil {
			return "", err
		}
		if !p.acceptKeyword("AND") {
			return "", p.errorf("BETWEEN needs AND, found %s", p.peek())
		}
		hi, err := p.operand()
		if err != nil {
			return "", err
		}
		if left, err = p.dateCompare(lt, lo, hi); err != nil {
			return "", err
		}
		op := " BETWEEN "
		if negate {
			op = " NOT BETWEEN "
		}
		return "(" + left + op + lo.sql + " AND " + hi.sql + ")", nil
	case negate:
		return "", p.errorf("NOT here must be followed by IN, LIKE or BETWEEN, found %s", p.peek())
	case p.acceptKeyword("IS"):
		not := p.acceptKeyword("NOT")
		if !p.acceptKeyword("NULL") {
			return "", p.errorf("IS must be followed by NULL or NOT NULL, found %s", p.peek())
		}
		if not {
			return "(" + left + " IS NOT NULL)", nil
		}
		return "(" + left + " IS NULL)", nil
	}
	t := p.peek()
	if t.kind != tokOp {
		return "", p.errorf("expected a comparison (=, <>, <, <=, >, >=, IN, LIKE, BETWEEN, IS NULL), found %s", t)
	}
	op := t.text
	switch op {
	case "=", "<>", "!=", "<", "<=", ">", ">=":
	default:
		return "", p.errorf("expected a comparison operator, found %s", t)
	}
	p.i++
	if p.keyword("NULL") {
		return "", p.errorf("compare with NULL using IS NULL or IS NOT NULL")
	}
	rt, err := p.operand()
	if err != nil {
		return "", err
	}
	if op == "!=" {
		op = "<>"
	}
	right := rt.sql
	if left, err = p.dateCompare(lt, rt); err != nil {
		return "", err
	}
	if right, err = p.dateCompare(rt, lt); err != nil {
		return "", err
	}
	return "(" + left + " " + op + " " + right + ")", nil
}

// term is a compiled operand: a column, or a literal bound as a parameter.
type term struct {
	sql string
	col string // the column, when the term is one
	arg int    // the p.args index of a quoted string literal, else -1
}

// operand is a column or a literal.
func (p *parser) operand() (term, error) {
	t := p.peek()
	if t.kind == tokIdent && (t.quoted || !reservedWords[strings.ToLower(t.text)]) {
		c, err := p.column()
		if err != nil {
			return term{}, err
		}
		return term{sql: quoteIdent(c), col: c, arg: -1}, nil
	}
	return p.literalTerm()
}

func (p *parser) literalTerm() (term, error) {
	str := p.peek().kind == tokString
	s, err := p.literal()
	if err != nil {
		return term{}, err
	}
	t := term{sql: s, arg: -1}
	if str {
		t.arg = len(p.args) - 1
	}
	return t, nil
}

// dateCompare fits the quoted literals compared with a date column to the
// column, and returns the column's SQL for the comparison. A literal is a
// date the way a date parameter is - yyyy-mm-dd, or an offset from today
// such as -15y - or a full timestamp, and is rewritten to the form the store
// keeps the column in, so text order is time order. When every literal is a
// plain day, a timestamp column is cut to its day first: without that,
// Birthdate <= '2011-09-24' would leave out 2011-09-24 00:00:00, which sorts
// after it as text. Anything else is not a column-and-literal comparison and
// comes back as it was.
func (p *parser) dateCompare(col term, lits ...term) (string, error) {
	typ := ""
	if col.col != "" {
		typ = p.types[col.col]
	}
	if !isDateType(typ) {
		return col.sql, nil
	}
	days := true
	resolved := make([]string, len(lits))
	for i, l := range lits {
		if l.arg < 0 {
			continue
		}
		text, _ := p.args[l.arg].(string)
		if d, err := parseDateArg(text, p.now); err == nil {
			resolved[i] = d.Format("2006-01-02")
			continue
		}
		if _, ok := parseTimestamp(text); !ok {
			return "", fmt.Errorf("%s: %q is not a date to compare %s with: use yyyy-mm-dd, a timestamp, today, yesterday, tomorrow, or an offset from today like -7d, -2w, -3m, -15y", p.what, text, col.col)
		}
		resolved[i] = text
		days = false
	}
	touched := false
	for i, l := range lits {
		if l.arg < 0 {
			continue
		}
		touched = true
		if days {
			p.args[l.arg] = resolved[i]
		} else {
			p.args[l.arg] = normalizeDate(resolved[i], typ)
		}
	}
	if touched && days && typ != "DATE" {
		return "date(" + col.sql + ")", nil
	}
	return col.sql, nil
}

// literal is a string, a number (optionally negative), TRUE or FALSE, bound as
// a parameter.
func (p *parser) literal() (string, error) {
	t := p.peek()
	switch {
	case t.kind == tokString:
		p.i++
		p.args = append(p.args, t.text)
		return "?", nil
	case t.kind == tokNumber || (t.kind == tokOp && t.text == "-" && p.toks[p.i+1].kind == tokNumber):
		neg := false
		if t.kind == tokOp {
			neg = true
			p.i++
			t = p.peek()
		}
		p.i++
		text := t.text
		if neg {
			text = "-" + text
		}
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			p.args = append(p.args, n)
		} else {
			f, _ := strconv.ParseFloat(text, 64)
			p.args = append(p.args, f)
		}
		return "?", nil
	case p.keyword("TRUE"):
		p.i++
		p.args = append(p.args, int64(1))
		return "?", nil
	case p.keyword("FALSE"):
		p.i++
		p.args = append(p.args, int64(0))
		return "?", nil
	case p.keyword("NULL"):
		return "", p.errorf("NULL is not a value to compare with; use IS NULL")
	}
	return "", p.errorf("expected a value (a quoted string, a number, TRUE or FALSE), found %s", t)
}

// compilePredicate parses src as a condition over cols. types, which may be
// nil, gives the SQL Server type of each column it knows; a date column
// compares with dates the way a date parameter takes them.
func compilePredicate(src, what string, cols []string, types map[string]string) (string, []any, error) {
	p, err := newParser(src, what, cols, types)
	if err != nil {
		return "", nil, err
	}
	if p.peek().kind == tokEOF {
		return "", nil, fmt.Errorf("%s is empty", what)
	}
	sqlText, err := p.predicate()
	if err != nil {
		return "", nil, err
	}
	if err := p.done(); err != nil {
		return "", nil, err
	}
	return sqlText, p.args, nil
}

// --- lists ---

// selectItem is one output column: its SQL and its name.
type selectItem struct {
	sql, name string
}

// compileColumnList parses "A, B AS C" (aliases only when allowAlias).
func compileColumnList(src, what string, cols []string, allowAlias bool) ([]selectItem, error) {
	p, err := newParser(src, what, cols, nil)
	if err != nil {
		return nil, err
	}
	var items []selectItem
	for {
		c, err := p.column()
		if err != nil {
			return nil, err
		}
		item := selectItem{sql: quoteIdent(c), name: c}
		if allowAlias && p.acceptKeyword("AS") {
			if item.name, err = p.alias(); err != nil {
				return nil, err
			}
		}
		items = append(items, item)
		if p.punct(",") {
			p.i++
			continue
		}
		break
	}
	if err := p.done(); err != nil {
		return nil, fmt.Errorf("%w (%s takes column names only, each optionally AS a new name; it cannot compute or join values - present the columns as they are)", err, what)
	}
	return items, checkUniqueNames(items, what)
}

// orderItem is one sort key.
type orderItem struct {
	col  string
	desc bool
}

// compileOrderList parses "A DESC, B".
func compileOrderList(src, what string, cols []string) ([]orderItem, error) {
	p, err := newParser(src, what, cols, nil)
	if err != nil {
		return nil, err
	}
	var items []orderItem
	for {
		c, err := p.column()
		if err != nil {
			return nil, err
		}
		item := orderItem{col: c}
		if p.acceptKeyword("DESC") {
			item.desc = true
		} else {
			p.acceptKeyword("ASC")
		}
		items = append(items, item)
		if p.punct(",") {
			p.i++
			continue
		}
		break
	}
	return items, p.done()
}

// joinPair is one join condition: a left column equal to a right column.
type joinPair struct{ left, right string }

// compileJoinOn parses "PersonID" (same name both sides) or
// "PersonID = StudentPersonID", comma-separated.
func compileJoinOn(src string, left, right []string) ([]joinPair, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, fmt.Errorf("On: %w", err)
	}
	lp := &parser{toks: toks, cols: left, what: "On"}
	var pairs []joinPair
	for {
		// Read the left name against the left columns, then the right one
		// against the right columns.
		l, err := lp.column()
		if err != nil {
			return nil, err
		}
		r := l
		if t := lp.peek(); t.kind == tokOp && t.text == "=" {
			lp.i++
			rp := &parser{toks: lp.toks, i: lp.i, cols: right, what: "On"}
			if r, err = rp.column(); err != nil {
				return nil, err
			}
			lp.i = rp.i
		} else {
			found := false
			for _, c := range right {
				if strings.EqualFold(c, l) {
					r, found = c, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("On: the right-hand result has no column %q; write Left = Right when the names differ (right columns: %s)", l, strings.Join(right, ", "))
			}
		}
		pairs = append(pairs, joinPair{left: l, right: r})
		if lp.punct(",") {
			lp.i++
			continue
		}
		break
	}
	return pairs, lp.done()
}

// aggItem is one aggregate in a group_aggregate call.
type aggItem struct {
	sql, name string
	typ       string // the SQL Server type min and max carry over from their column
}

// compileAggregates parses "count() AS n, count_distinct(PersonID) AS
// students, count_if(CourseName LIKE '%Homeroom%') AS homerooms".
func compileAggregates(src string, cols []string, types map[string]string) ([]aggItem, []any, error) {
	p, err := newParser(src, "Aggregates", cols, types)
	if err != nil {
		return nil, nil, err
	}
	var items []aggItem
	for n := 1; ; n++ {
		t := p.peek()
		if t.kind != tokIdent || t.quoted {
			return nil, nil, p.errorf("expected an aggregate (count, count_distinct, sum, avg, min, max, count_if, sum_if), found %s", t)
		}
		fn := strings.ToLower(t.text)
		p.i++
		if err := p.expectPunct("("); err != nil {
			return nil, nil, err
		}
		if (fn == "sum_if" || fn == "sum" || fn == "avg" || fn == "min" || fn == "max") && p.looksLikeCondition() {
			return nil, nil, p.errorf("%s takes a column name first%s; to count rows that match a condition use count_if(condition), e.g. count_if(Score >= 90) AS A", fn, map[bool]string{true: ", then a comma and the condition: sum_if(Col, condition)", false: ""}[fn == "sum_if"])
		}
		var item aggItem
		switch fn {
		case "count":
			if p.punct(")") {
				item = aggItem{sql: "COUNT(*)", name: "count"}
			} else if t := p.peek(); t.kind == tokOp && t.text == "*" {
				p.i++
				item = aggItem{sql: "COUNT(*)", name: "count"}
			} else {
				c, err := p.column()
				if err != nil {
					return nil, nil, err
				}
				item = aggItem{sql: "COUNT(" + quoteIdent(c) + ")", name: "count_" + c}
			}
		case "count_distinct":
			c, err := p.column()
			if err != nil {
				return nil, nil, err
			}
			item = aggItem{sql: "COUNT(DISTINCT " + quoteIdent(c) + ")", name: "distinct_" + c}
		case "sum", "avg", "min", "max":
			c, err := p.column()
			if err != nil {
				return nil, nil, err
			}
			item = aggItem{sql: strings.ToUpper(fn) + "(" + quoteIdent(c) + ")", name: fn + "_" + c}
			if fn == "min" || fn == "max" {
				item.typ = types[c]
			}
		case "count_if":
			cond, err := p.predicate()
			if err != nil {
				return nil, nil, err
			}
			item = aggItem{sql: "COALESCE(SUM(CASE WHEN " + cond + " THEN 1 ELSE 0 END), 0)", name: fmt.Sprintf("count_if_%d", n)}
		case "sum_if":
			c, err := p.column()
			if err != nil {
				return nil, nil, err
			}
			if err := p.expectPunct(","); err != nil {
				return nil, nil, err
			}
			cond, err := p.predicate()
			if err != nil {
				return nil, nil, err
			}
			item = aggItem{sql: "COALESCE(SUM(CASE WHEN " + cond + " THEN " + quoteIdent(c) + " END), 0)", name: "sum_if_" + c}
		default:
			return nil, nil, fmt.Errorf("Aggregates: unknown aggregate %q (count, count_distinct, sum, avg, min, max, count_if, sum_if)", t.text)
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, nil, err
		}
		if p.acceptKeyword("AS") {
			if item.name, err = p.alias(); err != nil {
				return nil, nil, err
			}
		}
		items = append(items, item)
		if p.punct(",") {
			p.i++
			continue
		}
		break
	}
	if err := p.done(); err != nil {
		return nil, nil, err
	}
	names := make([]selectItem, len(items))
	for i, it := range items {
		names[i] = selectItem{name: it.name}
	}
	return items, p.args, checkUniqueNames(names, "Aggregates")
}

func checkUniqueNames(items []selectItem, what string) error {
	seen := map[string]bool{}
	for _, it := range items {
		k := strings.ToLower(it.name)
		if seen[k] {
			return fmt.Errorf("%s: the output column %q appears twice; give one of them a different name with AS", what, it.name)
		}
		seen[k] = true
	}
	return nil
}

// looksLikeCondition reports whether the tokens after an aggregate's opening
// parenthesis are a comparison (Col >= 90) rather than a bare column: the
// mistake of writing sum_if(Score >= 90, 1) for count_if(Score >= 90).
func (p *parser) looksLikeCondition() bool {
	if p.i+1 >= len(p.toks) {
		return false
	}
	t := p.toks[p.i+1]
	return t.kind == tokOp && t.text != "*"
}
