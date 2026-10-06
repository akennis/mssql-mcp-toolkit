package main

import (
	"fmt"
	"strings"
)

// Statements whose interesting output is an affected-row count rather than a
// result set. They are dispatched through ExecContext.
var execKeywords = map[string]bool{
	"insert":   true,
	"update":   true,
	"delete":   true,
	"merge":    true,
	"truncate": true,
	"create":   true,
	"alter":    true,
	"drop":     true,
	"grant":    true,
	"revoke":   true,
	"deny":     true,
	"backup":   true,
	"restore":  true,
}

// Statements whose only effect is on the current session. They deliberately do
// NOT go through ExecContext: a batch such as "SET NOCOUNT ON; SELECT ..."
// still has a result set to return, and routing it to Exec would discard it.
// Their effect is also confined to whichever pooled connection ran them, which
// runQuery reports as a note.
var sessionKeywords = map[string]bool{
	"use": true,
	"set": true,
}

// sessionScoped reports whether the statement leads with a session-scoped
// keyword, whose effect will not survive to the next call over a pooled
// connection.
func sessionScoped(query string) bool {
	return sessionKeywords[leadingKeyword(stripNoise(query))]
}

// Statements allowed when the server runs with --read-only.
var readOnlyKeywords = map[string]bool{
	"select": true,
	"with":   true,
}

// Words that must not appear anywhere in a read-only query. "into" is included
// because SELECT ... INTO creates a table.
var writeWords = map[string]bool{
	"insert": true, "update": true, "delete": true, "merge": true,
	"truncate": true, "create": true, "alter": true, "drop": true,
	"grant": true, "revoke": true, "deny": true, "backup": true,
	"restore": true, "shutdown": true, "reconfigure": true,
	"exec": true, "execute": true, "into": true, "dbcc": true,
}

func isBlank(s string) bool { return strings.TrimSpace(s) == "" }

// withQuery appends the statement that failed to an error, so that a caller
// logging the failure has the SQL alongside it rather than only in its own
// request. The statement is carried verbatim on a line of its own: collapsing
// it onto one would hide everything after a "--" comment, and an audit trail
// that cannot be read back is not worth much.
//
// The wrapped error keeps its place at the front, so callers matching on the
// message ("timed out", "read-only mode") are unaffected.
func withQuery(err error, query string) error {
	return fmt.Errorf("%w\nsql: %s", err, strings.TrimSpace(query))
}

// execOnly reports whether the statement should go through ExecContext. A
// write statement carrying an OUTPUT clause still returns rows, so it stays on
// the query path.
func execOnly(query string) bool {
	code := stripNoise(query)
	if !execKeywords[leadingKeyword(code)] {
		return false
	}
	return !containsWord(code, "output")
}

// checkReadOnly rejects statements that can modify the database. It is a
// guardrail against accidents, not a security boundary: enforce real limits
// with a SQL login that only has read permissions.
func checkReadOnly(query string) error {
	code := stripNoise(query)
	kw := leadingKeyword(code)
	if !readOnlyKeywords[kw] {
		if kw == "" {
			return fmt.Errorf("read-only mode: no statement found in query")
		}
		return fmt.Errorf("read-only mode: %q statements are not allowed, only SELECT and WITH", strings.ToUpper(kw))
	}
	for _, word := range splitWords(code) {
		if writeWords[word] {
			return fmt.Errorf("read-only mode: query contains the disallowed keyword %q", strings.ToUpper(word))
		}
	}
	return nil
}

// leadingKeyword returns the first keyword of already-stripped SQL, lowercased.
func leadingKeyword(code string) string {
	for _, word := range splitWords(code) {
		return word
	}
	return ""
}

func containsWord(code, word string) bool {
	for _, w := range splitWords(code) {
		if w == word {
			return true
		}
	}
	return false
}

func splitWords(code string) []string {
	return strings.FieldsFunc(strings.ToLower(code), func(r rune) bool {
		return !(r == '_' || r == '@' || r == '#' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

// stripNoise removes comments and the contents of string literals and quoted
// identifiers, so that keyword inspection cannot be fooled by text such as
// SELECT 'drop table x'.
func stripNoise(query string) string {
	var out strings.Builder
	out.Grow(len(query))

	runes := []rune(query)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			depth := 1
			i += 2
			for i < len(runes) && depth > 0 {
				if runes[i] == '/' && i+1 < len(runes) && runes[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			i--
			out.WriteByte(' ')
		case c == '\'':
			i++
			for i < len(runes) {
				if runes[i] == '\'' {
					if i+1 < len(runes) && runes[i+1] == '\'' { // escaped quote
						i += 2
						continue
					}
					break
				}
				i++
			}
			out.WriteString(" '' ")
		case c == '"':
			i++
			for i < len(runes) {
				if runes[i] == '"' {
					if i+1 < len(runes) && runes[i+1] == '"' { // escaped quote
						i += 2
						continue
					}
					break
				}
				i++
			}
			out.WriteString(" id ")
		case c == '[':
			i++
			for i < len(runes) {
				if runes[i] == ']' {
					if i+1 < len(runes) && runes[i+1] == ']' { // escaped bracket
						i += 2
						continue
					}
					break
				}
				i++
			}
			out.WriteString(" id ")
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}
