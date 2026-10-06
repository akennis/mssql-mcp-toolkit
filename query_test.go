package main

import (
	"reflect"
	"testing"
	"time"
)

func TestConvertValue(t *testing.T) {
	ts := time.Date(2026, 8, 14, 9, 30, 0, 0, time.UTC)
	guid := []byte{0x67, 0x45, 0x23, 0x01, 0xAB, 0x89, 0xEF, 0xCD, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF}

	cases := []struct {
		name   string
		value  any
		dbType string
		want   any
	}{
		{"null", nil, "INT", nil},
		{"int", int64(42), "INT", int64(42)},
		{"datetime", ts, "DATETIME2", "2026-08-14T09:30:00Z"},
		{"decimal keeps precision", []byte("123.456"), "DECIMAL", "123.456"},
		{"varbinary is base64", []byte{0x00, 0x01, 0x02}, "VARBINARY", "AAEC"},
		{"uniqueidentifier", guid, "UNIQUEIDENTIFIER", "01234567-89AB-CDEF-0123-456789ABCDEF"},
		{"text bytes become string", []byte("hello"), "NVARCHAR", "hello"},
	}
	for _, c := range cases {
		got, cut := convertValue(c.value, c.dbType, 0)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: convertValue(%v, %q) = %#v, want %#v", c.name, c.value, c.dbType, got, c.want)
		}
		if cut {
			t.Errorf("%s: convertValue reported a truncation with no cell limit set", c.name)
		}
	}
}

func TestUniqueColumnNames(t *testing.T) {
	got := uniqueColumnNames([]string{"id", "", "id", "name", "id"})
	want := []string{"id", "column_2", "id_2", "name", "id_3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("uniqueColumnNames = %v, want %v", got, want)
	}
}

// A generated suffix can collide with a real column name further along. When
// it does, the colliding column's value is lost from the JSON object, which is
// exactly what this function exists to prevent.
func TestUniqueColumnNamesAvoidsGeneratedCollisions(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"no duplicates", []string{"id", "name"}, []string{"id", "name"}},
		{"plain duplicates", []string{"id", "", "id", "name", "id"}, []string{"id", "column_2", "id_2", "name", "id_3"}},
		{"suffix collides with a real column", []string{"a", "a_2", "a"}, []string{"a", "a_2", "a_3"}},
		{"suffix collides twice over", []string{"a", "a_2", "a_3", "a"}, []string{"a", "a_2", "a_3", "a_4"}},
		{"generated name collides with a real one", []string{"", "column_1"}, []string{"column_1", "column_1_2"}},
		{"real name collides with a later generated one", []string{"column_2", ""}, []string{"column_2", "column_2_2"}},
		{"empty input", nil, []string{}},
	}
	for _, c := range cases {
		got := uniqueColumnNames(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: uniqueColumnNames(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// The property that matters, over inputs chosen to be awkward: the result is
// the same length as the input, has no empty names, and has no repeats.
func TestUniqueColumnNamesAlwaysProducesDistinctNames(t *testing.T) {
	inputs := [][]string{
		{"a", "a", "a", "a"},
		{"a", "a_2", "a", "a_2", "a"},
		{"", "", "", ""},
		{"", "column_1", "column_1", ""},
		{"x_2", "x", "x", "x"},
		{"column_3", "", "", ""},
	}
	for _, in := range inputs {
		got := uniqueColumnNames(in)
		if len(got) != len(in) {
			t.Errorf("uniqueColumnNames(%v) returned %d names, want %d", in, len(got), len(in))
			continue
		}
		seen := make(map[string]bool, len(got))
		for i, name := range got {
			if name == "" {
				t.Errorf("uniqueColumnNames(%v)[%d] is empty", in, i)
			}
			if seen[name] {
				t.Errorf("uniqueColumnNames(%v) = %v repeats %q", in, got, name)
			}
			seen[name] = true
		}
	}
}
