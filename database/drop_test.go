package database

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestParseDropAuthorization(t *testing.T) {
	query := func(raw string) url.Values {
		v, err := url.ParseQuery(raw)
		if err != nil {
			t.Fatalf("invalid test query %q: %v", raw, err)
		}
		return v
	}

	tests := []struct {
		name        string
		rawQuery    string
		authorized  bool
		wantIssue   string
		issueAbsent string
	}{
		{
			name:       "missing parameter denies",
			rawQuery:   "sslmode=disable&x-migrations-table=schema_migrations",
			authorized: false,
		},
		{
			name:       "explicit true authorizes",
			rawQuery:   "x-drop-authorized=true",
			authorized: true,
		},
		{
			name:       "explicit true alongside other custom params authorizes",
			rawQuery:   "x-multi-statement=true&x-drop-authorized=true&x-no-tx-wrap=false",
			authorized: true,
		},
		{
			name:       "explicit false denies",
			rawQuery:   "x-drop-authorized=false",
			authorized: false,
		},
		{
			name:       "truthy but non-canonical value denies",
			rawQuery:   "x-drop-authorized=1",
			authorized: false,
			wantIssue:  `invalid value "1" for parameter "x-drop-authorized"`,
		},
		{
			name:       "yes is not a valid value",
			rawQuery:   "x-drop-authorized=yes",
			authorized: false,
			wantIssue:  `invalid value "yes" for parameter "x-drop-authorized"`,
		},
		{
			name:       "TRUE is not the canonical value",
			rawQuery:   "x-drop-authorized=TRUE",
			authorized: false,
			wantIssue:  `invalid value "TRUE" for parameter "x-drop-authorized"`,
		},
		{
			name:       "misspelled parameter is pointed out",
			rawQuery:   "x-drop-authorised=true",
			authorized: false,
			wantIssue:  `unknown parameter "x-drop-authorised" (did you mean "x-drop-authorized"?)`,
		},
		{
			name:       "wrong case parameter is pointed out",
			rawQuery:   "X-Drop-Authorized=true",
			authorized: false,
			wantIssue:  `unknown parameter "X-Drop-Authorized"`,
		},
		{
			name:        "unrelated x- parameters do not match the typo heuristic",
			rawQuery:    "x-no-tx-wrap=true&x-migrations-table=foo",
			authorized:  false,
			issueAbsent: "unknown parameter",
		},
		{
			name:        "native database parameters do not match the typo heuristic",
			rawQuery:    "sslmode=disable&search_path=foo",
			authorized:  false,
			issueAbsent: "unknown parameter",
		},
		{
			name:       "parameter set more than once denies",
			rawQuery:   "x-drop-authorized=true&x-drop-authorized=false",
			authorized: false,
			wantIssue:  `must be set at most once`,
		},
		{
			name:       "canonical true together with a misspelled occurrence still denies",
			rawQuery:   "x-drop-authorized=true&x-drop-authorize=true",
			authorized: false,
			wantIssue:  `unknown parameter "x-drop-authorize"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authz := ParseDropAuthorization(query(tt.rawQuery))
			if authz.Authorized != tt.authorized {
				t.Fatalf("Authorized = %v, want %v (issue: %q)", authz.Authorized, tt.authorized, authz.Issue)
			}
			if tt.wantIssue != "" && !strings.Contains(authz.Issue, tt.wantIssue) {
				t.Fatalf("Issue = %q, want it to contain %q", authz.Issue, tt.wantIssue)
			}
			if tt.issueAbsent != "" && strings.Contains(authz.Issue, tt.issueAbsent) {
				t.Fatalf("Issue = %q, expected it not to contain %q", authz.Issue, tt.issueAbsent)
			}
			if authz.Method == "" {
				t.Fatal("Method should always explain how to grant authorization")
			}
		})
	}
}

func TestParseDropAuthorizationParams(t *testing.T) {
	authz := ParseDropAuthorizationParams(map[string]string{"x-drop-authorized": "true"})
	if !authz.Authorized {
		t.Fatal("expected map variant to authorize on value true")
	}

	denied := ParseDropAuthorizationParams(map[string]string{"x-drop-authorized": "maybe"})
	if denied.Authorized || !strings.Contains(denied.Issue, `invalid value "maybe"`) {
		t.Fatalf("expected invalid value denial, got %+v", denied)
	}
}

func TestDropNotAuthorizedError(t *testing.T) {
	err := DropAuthorization{
		Authorized: false,
		InstanceID: "mydb",
		Method:     `set connection parameter "x-drop-authorized=true"`,
	}.DeniedError()

	var target *DropNotAuthorizedError
	if !errors.As(err, &target) {
		t.Fatal("expected errors.As to succeed for *DropNotAuthorizedError")
	}
	if !errors.Is(err, ErrDropNotAuthorized) {
		t.Fatal("expected errors.Is(err, ErrDropNotAuthorized) to succeed")
	}
	if target.InstanceID != "mydb" {
		t.Fatalf("InstanceID = %q, want %q", target.InstanceID, "mydb")
	}
	if msg := err.Error(); !strings.Contains(msg, "mydb") || !strings.Contains(msg, "x-drop-authorized=true") {
		t.Fatalf("error message %q must carry the instance identifier and the method", msg)
	}
}

func TestRequireDropAuthorization(t *testing.T) {
	if err := RequireDropAuthorization(DropAuthorization{Authorized: true}); err != nil {
		t.Fatalf("authorized conclusion must be allowed, got %v", err)
	}
	if err := RequireDropAuthorization(DropAuthorization{
		InstanceID: "db",
		Method:     "m",
		Issue:      `invalid value "yes" for parameter "x-drop-authorized"`,
	}); err == nil {
		t.Fatal("unauthorized conclusion must be rejected")
	} else if !errors.Is(err, ErrDropNotAuthorized) {
		t.Fatalf("rejection must wrap ErrDropNotAuthorized, got %T %v", err, err)
	}
}
