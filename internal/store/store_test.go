package store

import (
	"context"
	"strings"
	"testing"
)

func TestSplitSQLStatements(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty input",
			in:   "",
			want: []string{},
		},
		{
			name: "whitespace only",
			in:   "   \n\t  ",
			want: []string{},
		},
		{
			name: "single statement without trailing semicolon",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "trailing semicolon does not yield an empty statement",
			in:   "SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "multiple statements are trimmed and split",
			in:   "CREATE TABLE t (id int);\n\nSELECT 1;\n",
			want: []string{"CREATE TABLE t (id int)", "SELECT 1"},
		},
		{
			name: "blank statements between semicolons are dropped",
			in:   "SELECT 1;;\n  ;SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSQLStatements(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d statements %q, want %d %q", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("statement %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNew_InvalidDSNReturnsError(t *testing.T) {
	// A malformed DSN fails at config parse time, before any connection is made,
	// so this exercises New's error-wrapping path without needing a database.
	_, err := New(context.Background(), "://not-a-valid-dsn")
	if err == nil {
		t.Fatal("expected error for invalid DSN, got nil")
	}
	if !strings.Contains(err.Error(), "create pgx pool") {
		t.Errorf("error %q should mention pool creation", err.Error())
	}
}
