package migrate

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
	dStub "github.com/golang-migrate/migrate/v4/database/stub"
	sStub "github.com/golang-migrate/migrate/v4/source/stub"
)

// lastChanceDriver implements database.Driver but not the optional
// database.DropAuthorizer interface. Migrate must deny Drop by default.
type lastChanceDriver struct{}

func (lastChanceDriver) Open(string) (database.Driver, error) { return lastChanceDriver{}, nil }
func (lastChanceDriver) Close() error                         { return nil }
func (lastChanceDriver) Lock() error                          { return nil }
func (lastChanceDriver) Unlock() error                        { return nil }
func (lastChanceDriver) Run(io.Reader) error                  { return nil }
func (lastChanceDriver) SetVersion(int, bool) error           { return nil }
func (lastChanceDriver) Version() (int, bool, error)          { return 0, false, nil }
func (lastChanceDriver) Drop() error                          { panic("Drop must never be called without authorization") }

func assertDropRejected(t *testing.T, err error) *DropNotAuthorizedError {
	t.Helper()
	if err == nil {
		t.Fatal("expected Drop to be rejected, got nil")
	}
	if !errors.Is(err, ErrDropNotAuthorized) {
		t.Fatalf("expected errors.Is(err, ErrDropNotAuthorized), got %v", err)
	}
	var rejected *DropNotAuthorizedError
	if !errors.As(err, &rejected) {
		t.Fatalf("expected *DropNotAuthorizedError, got %T: %v", err, err)
	}
	return rejected
}

func TestDropRejectedWithoutAuthorization(t *testing.T) {
	m, err := New("stub://", "stub://")
	if err != nil {
		t.Fatal(err)
	}
	dbDrv := m.databaseDrv.(*dStub.Stub)

	err = m.Drop()
	rejected := assertDropRejected(t, err)

	if rejected.InstanceID == "" {
		t.Fatal("rejection must carry the instance identifier")
	}
	if rejected.Method == "" {
		t.Fatal("rejection must explain how to grant authorization")
	}
	if msg := err.Error(); !strings.Contains(msg, "stub://") || !strings.Contains(msg, database.DropAuthorizationParam) {
		t.Fatalf("rejection message %q must carry the instance identifier and the parameter name", msg)
	}
	if len(dbDrv.MigrationSequence) != 0 {
		t.Fatalf("driver must not have dropped anything, sequence: %v", dbDrv.MigrationSequence)
	}
}

func TestDropRejectedBeforeLock(t *testing.T) {
	m, err := New("stub://", "stub://")
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Drop()
	if m.isLocked {
		t.Fatal("the lock must not be taken when Drop is unauthorized")
	}
}

func TestDropPreflight(t *testing.T) {
	m, err := New("stub://", "stub://")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m.DropAuthorized(); ok || err == nil {
		t.Fatalf("expected preflight denial, got ok=%v err=%v", ok, err)
	}

	m2, err := New("stub://", "stub://?"+database.DropAuthorizationParam+"=true")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := m2.DropAuthorized(); !ok || err != nil {
		t.Fatalf("expected preflight authorization, got ok=%v err=%v", ok, err)
	}
}

func TestDropInvalidValueRejectedWithHint(t *testing.T) {
	m, err := New("stub://", "stub://?"+database.DropAuthorizationParam+"=yes")
	if err != nil {
		t.Fatal(err)
	}
	rejected := assertDropRejected(t, m.Drop())
	if !strings.Contains(rejected.Issue, `invalid value "yes"`) {
		t.Fatalf("expected invalid-value hint, got %q", rejected.Issue)
	}
}

func TestDropMisspelledParameterRejectedWithHint(t *testing.T) {
	m, err := New("stub://", "stub://?x-drop-authorised=true")
	if err != nil {
		t.Fatal(err)
	}
	rejected := assertDropRejected(t, m.Drop())
	if !strings.Contains(rejected.Issue, `unknown parameter "x-drop-authorised"`) {
		t.Fatalf("expected misspelled-parameter hint, got %q", rejected.Issue)
	}
}

func TestUnauthorizedDriverDropRejectedDirectly(t *testing.T) {
	d, err := (&dStub.Stub{}).Open("stub://")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Drop(); !errors.Is(err, database.ErrDropNotAuthorized) {
		t.Fatalf("direct driver Drop must be rejected too, got %v", err)
	}
}

func TestMalformedAuthorizationDoesNotBreakOtherCommands(t *testing.T) {
	m, err := New("stub://", "stub://?"+database.DropAuthorizationParam+"=yes")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Version(); err != ErrNilVersion {
		t.Fatalf("expected malformed drop authorization not to affect Version, got %v", err)
	}
}

func TestDropDeniedForDriverWithoutDropAuthorizer(t *testing.T) {
	m, err := NewWithInstance("stub", &sStub.Stub{}, "legacy", lastChanceDriver{})
	if err != nil {
		t.Fatal(err)
	}
	rejected := assertDropRejected(t, m.Drop())
	if !strings.Contains(rejected.Method, "DropAuthorizer") {
		t.Fatalf("expected the rejection to mention the missing interface, got %q", rejected.Method)
	}
}
