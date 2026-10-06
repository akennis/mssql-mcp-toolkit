package main

import "testing"

func TestLeadingKeyword(t *testing.T) {
	cases := map[string]string{
		"SELECT 1":                                        "select",
		"  \n\t select top 5 * from t":                    "select",
		"-- a comment\nSELECT 1":                          "select",
		"/* block\n comment */ WITH c AS()":               "with",
		"/* nested /* inner */ still */ UPDATE t SET a=1": "update",
		"":         "",
		"   -- x ": "",
	}
	for in, want := range cases {
		if got := leadingKeyword(stripNoise(in)); got != want {
			t.Errorf("leadingKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckReadOnly(t *testing.T) {
	allowed := []string{
		"SELECT * FROM dbo.Orders",
		"select 'drop table dbo.Orders' as note",
		"-- delete this later\nSELECT 1",
		"WITH c AS (SELECT 1 AS x) SELECT * FROM c",
		"SELECT [insert] FROM dbo.[update]",
	}
	for _, q := range allowed {
		if err := checkReadOnly(q); err != nil {
			t.Errorf("checkReadOnly(%q) = %v, want nil", q, err)
		}
	}

	rejected := []string{
		"DELETE FROM dbo.Orders",
		"UPDATE dbo.Orders SET total = 0",
		"SELECT * INTO dbo.Copy FROM dbo.Orders",
		"EXEC sp_who",
		"select 1; drop table dbo.Orders",
		"DROP TABLE dbo.Orders",
		"",
	}
	for _, q := range rejected {
		if err := checkReadOnly(q); err == nil {
			t.Errorf("checkReadOnly(%q) = nil, want error", q)
		}
	}
}

func TestExecOnly(t *testing.T) {
	cases := map[string]bool{
		"SELECT 1":                                        false,
		"INSERT INTO dbo.T (a) VALUES (1)":                true,
		"UPDATE dbo.T SET a = 1":                          true,
		"DELETE FROM dbo.T WHERE a = 1":                   true,
		"CREATE TABLE dbo.T (a int)":                      true,
		"EXEC dbo.usp_GetOrders":                          false,
		"DECLARE @x int = 1; SELECT @x":                   false,
		"INSERT INTO dbo.T OUTPUT inserted.id VALUES (1)": false,
		"DELETE FROM dbo.T OUTPUT deleted.id WHERE a = 1": false,
	}
	for in, want := range cases {
		if got := execOnly(in); got != want {
			t.Errorf("execOnly(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStripNoiseKeepsStatementShape(t *testing.T) {
	got := stripNoise("SELECT 'it''s here -- not a comment' FROM [my table] -- trailing\n")
	if containsWord(got, "comment") {
		t.Errorf("string literal leaked into stripped SQL: %q", got)
	}
	if !containsWord(got, "select") || !containsWord(got, "from") {
		t.Errorf("stripped SQL lost its keywords: %q", got)
	}
}

// SQL Server escapes a closing bracket inside a quoted identifier by doubling
// it, and a closing double quote the same way. Stopping at the first one
// leaked the rest of the identifier back into keyword scanning, which rejected
// legitimately-named objects.
func TestStripNoiseHandlesDoubledIdentifierDelimiters(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"escaped bracket", "SELECT * FROM [x]]drop]"},
		{"escaped bracket mid-name", "SELECT * FROM [my]]delete]table]"},
		{"escaped double quote", `SELECT * FROM "x""drop"`},
		{"bracket containing keywords", "SELECT [drop] FROM [insert into]"},
		{"quoted identifier containing keywords", `SELECT "drop" FROM "insert into"`},
	}
	for _, c := range cases {
		code := stripNoise(c.query)
		for _, word := range []string{"drop", "delete", "insert", "into"} {
			if containsWord(code, word) {
				t.Errorf("%s: stripNoise(%q) = %q, leaked %q", c.name, c.query, code, word)
			}
		}
		if !containsWord(code, "select") || !containsWord(code, "from") {
			t.Errorf("%s: stripNoise(%q) = %q, lost the statement shape", c.name, c.query, code)
		}
		if err := checkReadOnly(c.query); err != nil {
			t.Errorf("%s: checkReadOnly(%q) = %v, want nil", c.name, c.query, err)
		}
	}
}

// Escaping must not become a way to smuggle a real keyword past the check: the
// bracket that actually closes the identifier still ends it.
func TestStripNoiseStillCatchesKeywordsAfterAQuotedIdentifier(t *testing.T) {
	rejected := []string{
		"SELECT * FROM [t]; DROP TABLE dbo.Orders",
		"SELECT * FROM [x]]y]; DELETE FROM dbo.Orders",
		`SELECT * FROM "x""y"; DROP TABLE dbo.Orders`,
	}
	for _, q := range rejected {
		if err := checkReadOnly(q); err == nil {
			t.Errorf("checkReadOnly(%q) = nil, want an error", q)
		}
	}
}

// USE and SET stay off the exec path: a batch that sets an option and then
// selects still has a result set to return.
func TestSessionScopedStatementsUseTheQueryPath(t *testing.T) {
	for _, q := range []string{
		"USE OtherDb",
		"SET NOCOUNT ON",
		"SET NOCOUNT ON; SELECT * FROM dbo.Widget",
		"  set ansi_nulls on",
	} {
		if execOnly(q) {
			t.Errorf("execOnly(%q) = true, want false - its result set would be discarded", q)
		}
		if !sessionScoped(q) {
			t.Errorf("sessionScoped(%q) = false, want true", q)
		}
	}

	for _, q := range []string{
		"SELECT 1",
		"INSERT INTO dbo.T (a) VALUES (1)",
		"-- set this up first\nSELECT 1",
	} {
		if sessionScoped(q) {
			t.Errorf("sessionScoped(%q) = true, want false", q)
		}
	}
}

// Read-only mode still rejects them: they are not SELECT or WITH.
func TestCheckReadOnlyRejectsSessionScopedStatements(t *testing.T) {
	for _, q := range []string{"USE OtherDb", "SET NOCOUNT ON"} {
		if err := checkReadOnly(q); err == nil {
			t.Errorf("checkReadOnly(%q) = nil, want an error", q)
		}
	}
}
