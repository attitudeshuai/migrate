package database

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// DropAuthorizationParam is the connection string parameter that explicitly
// authorizes a driver to drop everything in the database instance.
// It is the only parameter name that grants authorization: misspelled names,
// unknown look-alike names and any value other than "true" are treated as
// unauthorized. Like every other "x-" prefixed parameter it is consumed by
// migrate and never forwarded to the underlying database driver.
const DropAuthorizationParam = "x-drop-authorized"

// ErrDropNotAuthorized is returned when Drop is attempted without explicit
// authorization. *DropNotAuthorizedError wraps it, so callers can distinguish a
// rejected Drop from a failed Drop with errors.Is/errors.As.
var ErrDropNotAuthorized = errors.New("drop not authorized")

const (
	urlDropAuthorizationMethod = `set connection parameter "` + DropAuthorizationParam + `=true" to explicitly authorize dropping everything`

	configDropAuthorizationMethod = `set DropAuthorization.Authorized = true in the driver Config to explicitly authorize dropping everything`
)

// DropAuthorization is a driver's conclusion about whether dropping
// everything in the connected database instance is authorized.
// Drivers obtain the conclusion when parsing the connection string or from
// their Config when created via WithInstance, and hand it back to migrate via
// the optional DropAuthorizer interface.
type DropAuthorization struct {
	// Authorized is true only if the instance explicitly authorized Drop.
	// The zero value denies Drop.
	Authorized bool

	// InstanceID identifies the database instance the conclusion applies to,
	// e.g. the database/keyspace name or the connected host.
	InstanceID string

	// Method tells the caller how authorization can be granted.
	Method string

	// Issue optionally reports a malformed authorization parameter, e.g. an
	// invalid value or a misspelled parameter name. It never grants
	// authorization by itself.
	Issue string
}

// DropAuthorizer is the optional interface drivers implement to provide their
// drop authorization conclusion. Drivers that do not implement it are treated
// as unauthorized by default: migrate never calls Drop on an instance it has
// no explicit authorization conclusion for.
type DropAuthorizer interface {
	DropAuthorized() DropAuthorization
}

// DropNotAuthorizedError is returned when Drop was rejected because the
// instance did not explicitly authorize it. It is intentionally distinct from
// Error, which is used for failures while executing Drop.
type DropNotAuthorizedError struct {
	// InstanceID identifies the database instance Drop was rejected for.
	InstanceID string

	// Method describes how the caller can grant authorization.
	Method string

	// Issue optionally points out a malformed authorization parameter.
	Issue string
}

// Error implements the error interface.
func (e *DropNotAuthorizedError) Error() string {
	var b strings.Builder
	b.WriteString(ErrDropNotAuthorized.Error())
	if e.InstanceID != "" {
		fmt.Fprintf(&b, " for database instance %q", e.InstanceID)
	}
	if e.Issue != "" {
		b.WriteString(": ")
		b.WriteString(e.Issue)
	}
	if e.Method != "" {
		if e.Issue != "" {
			b.WriteString("; ")
		} else {
			b.WriteString(": ")
		}
		b.WriteString(e.Method)
	}
	return b.String()
}

// Unwrap exposes ErrDropNotAuthorized for errors.Is.
func (e *DropNotAuthorizedError) Unwrap() error {
	return ErrDropNotAuthorized
}

// DeniedError builds the rejection error for an authorization conclusion that
// does not grant Drop.
func (a DropAuthorization) DeniedError() *DropNotAuthorizedError {
	return &DropNotAuthorizedError{
		InstanceID: a.InstanceID,
		Method:     a.Method,
		Issue:      a.Issue,
	}
}

// RequireDropAuthorization returns nil when the conclusion authorizes Drop,
// and the identifiable *DropNotAuthorizedError otherwise. Drivers call this as
// the first statement of their Drop implementation.
func RequireDropAuthorization(authz DropAuthorization) error {
	if authz.Authorized {
		return nil
	}
	return authz.DeniedError()
}

// NormalizeDropAuthorization fills the instance identifier and the
// authorization method on a conclusion coming from a driver Config
// (WithInstance). Values already present, including conclusions parsed from
// connection strings, are kept.
func NormalizeDropAuthorization(authz DropAuthorization, instanceID string) DropAuthorization {
	if authz.InstanceID == "" {
		authz.InstanceID = instanceID
	}
	if authz.Method == "" {
		authz.Method = configDropAuthorizationMethod
	}
	return authz
}

// ParseDropAuthorization strictly parses the drop authorization parameter from
// connection string query values. It never returns an error: a missing,
// misspelled or invalid parameter simply results in a denied conclusion whose
// Issue field explains what was wrong, so the rest of the connection string
// keeps working for non-drop commands.
func ParseDropAuthorization(query url.Values) DropAuthorization {
	return parseDropAuthorization(query, DropAuthorizationParam)
}

// ParseDropAuthorizationParams is the map[string]string variant for drivers
// whose connection strings are not parsed with net/url (e.g. MySQL DSNs).
func ParseDropAuthorizationParams(params map[string]string) DropAuthorization {
	query := make(url.Values, len(params))
	for k, v := range params {
		query[k] = []string{v}
	}
	return parseDropAuthorization(query, DropAuthorizationParam)
}

func parseDropAuthorization(query url.Values, canonical string) DropAuthorization {
	authz := DropAuthorization{Method: urlDropAuthorizationMethod}

	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var issues []string
	for _, k := range keys {
		switch {
		case k == canonical:
			values := query[k]
			if len(values) > 1 {
				issues = append(issues, fmt.Sprintf(
					"parameter %q must be set at most once", canonical))
				continue
			}
			switch values[0] {
			case "true":
				authz.Authorized = true
			case "false":
				// explicit denial, nothing to report
			default:
				issues = append(issues, fmt.Sprintf(
					"invalid value %q for parameter %q (only %q authorizes dropping everything)",
					values[0], canonical, "true"))
			}
		case looksLikeDropAuthorizationParam(k, canonical):
			issues = append(issues, fmt.Sprintf(
				"unknown parameter %q (did you mean %q?)", k, canonical))
		}
	}

	if len(issues) > 0 {
		// A malformed parameter never authorizes Drop, even if another
		// occurrence of the canonical parameter is present.
		authz.Authorized = false
		authz.Issue = strings.Join(issues, "; ")
	}
	return authz
}

// looksLikeDropAuthorizationParam reports whether key is plausibly a misspelled
// authorization parameter. Only "x-" prefixed parameters are considered so
// native database parameters never trigger the hint.
func looksLikeDropAuthorizationParam(key, canonical string) bool {
	if !strings.HasPrefix(strings.ToLower(key), "x-") {
		return false
	}
	k := strings.ToLower(key)
	c := strings.ToLower(canonical)
	if k == c {
		// differs only in case, the name is case-sensitive
		return true
	}
	if strings.ReplaceAll(k, "_", "-") == c {
		return true
	}
	return levenshtein(k, c) <= 2
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}

	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = minInt(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func minInt(values ...int) int {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
