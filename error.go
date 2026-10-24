package migrate

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/golang-migrate/migrate/v4/source"
)

// Stage is the phase of the migration pipeline in which a failure occurred.
//
// The set of stages is fixed:
//
//   - StageRead: a migration could not be enumerated or read from the
//     [source.Driver] (e.g. First/Next/Prev or ReadUp/ReadDown failed).
//   - StageBuffer: the background goroutine buffering the migration body
//     into memory failed.
//   - StageExecute: the [database.Driver] failed while executing the
//     migration statements.
//   - StageSetVersion: writing the migration version (dirty or clean state)
//     to the database version table failed.
type Stage string

const (
	// StageRead is the failure stage while reading a migration from the source.
	StageRead Stage = "read"

	// StageBuffer is the failure stage while buffering a migration body.
	StageBuffer Stage = "buffer"

	// StageExecute is the failure stage while executing migration statements.
	StageExecute Stage = "execute"

	// StageSetVersion is the failure stage while writing the version table.
	StageSetVersion Stage = "set-version"
)

// MigrationError attributes a migration failure to the version, direction,
// source identifier (usually the file name) and pipeline [Stage] it occurred
// in. The original error is kept on the error chain, so [errors.Is] and
// [errors.As] keep working for sentinel errors (e.g. ErrNoChange, ErrDirty)
// and driver error types (e.g. database.Error).
//
// Use [errors.As] to access the structured fields programmatically:
//
//	var me *migrate.MigrationError
//	if errors.As(err, &me) {
//	    _ = me.Stage     // StageRead, StageBuffer, StageExecute or StageSetVersion
//	    _ = me.Version   // migration version
//	    _ = me.Direction // source.Up or source.Down ("" when it could not be determined)
//	    _ = me.Identifier
//	    _ = me.Statement // 1-based statement index within a multi-statement migration
//	}
//
// Fields may hold zero values when the information is not available at the
// failure point: Version is 0 and Direction is the empty string when a
// migration could not be enumerated from the source, Identifier is empty
// when the migration body could not be opened, and Statement is 0 outside a
// multi-statement execution failure.
type MigrationError struct {
	// Version is the version of the failed migration. It is 0 when the
	// version could not be determined (e.g. the source failed to enumerate
	// the next migration).
	Version uint

	// Direction is the direction of the failed migration, either
	// [source.Up] or [source.Down]. It is the empty string when the
	// direction could not be determined.
	Direction source.Direction

	// Identifier identifies the migration in the source, usually the file
	// name. It is empty when the failure happened before the identifier was
	// known (typically StageRead).
	Identifier string

	// Stage is the pipeline stage in which the failure occurred.
	Stage Stage

	// Statement is the 1-based index of the failing statement within a
	// multi-statement migration. It is 0 when no statement index applies,
	// e.g. for StageRead, StageBuffer or StageSetVersion failures.
	Statement int

	// Err is the underlying error. Inspect it with [errors.Is]/[errors.As].
	Err error
}

// Error implements the error interface. The version, direction, identifier,
// stage and statement are rendered before the underlying error text, whose
// wording is left unchanged.
func (e *MigrationError) Error() string {
	direction := e.Direction
	if direction == "" {
		direction = "unknown"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "migration v%d %s", e.Version, direction)
	if e.Identifier != "" {
		fmt.Fprintf(&b, " %q", e.Identifier)
	}
	b.WriteString(" failed at ")
	b.WriteString(string(e.Stage))
	if e.Statement > 0 {
		fmt.Fprintf(&b, " (statement %d)", e.Statement)
	}
	b.WriteString(": ")
	b.WriteString(e.Err.Error())
	return b.String()
}

// Unwrap exposes the underlying error so that [errors.Is] and [errors.As]
// keep matching sentinel errors and driver error types.
func (e *MigrationError) Unwrap() error {
	return e.Err
}

// wrapReadError attributes err to StageRead with the migration version,
// direction and identifier. Control-flow errors that callers match on
// directly (ErrNoChange, ErrShortLimit and os.ErrNotExist), as well as
// errors already carrying a MigrationError, are returned unchanged.
func wrapReadError(direction source.Direction, version uint, identifier string, err error) error {
	if err == nil {
		return nil
	}

	var me *MigrationError
	if errors.As(err, &me) {
		return err
	}

	if errors.Is(err, ErrNoChange) || errors.Is(err, os.ErrNotExist) {
		return err
	}

	var shortLimit ErrShortLimit
	if errors.As(err, &shortLimit) {
		return err
	}

	return &MigrationError{
		Version:    version,
		Direction:  direction,
		Identifier: identifier,
		Stage:      StageRead,
		Err:        err,
	}
}

// direction returns the migration direction derived from version and
// targetVersion.
func directionFor(version uint, targetVersion int) source.Direction {
	if targetVersion < int(version) {
		return source.Down
	}
	return source.Up
}

// fail builds a MigrationError for this migration at the given stage.
func (m *Migration) fail(stage Stage, err error) *MigrationError {
	return &MigrationError{
		Version:    m.Version,
		Direction:  directionFor(m.Version, m.TargetVersion),
		Identifier: m.Identifier,
		Stage:      stage,
		Err:        err,
	}
}

// executeError builds a StageExecute MigrationError, additionally carrying
// the 1-based index of the failing statement within a multi-statement
// migration. statement is ignored when <= 0.
func (m *Migration) executeError(err error, statement int) *MigrationError {
	me := m.fail(StageExecute, err)
	me.Statement = statement
	return me
}
