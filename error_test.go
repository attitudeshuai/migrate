package migrate

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/multistmt"
	dStub "github.com/golang-migrate/migrate/v4/database/stub"
	"github.com/golang-migrate/migrate/v4/source"
	sStub "github.com/golang-migrate/migrate/v4/source/stub"
)

var (
	errExecuteSentinel    = errors.New("execute boom")
	errBufferSentinel     = errors.New("buffer boom")
	errReadUpSentinel     = errors.New("read up boom")
	errSetVersionSentinel = errors.New("set version boom")
)

// multiStmtDB wraps the stub driver and mimics a multi-statement capable
// database driver: statements are parsed and executed sequentially and a
// failure on a given statement is reported as database.Error.
type multiStmtDB struct {
	*dStub.Stub

	// failAtStatement makes Run fail when the 1-based statement is reached.
	failAtStatement int

	failSetVersion bool
	// failCleanOnly limits SetVersion failures to the clean-state write.
	failCleanOnly bool

	setVersionCalls int
}

func (d *multiStmtDB) Run(migration io.Reader) error {
	statement := 0
	var runErr error
	if err := multistmt.Parse(migration, []byte(";"), 10*1024*1024, func(stmt []byte) bool {
		statement++
		if d.failAtStatement > 0 && statement == d.failAtStatement {
			runErr = database.Error{
				OrigErr: errExecuteSentinel,
				Err:     "migration failed",
				Query:   append([]byte(nil), stmt...),
				Line:    1,
			}
			return false
		}
		d.MigrationSequence = append(d.MigrationSequence, string(stmt))
		return true
	}); err != nil {
		return err
	}
	return runErr
}

func (d *multiStmtDB) SetVersion(version int, dirty bool) error {
	d.setVersionCalls++
	if d.failSetVersion && (!d.failCleanOnly || !dirty) {
		return errSetVersionSentinel
	}
	return d.Stub.SetVersion(version, dirty)
}

// bodySource wraps the stub source and serves configured migration bodies
// and identifiers.
type bodySource struct {
	*sStub.Stub

	upBody   string
	upID     string
	downBody string
	downID   string
}

func (s *bodySource) ReadUp(version uint) (io.ReadCloser, string, error) {
	if s.upBody != "" {
		if _, ok := s.Migrations.Up(version); ok {
			return io.NopCloser(strings.NewReader(s.upBody)), s.upID, nil
		}
	}
	return s.Stub.ReadUp(version)
}

func (s *bodySource) ReadDown(version uint) (io.ReadCloser, string, error) {
	if s.downBody != "" {
		if _, ok := s.Migrations.Down(version); ok {
			return io.NopCloser(strings.NewReader(s.downBody)), s.downID, nil
		}
	}
	return s.Stub.ReadDown(version)
}

// failingReadSource fails ReadUp no matter what.
type failingReadSource struct {
	*sStub.Stub
}

func (s *failingReadSource) ReadUp(uint) (io.ReadCloser, string, error) {
	return nil, "", errReadUpSentinel
}

// errReadCloser fails every Read.
type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errBufferSentinel }
func (errReadCloser) Close() error             { return nil }

func newBodyMigrations() *source.Migrations {
	migs := source.NewMigrations()
	migs.Append(&source.Migration{Version: 1, Direction: source.Up, Identifier: "CREATE 1"})
	migs.Append(&source.Migration{Version: 1, Direction: source.Down, Identifier: "DROP 1"})
	return migs
}

func TestMigrationErrorExecuteMultiStatementUp(t *testing.T) {
	m, _ := New("stub://", "stub://")
	stubDB := m.databaseDrv.(*dStub.Stub)
	m.databaseDrv = &multiStmtDB{Stub: stubDB, failAtStatement: 2}

	src := m.sourceDrv.(*sStub.Stub)
	src.Migrations = newBodyMigrations()
	m.sourceDrv = &bodySource{
		Stub:   src,
		upBody: "CREATE a; CREATE BAD; CREATE c;",
		upID:   "1_multi.up.sql",
	}

	err := m.Steps(1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MigrationError, got %T: %v", err, err)
	}

	if me.Stage != StageExecute {
		t.Errorf("expected stage %q, got %q", StageExecute, me.Stage)
	}
	if me.Version != 1 {
		t.Errorf("expected version 1, got %v", me.Version)
	}
	if me.Direction != source.Up {
		t.Errorf("expected direction up, got %q", me.Direction)
	}
	if me.Identifier != "1_multi.up.sql" {
		t.Errorf("expected identifier 1_multi.up.sql, got %q", me.Identifier)
	}
	if me.Statement != 2 {
		t.Errorf("expected failing statement 2, got %v", me.Statement)
	}

	// driver error type stays reachable via errors.As
	var dbErr database.Error
	if !errors.As(err, &dbErr) {
		t.Fatal("expected errors.As to find database.Error")
	}
	if dbErr.Line != 1 {
		t.Errorf("expected driver line 1 preserved, got %v", dbErr.Line)
	}

	// sentinel error stays reachable through the driver error type; note
	// database.Error does not implement Unwrap, so its OrigErr is reached
	// via the field, as with unwrapped driver errors.
	if !errors.Is(dbErr.OrigErr, errExecuteSentinel) {
		t.Errorf("expected database.Error.OrigErr to be %v, got %v", errExecuteSentinel, dbErr.OrigErr)
	}

	// existing text is unchanged, field information is rendered ahead of it
	if !strings.HasSuffix(err.Error(), dbErr.Error()) {
		t.Errorf("expected underlying error text %q to be preserved, got %q", dbErr.Error(), err.Error())
	}
	for _, want := range []string{"migration v1 up", `"1_multi.up.sql"`, "failed at execute", "statement 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error text %q to contain %q", err.Error(), want)
		}
	}
	if strings.Index(err.Error(), "migration v1") > strings.Index(err.Error(), dbErr.Error()) {
		t.Error("expected migration context to be rendered before underlying error text")
	}
}

func TestMigrationErrorExecuteDown(t *testing.T) {
	m, _ := New("stub://", "stub://")
	stubDB := m.databaseDrv.(*dStub.Stub)
	if err := stubDB.SetVersion(1, false); err != nil {
		t.Fatal(err)
	}
	m.databaseDrv = &multiStmtDB{Stub: stubDB, failAtStatement: 2}

	src := m.sourceDrv.(*sStub.Stub)
	src.Migrations = newBodyMigrations()
	m.sourceDrv = &bodySource{
		Stub:     src,
		downBody: "DROP a; DROP BAD;",
		downID:   "1_multi.down.sql",
	}

	err := m.Steps(-1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MigrationError, got %T: %v", err, err)
	}

	if me.Stage != StageExecute {
		t.Errorf("expected stage %q, got %q", StageExecute, me.Stage)
	}
	if me.Version != 1 {
		t.Errorf("expected version 1, got %v", me.Version)
	}
	if me.Direction != source.Down {
		t.Errorf("expected direction down, got %q", me.Direction)
	}
	if me.Identifier != "1_multi.down.sql" {
		t.Errorf("expected identifier 1_multi.down.sql, got %q", me.Identifier)
	}
	if me.Statement != 2 {
		t.Errorf("expected failing statement 2, got %v", me.Statement)
	}
}

func TestMigrationErrorBuffer(t *testing.T) {
	m, _ := New("stub://", "stub://")

	migr, err := NewMigration(io.NopCloser(errReadCloser{}), "1_bad.up.sql", 1, 2)
	if err != nil {
		t.Fatal(err)
	}

	err = m.Run(migr)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MigrationError, got %T: %v", err, err)
	}

	if me.Stage != StageBuffer {
		t.Errorf("expected stage %q, got %q", StageBuffer, me.Stage)
	}
	if me.Version != 1 {
		t.Errorf("expected version 1, got %v", me.Version)
	}
	if me.Direction != source.Up {
		t.Errorf("expected direction up, got %q", me.Direction)
	}
	if me.Identifier != "1_bad.up.sql" {
		t.Errorf("expected identifier 1_bad.up.sql, got %q", me.Identifier)
	}
	if me.Statement != 0 {
		t.Errorf("expected no statement index for buffer failure, got %v", me.Statement)
	}
	if !errors.Is(err, errBufferSentinel) {
		t.Errorf("expected errors.Is to match %v, got %v", errBufferSentinel, err)
	}
}

func TestMigrationErrorRead(t *testing.T) {
	m, _ := New("stub://", "stub://")

	src := m.sourceDrv.(*sStub.Stub)
	src.Migrations = newBodyMigrations()
	m.sourceDrv = &failingReadSource{Stub: src}

	err := m.Steps(1)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var me *MigrationError
	if !errors.As(err, &me) {
		t.Fatalf("expected *MigrationError, got %T: %v", err, err)
	}

	if me.Stage != StageRead {
		t.Errorf("expected stage %q, got %q", StageRead, me.Stage)
	}
	if me.Version != 1 {
		t.Errorf("expected version 1, got %v", me.Version)
	}
	if me.Direction != source.Up {
		t.Errorf("expected direction up, got %q", me.Direction)
	}
	if me.Identifier != "" {
		t.Errorf("expected empty identifier for read failure, got %q", me.Identifier)
	}
	if me.Statement != 0 {
		t.Errorf("expected no statement index for read failure, got %v", me.Statement)
	}
	if !errors.Is(err, errReadUpSentinel) {
		t.Errorf("expected errors.Is to match %v, got %v", errReadUpSentinel, err)
	}
}

func TestMigrationErrorSetVersion(t *testing.T) {
	t.Run("dirty write", func(t *testing.T) {
		m, _ := New("stub://", "stub://")
		stubDB := m.databaseDrv.(*dStub.Stub)
		m.databaseDrv = &multiStmtDB{Stub: stubDB, failSetVersion: true}

		src := m.sourceDrv.(*sStub.Stub)
		src.Migrations = newBodyMigrations()

		err := m.Steps(1)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		var me *MigrationError
		if !errors.As(err, &me) || me.Stage != StageSetVersion {
			t.Fatalf("expected StageSetVersion MigrationError, got %T: %v", err, err)
		}
		if me.Version != 1 {
			t.Errorf("expected version 1, got %v", me.Version)
		}
		if !errors.Is(err, errSetVersionSentinel) {
			t.Errorf("expected errors.Is to match %v, got %v", errSetVersionSentinel, err)
		}
	})

	t.Run("clean write", func(t *testing.T) {
		m, _ := New("stub://", "stub://")
		stubDB := m.databaseDrv.(*dStub.Stub)
		db := &multiStmtDB{Stub: stubDB, failSetVersion: true, failCleanOnly: true}
		m.databaseDrv = db

		src := m.sourceDrv.(*sStub.Stub)
		src.Migrations = newBodyMigrations()

		err := m.Steps(1)
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		var me *MigrationError
		if !errors.As(err, &me) || me.Stage != StageSetVersion {
			t.Fatalf("expected StageSetVersion MigrationError, got %T: %v", err, err)
		}
		if db.setVersionCalls != 2 {
			t.Errorf("expected failure on the clean write (call 2), got %v calls", db.setVersionCalls)
		}
	})
}

func TestSentinelErrorsUnchanged(t *testing.T) {
	m, _ := New("stub://", "stub://")

	// ErrNoChange must stay identical, callers compare it directly.
	if err := m.Steps(0); err != ErrNoChange {
		t.Errorf("expected ErrNoChange identical, got %v", err)
	}

	// missing migrations stay matched with errors.Is and must not be wrapped.
	if err := m.Steps(1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist via errors.Is, got %v", err)
	} else {
		var me *MigrationError
		if errors.As(err, &me) {
			t.Errorf("expected control-flow error not to be wrapped, got %v", me)
		}
	}

	// ErrDirty stays the concrete value type callers assert on.
	dbDrv := m.databaseDrv.(*dStub.Stub)
	if err := dbDrv.SetVersion(0, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil {
		if _, ok := err.(ErrDirty); !ok {
			t.Errorf("expected ErrDirty, got %T: %v", err, err)
		}
	} else {
		t.Error("expected ErrDirty error")
	}
}

func TestStatementReader(t *testing.T) {
	t.Run("delivered count", func(t *testing.T) {
		r := newStatementReader(strings.NewReader("CREATE a; CREATE b; CREATE c;"))
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "CREATE a; CREATE b; CREATE c;" {
			t.Errorf("reader changed migration text: %q", body)
		}
		if got := r.statementsDelivered(); got != 3 {
			t.Errorf("expected 3 statements delivered, got %v", got)
		}
	})

	t.Run("small read buffer and trailing statement", func(t *testing.T) {
		r := newStatementReader(strings.NewReader("ab;cde;f"))
		var b strings.Builder
		buf := make([]byte, 2)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if b.String() != "ab;cde;f" {
			t.Errorf("expected unchanged text, got %q", b.String())
		}
		if got := r.statementsDelivered(); got != 3 {
			t.Errorf("expected 3 statements delivered, got %v", got)
		}
	})
}
