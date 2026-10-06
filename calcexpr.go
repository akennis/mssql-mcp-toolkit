package main

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// calc's arithmetic: + - * / over numbers, parentheses, a few functions, and
// references to stored results — @qx4.row_count, @qx4.sum(Score),
// @qx4.median(Score), @qx4.percentile(Score, 90). Evaluated here in Go rather
// than by the model, which is the point: a percentage or a median worked out
// in the head of a language model is a guess.

// statFunc computes one statistic of a stored result's column. arg is the
// percentile for percentile(), unused otherwise.
type refResolver func(handle, fn, col string, arg float64, hasArg bool) (float64, error)

type exprEval struct {
	p       *parser
	resolve refResolver
}

// evalExpression evaluates one calc expression.
func evalExpression(src string, resolve refResolver) (float64, error) {
	toks, err := tokenize(src)
	if err != nil {
		return 0, err
	}
	e := &exprEval{p: &parser{toks: toks, what: "Expression"}, resolve: resolve}
	if e.p.peek().kind == tokEOF {
		return 0, errors.New("empty expression")
	}
	v, err := e.sum()
	if err != nil {
		return 0, err
	}
	if err := e.p.done(); err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, errors.New("the result is not a finite number")
	}
	return v, nil
}

func (e *exprEval) sum() (float64, error) {
	v, err := e.product()
	if err != nil {
		return 0, err
	}
	for {
		t := e.p.peek()
		if t.kind != tokOp || (t.text != "+" && t.text != "-") {
			return v, nil
		}
		e.p.i++
		w, err := e.product()
		if err != nil {
			return 0, err
		}
		if t.text == "+" {
			v += w
		} else {
			v -= w
		}
	}
}

func (e *exprEval) product() (float64, error) {
	v, err := e.unary()
	if err != nil {
		return 0, err
	}
	for {
		t := e.p.peek()
		if t.kind != tokOp || (t.text != "*" && t.text != "/" && t.text != "%") {
			return v, nil
		}
		e.p.i++
		w, err := e.unary()
		if err != nil {
			return 0, err
		}
		switch t.text {
		case "*":
			v *= w
		case "/":
			if w == 0 {
				return 0, errors.New("division by zero")
			}
			v /= w
		default:
			if w == 0 {
				return 0, errors.New("modulo by zero")
			}
			v = math.Mod(v, w)
		}
	}
}

func (e *exprEval) unary() (float64, error) {
	if t := e.p.peek(); t.kind == tokOp && (t.text == "-" || t.text == "+") {
		e.p.i++
		v, err := e.unary()
		if t.text == "-" {
			v = -v
		}
		return v, err
	}
	return e.primary()
}

func (e *exprEval) primary() (float64, error) {
	t := e.p.peek()
	switch t.kind {
	case tokNumber:
		e.p.i++
		return strconv.ParseFloat(t.text, 64)
	case tokPunct:
		if t.text == "(" {
			e.p.i++
			v, err := e.sum()
			if err != nil {
				return 0, err
			}
			return v, e.p.expectPunct(")")
		}
	case tokHandle:
		return e.ref()
	case tokIdent:
		return e.call()
	}
	return 0, e.p.errorf("expected a number, a function, a @handle reference or '(', found %s", t)
}

// ref evaluates @qx4.row_count or @qx4.fn(Column[, p]).
func (e *exprEval) ref() (float64, error) {
	h := e.p.next().text
	if err := e.p.expectPunct("."); err != nil {
		return 0, err
	}
	t := e.p.next()
	if t.kind != tokIdent {
		return 0, e.p.errorf("expected row_count or a statistic after @%s., found %s", h, t)
	}
	fn := strings.ToLower(t.text)
	if fn == "row_count" {
		return e.resolve(h, fn, "", 0, false)
	}
	if err := e.p.expectPunct("("); err != nil {
		return 0, err
	}
	ct := e.p.next()
	if ct.kind != tokIdent {
		return 0, e.p.errorf("expected a column name, found %s", ct)
	}
	var arg float64
	hasArg := false
	if e.p.punct(",") {
		e.p.i++
		v, err := e.sum()
		if err != nil {
			return 0, err
		}
		arg, hasArg = v, true
	}
	if err := e.p.expectPunct(")"); err != nil {
		return 0, err
	}
	return e.resolve(h, fn, ct.text, arg, hasArg)
}

// call evaluates a plain function: round, abs, percent, ratio, min, max,
// days_between.
func (e *exprEval) call() (float64, error) {
	name := strings.ToLower(e.p.next().text)
	if err := e.p.expectPunct("("); err != nil {
		return 0, err
	}
	if name == "days_between" {
		var dates [2]time.Time
		for i := range dates {
			if i > 0 {
				if err := e.p.expectPunct(","); err != nil {
					return 0, err
				}
			}
			t := e.p.next()
			if t.kind != tokString {
				return 0, e.p.errorf("days_between takes two quoted dates, found %s", t)
			}
			d, err := parseDateArg(t.text, time.Now())
			if err != nil {
				return 0, err
			}
			dates[i] = d
		}
		if err := e.p.expectPunct(")"); err != nil {
			return 0, err
		}
		return math.Round(dates[1].Sub(dates[0]).Hours() / 24), nil
	}
	var args []float64
	if !e.p.punct(")") {
		for {
			v, err := e.sum()
			if err != nil {
				return 0, err
			}
			args = append(args, v)
			if e.p.punct(",") {
				e.p.i++
				continue
			}
			break
		}
	}
	if err := e.p.expectPunct(")"); err != nil {
		return 0, err
	}
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s takes %d argument(s), got %d", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "round":
		if len(args) == 1 {
			args = append(args, 0)
		}
		if err := need(2); err != nil {
			return 0, err
		}
		scale := math.Pow(10, math.Round(args[1]))
		return math.Round(args[0]*scale) / scale, nil
	case "abs":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Abs(args[0]), nil
	case "percent", "ratio":
		if err := need(2); err != nil {
			return 0, err
		}
		if args[1] == 0 {
			return 0, fmt.Errorf("%s: division by zero", name)
		}
		if name == "percent" {
			return 100 * args[0] / args[1], nil
		}
		return args[0] / args[1], nil
	case "min", "max":
		if len(args) == 0 {
			return 0, fmt.Errorf("%s needs at least one argument", name)
		}
		v := args[0]
		for _, a := range args[1:] {
			if (name == "min" && a < v) || (name == "max" && a > v) {
				v = a
			}
		}
		return v, nil
	}
	return 0, fmt.Errorf("unknown function %q (round, abs, percent, ratio, min, max, days_between; statistics of a stored result are written @handle.fn(Column))", name)
}

// columnStat computes one statistic over a stored result's column. count is
// the non-null values; the numeric statistics skip values that are not
// numbers and say so by failing when none are.
func columnStat(r *storedResult, fn, col string, arg float64, hasArg bool) (float64, error) {
	if fn == "row_count" {
		return float64(len(r.Rows)), nil
	}
	c, ok := r.column(col)
	if !ok {
		return 0, fmt.Errorf("%s has no column %q (columns: %s)", r.ID, col, strings.Join(r.Columns, ", "))
	}
	switch fn {
	case "count":
		n := 0
		for _, row := range r.Rows {
			if row[c] != nil {
				n++
			}
		}
		return float64(n), nil
	case "count_distinct":
		seen := map[string]bool{}
		for _, row := range r.Rows {
			if v := row[c]; v != nil {
				seen[strings.ToLower(scalarString(v))] = true
			}
		}
		return float64(len(seen)), nil
	}
	var xs []float64
	for _, row := range r.Rows {
		v := row[c]
		if v == nil {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(scalarString(v)), 64)
		if err != nil {
			return 0, fmt.Errorf("%s.%s holds %q, which is not a number", r.ID, c, scalarString(v))
		}
		xs = append(xs, f)
	}
	if len(xs) == 0 {
		return 0, fmt.Errorf("%s.%s has no numeric values", r.ID, c)
	}
	sort.Float64s(xs)
	total := 0.0
	for _, x := range xs {
		total += x
	}
	switch fn {
	case "sum":
		return total, nil
	case "avg", "mean":
		return total / float64(len(xs)), nil
	case "min":
		return xs[0], nil
	case "max":
		return xs[len(xs)-1], nil
	case "median":
		return percentileOf(xs, 50), nil
	case "percentile":
		if !hasArg || arg < 0 || arg > 100 {
			return 0, errors.New("percentile takes a column and a percentile from 0 to 100: @h.percentile(Score, 90)")
		}
		return percentileOf(xs, arg), nil
	case "stdev":
		if len(xs) < 2 {
			return 0, fmt.Errorf("%s.%s: a standard deviation needs at least two values", r.ID, c)
		}
		mean := total / float64(len(xs))
		ss := 0.0
		for _, x := range xs {
			ss += (x - mean) * (x - mean)
		}
		return math.Sqrt(ss / float64(len(xs)-1)), nil
	}
	return 0, fmt.Errorf("unknown statistic %q (row_count, count, count_distinct, sum, avg, min, max, median, stdev, percentile)", fn)
}

// percentileOf is the linearly interpolated percentile of sorted xs.
func percentileOf(xs []float64, p float64) float64 {
	if len(xs) == 1 {
		return xs[0]
	}
	rank := p / 100 * float64(len(xs)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	return xs[lo] + (xs[hi]-xs[lo])*(rank-float64(lo))
}

// formatNumber writes a calc result without float noise: whole numbers bare,
// others to at most ten decimal places.
func formatNumber(v float64) string {
	v = math.Round(v*1e10) / 1e10
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}
