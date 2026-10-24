// Package migrate reads migrations from sources and runs them against databases.
// Sources are defined by the `source.Driver` and databases by the `database.Driver`
// interface. The driver interfaces are kept "dumb", all migration logic is kept
// in this package.
package migrate

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	iurl "github.com/golang-migrate/migrate/v4/internal/url"
	"github.com/golang-migrate/migrate/v4/source"
)

// DefaultPrefetchMigrations sets the number of migrations to pre-read
// from the source. This is helpful if the source is remote, but has little
// effect for a local source (i.e. file system).
// Please note that this setting has a major impact on the memory usage,
// since each pre-read migration is buffered in memory. See DefaultBufferSize.
var DefaultPrefetchMigrations = uint(10)

// DefaultLockTimeout sets the max time a database driver has to acquire a lock.
var DefaultLockTimeout = 15 * time.Second

var (
	ErrNoChange       = errors.New("no change")
	ErrNilVersion     = errors.New("no migration")
	ErrInvalidVersion = errors.New("version must be >= -1")
	ErrLocked         = errors.New("database locked")
	ErrLockTimeout    = errors.New("timeout: can't acquire database lock")

	// ErrStop is returned when an execution was stopped due to a value
	// sent on GracefulStop. It is a sentinel error; use errors.Is to
	// distinguish a stopped execution from a successful one and the
	// returned error may also be an ErrStopped carrying version state.
	ErrStop = errors.New("migration stopped")
)

// ErrShortLimit is an error returned when not enough migrations
// can be returned by a source for a given limit.
type ErrShortLimit struct {
	Short uint
}

// Error implements the error interface.
func (e ErrShortLimit) Error() string {
	return fmt.Sprintf("limit %v short", e.Short)
}

type ErrDirty struct {
	Version int
}

func (e ErrDirty) Error() string {
	return fmt.Sprintf("Dirty database version %v. Fix and force version.", e.Version)
}

// ErrStopped is returned by an execution that ended because of a graceful
// stop request. It records the version table state observed at the time the
// stop took effect, so callers can tell which version the database stopped
// at and whether it was left dirty.
//
// ErrStopped wraps ErrStop, so errors.Is(err, ErrStop) matches both the
// plain sentinel (e.g. a stop during the lock wait or a rejected reuse) and
// an ErrStopped.
type ErrStopped struct {
	// Version is the version table version at the time of the stop.
	// database.NilVersion (-1) means no migration had been applied.
	Version int

	// Dirty is the dirty flag of the version table at the time of the stop.
	Dirty bool
}

func (e ErrStopped) Error() string {
	switch {
	case e.Dirty:
		return fmt.Sprintf("migration stopped at version %v (dirty): inspect the database and force version %v to continue", e.Version, e.Version)
	case e.Version == database.NilVersion:
		return "migration stopped before any migration was applied"
	default:
		return fmt.Sprintf("migration stopped at version %v (clean)", e.Version)
	}
}

// Unwrap returns ErrStop so that errors.Is with ErrStop succeeds.
func (e ErrStopped) Unwrap() error {
	return ErrStop
}

type Migrate struct {
	sourceName         string
	sourceDrv          source.Driver
	databaseDriverName string
	databaseDrv        database.Driver

	// Log accepts a Logger interface
	Log Logger

	// GracefulStop accepts `true` and will stop executing migrations
	// as soon as possible at a safe break point, so that the database
	// is not corrupted.
	// The channel type and the way a stop is requested (sending `true`)
	// are kept stable for library callers. After a stop took effect, the
	// instance is unusable for further executions until ResetGracefulStop
	// is called; see ErrStop.
	GracefulStop chan bool

	// stopMu guards isGracefulStop.
	stopMu     sync.Mutex
	isLockedMu *sync.Mutex

	isGracefulStop bool
	isLocked       bool

	// PrefetchMigrations defaults to DefaultPrefetchMigrations,
	// but can be set per Migrate instance.
	PrefetchMigrations uint

	// LockTimeout defaults to DefaultLockTimeout,
	// but can be set per Migrate instance.
	LockTimeout time.Duration
}

// New returns a new Migrate instance from a source URL and a database URL.
// The URL scheme is defined by each driver.
func New(sourceURL, databaseURL string) (*Migrate, error) {
	m := newCommon()

	sourceName, err := iurl.SchemeFromURL(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse scheme from source URL: %w", err)
	}
	m.sourceName = sourceName

	databaseDriverName, err := iurl.SchemeFromURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse scheme from database URL: %w", err)
	}
	m.databaseDriverName = databaseDriverName

	sourceDrv, err := source.Open(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open source, %q: %w", sourceURL, err)
	}
	m.sourceDrv = sourceDrv

	databaseDrv, err := database.Open(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	m.databaseDrv = databaseDrv

	return m, nil
}

// NewWithDatabaseInstance returns a new Migrate instance from a source URL
// and an existing database instance. The source URL scheme is defined by each driver.
// Use any string that can serve as an identifier during logging as databaseDriverName.
// You are responsible for closing the underlying database client if necessary.
func NewWithDatabaseInstance(sourceURL string, databaseDriverName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	sourceName, err := iurl.SchemeFromURL(sourceURL)
	if err != nil {
		return nil, err
	}
	m.sourceName = sourceName

	m.databaseDriverName = databaseDriverName

	sourceDrv, err := source.Open(sourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open source, %q: %w", sourceURL, err)
	}
	m.sourceDrv = sourceDrv

	m.databaseDrv = databaseInstance

	return m, nil
}

// NewWithSourceInstance returns a new Migrate instance from an existing source instance
// and a database URL. The database URL scheme is defined by each driver.
// Use any string that can serve as an identifier during logging as sourceName.
// You are responsible for closing the underlying source client if necessary.
func NewWithSourceInstance(sourceName string, sourceInstance source.Driver, databaseURL string) (*Migrate, error) {
	m := newCommon()

	databaseDriverName, err := iurl.SchemeFromURL(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse scheme from database URL: %w", err)
	}
	m.databaseDriverName = databaseDriverName

	m.sourceName = sourceName

	databaseDrv, err := database.Open(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	m.databaseDrv = databaseDrv

	m.sourceDrv = sourceInstance

	return m, nil
}

// NewWithInstance returns a new Migrate instance from an existing source and
// database instance. Use any string that can serve as an identifier during logging
// as sourceName and databaseDriverName. You are responsible for closing down
// the underlying source and database client if necessary.
func NewWithInstance(sourceName string, sourceInstance source.Driver, databaseDriverName string, databaseInstance database.Driver) (*Migrate, error) {
	m := newCommon()

	m.sourceName = sourceName
	m.databaseDriverName = databaseDriverName

	m.sourceDrv = sourceInstance
	m.databaseDrv = databaseInstance

	return m, nil
}

func newCommon() *Migrate {
	return &Migrate{
		GracefulStop:       make(chan bool, 1),
		PrefetchMigrations: DefaultPrefetchMigrations,
		LockTimeout:        DefaultLockTimeout,
		isLockedMu:         &sync.Mutex{},
	}
}

// Close closes the source and the database.
func (m *Migrate) Close() (source error, database error) {
	databaseSrvClose := make(chan error)
	sourceSrvClose := make(chan error)

	m.logVerbosePrintf("Closing source and database\n")

	go func() {
		databaseSrvClose <- m.databaseDrv.Close()
	}()

	go func() {
		sourceSrvClose <- m.sourceDrv.Close()
	}()

	return <-sourceSrvClose, <-databaseSrvClose
}

// Migrate looks at the currently active migration version,
// then migrates either up or down to the specified version.
func (m *Migrate) Migrate(version uint) error {
	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.read(curVersion, int(version), ret)

	return m.unlockErr(m.runMigrations(ret))
}

// Steps looks at the currently active migration version.
// It will migrate up if n > 0, and down if n < 0.
func (m *Migrate) Steps(n int) error {
	if n == 0 {
		return ErrNoChange
	}

	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	if n > 0 {
		go m.readUp(curVersion, n, ret)
	} else {
		go m.readDown(curVersion, -n, ret)
	}

	return m.unlockErr(m.runMigrations(ret))
}

// Up looks at the currently active migration version
// and will migrate all the way up (applying all up migrations).
func (m *Migrate) Up() error {
	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	go m.readUp(curVersion, -1, ret)
	return m.unlockErr(m.runMigrations(ret))
}

// Down looks at the currently active migration version
// and will migrate all the way down (applying all down migrations).
func (m *Migrate) Down() error {
	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)
	go m.readDown(curVersion, -1, ret)
	return m.unlockErr(m.runMigrations(ret))
}

// Drop deletes everything in the database.
func (m *Migrate) Drop() error {
	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}
	if err := m.databaseDrv.Drop(); err != nil {
		return m.unlockErr(err)
	}
	return m.unlock()
}

// Run runs any migration provided by you against the database.
// It does not check any currently active version in database.
// Usually you don't need this function at all. Use Migrate,
// Steps, Up or Down instead.
func (m *Migrate) Run(migration ...*Migration) error {
	if len(migration) == 0 {
		return ErrNoChange
	}

	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	curVersion, dirty, err := m.databaseDrv.Version()
	if err != nil {
		return m.unlockErr(err)
	}

	if dirty {
		return m.unlockErr(ErrDirty{curVersion})
	}

	ret := make(chan interface{}, m.PrefetchMigrations)

	go func() {
		defer close(ret)
		for _, migr := range migration {
			if m.PrefetchMigrations > 0 && migr.Body != nil {
				m.logVerbosePrintf("Start buffering %v\n", migr.LogString())
			} else {
				m.logVerbosePrintf("Scheduled %v\n", migr.LogString())
			}

			ret <- migr
			go func(migr *Migration) {
				if err := migr.Buffer(); err != nil {
					m.logErr(err)
				}
			}(migr)
		}
	}()

	return m.unlockErr(m.runMigrations(ret))
}

// Force sets a migration version.
// It does not check any currently active version in database.
// It resets the dirty state to false.
func (m *Migrate) Force(version int) error {
	if version < -1 {
		return ErrInvalidVersion
	}

	if err := m.checkNotStopped(); err != nil {
		return err
	}

	if err := m.lockForRun(); err != nil {
		return err
	}

	if err := m.databaseDrv.SetVersion(version, false); err != nil {
		return m.unlockErr(err)
	}

	return m.unlock()
}

// Version returns the currently active migration version.
// If no migration has been applied, yet, it will return ErrNilVersion.
func (m *Migrate) Version() (version uint, dirty bool, err error) {
	v, d, err := m.databaseDrv.Version()
	if err != nil {
		return 0, false, err
	}

	if v == database.NilVersion {
		return 0, false, ErrNilVersion
	}

	return suint(v), d, nil
}

// read reads either up or down migrations from source `from` to `to`.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once read is done reading it will close the ret channel.
func (m *Migrate) read(from int, to int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if err := m.versionExists(suint(from)); err != nil {
			ret <- err
			return
		}
	}

	// check if to version exists
	if to >= 0 {
		if err := m.versionExists(suint(to)); err != nil {
			ret <- err
			return
		}
	}

	// no change?
	if from == to {
		ret <- ErrNoChange
		return
	}

	if from < to {
		// it's going up
		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go func() {
				if err := migr.Buffer(); err != nil {
					m.logErr(err)
				}
			}()

			from = int(firstVersion)
		}

		// run until we reach target ...
		for from < to {
			if m.stop() {
				return
			}

			next, err := m.sourceDrv.Next(suint(from))
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(next, int(next))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go func() {
				if err := migr.Buffer(); err != nil {
					m.logErr(err)
				}
			}()

			from = int(next)
		}

	} else {
		// it's going down
		// run until we reach target ...
		for from > to && from >= 0 {
			if m.stop() {
				return
			}

			prev, err := m.sourceDrv.Prev(suint(from))
			if errors.Is(err, os.ErrNotExist) && to == -1 {
				// apply nil migration
				migr, err := m.newMigration(suint(from), -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go func() {
					if err := migr.Buffer(); err != nil {
						m.logErr(err)
					}
				}()

				return

			} else if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(suint(from), int(prev))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go func() {
				if err := migr.Buffer(); err != nil {
					m.logErr(err)
				}
			}()

			from = int(prev)
		}
	}
}

// readUp reads up migrations from `from` limited by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readUp is done reading it will close the ret channel.
func (m *Migrate) readUp(from int, limit int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if err := m.versionExists(suint(from)); err != nil {
			ret <- err
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		// apply first migration if from is nil version
		if from == -1 {
			firstVersion, err := m.sourceDrv.First()
			if err != nil {
				ret <- err
				return
			}

			migr, err := m.newMigration(firstVersion, int(firstVersion))
			if err != nil {
				ret <- err
				return
			}

			ret <- migr
			go func() {
				if err := migr.Buffer(); err != nil {
					m.logErr(err)
				}
			}()
			from = int(firstVersion)
			count++
			continue
		}

		// apply next migration
		next, err := m.sourceDrv.Next(suint(from))
		if errors.Is(err, os.ErrNotExist) {
			// no limit, but no migrations applied?
			if limit == -1 && count == 0 {
				ret <- ErrNoChange
				return
			}

			// no limit, reached end
			if limit == -1 {
				return
			}

			// reached end, and didn't apply any migrations
			if limit > 0 && count == 0 {
				ret <- os.ErrNotExist
				return
			}

			// applied less migrations than limit?
			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
				return
			}
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(next, int(next))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go func() {
			if err := migr.Buffer(); err != nil {
				m.logErr(err)
			}
		}()
		from = int(next)
		count++
	}
}

// readDown reads down migrations from `from` limited by `limit`.
// limit can be -1, implying no limit and reading until there are no more migrations.
// Each migration is then written to the ret channel.
// If an error occurs during reading, that error is written to the ret channel, too.
// Once readDown is done reading it will close the ret channel.
func (m *Migrate) readDown(from int, limit int, ret chan<- interface{}) {
	defer close(ret)

	// check if from version exists
	if from >= 0 {
		if err := m.versionExists(suint(from)); err != nil {
			ret <- err
			return
		}
	}

	if limit == 0 {
		ret <- ErrNoChange
		return
	}

	// no change if already at nil version
	if from == -1 && limit == -1 {
		ret <- ErrNoChange
		return
	}

	// can't go over limit if already at nil version
	if from == -1 && limit > 0 {
		ret <- os.ErrNotExist
		return
	}

	count := 0
	for count < limit || limit == -1 {
		if m.stop() {
			return
		}

		prev, err := m.sourceDrv.Prev(suint(from))
		if errors.Is(err, os.ErrNotExist) {
			// no limit or haven't reached limit, apply "first" migration
			if limit == -1 || limit-count > 0 {
				firstVersion, err := m.sourceDrv.First()
				if err != nil {
					ret <- err
					return
				}

				migr, err := m.newMigration(firstVersion, -1)
				if err != nil {
					ret <- err
					return
				}
				ret <- migr
				go func() {
					if err := migr.Buffer(); err != nil {
						m.logErr(err)
					}
				}()
				count++
			}

			if count < limit {
				ret <- ErrShortLimit{suint(limit - count)}
			}
			return
		}
		if err != nil {
			ret <- err
			return
		}

		migr, err := m.newMigration(suint(from), int(prev))
		if err != nil {
			ret <- err
			return
		}

		ret <- migr
		go func() {
			if err := migr.Buffer(); err != nil {
				m.logErr(err)
			}
		}()
		from = int(prev)
		count++
	}
}

// runMigrations reads *Migration and error from a channel. Any other type
// sent on this channel will result in a panic. Each migration is then
// proxied to the database driver and run against the database.
// Before running a newly received migration it will check if it's supposed
// to stop execution because it might have received a stop signal on the
// GracefulStop channel.
//
// A stopped run returns an error wrapping ErrStop (usually an ErrStopped
// with the current version table state) instead of nil, so that callers
// can tell a stopped execution apart from a successful one.
func (m *Migrate) runMigrations(ret <-chan interface{}) error {
	for r := range ret {

		if m.stop() {
			return m.stoppedErr()
		}

		switch r := r.(type) {
		case error:
			return r

		case *Migration:
			migr := r

			// set version with dirty state
			if err := m.databaseDrv.SetVersion(migr.TargetVersion, true); err != nil {
				return err
			}

			if migr.Body != nil {
				m.logVerbosePrintf("Read and execute %v\n", migr.LogString())
				if err := m.databaseDrv.Run(migr.BufferedBody); err != nil {
					return err
				}
			}

			// If a stop was requested while the statement was in
			// flight, take effect here at the safe point but keep the
			// version table dirty at the migration's target version.
			// The dirty marker (set before Run) is the accounting the
			// database already has; the report points it out and the
			// operator must inspect/force before migrating again.
			if m.stop() {
				return m.stoppedErr()
			}

			// set clean state
			if err := m.databaseDrv.SetVersion(migr.TargetVersion, false); err != nil {
				return err
			}

			endTime := time.Now()
			readTime := migr.FinishedReading.Sub(migr.StartedBuffering)
			runTime := endTime.Sub(migr.FinishedReading)

			// log either verbose or normal
			if m.Log != nil {
				if m.Log.Verbose() {
					m.logPrintf("Finished %v (read %v, ran %v)\n", migr.LogString(), readTime, runTime)
				} else {
					m.logPrintf("%v (%v)\n", migr.LogString(), readTime+runTime)
				}
			}

		default:
			return fmt.Errorf("unknown type: %T with value: %+v", r, r)
		}
	}

	// The channel might have been closed by the reader because it saw the
	// stop request before another migration could be delivered.
	if m.stop() {
		return m.stoppedErr()
	}

	return nil
}

// versionExists checks the source if either the up or down migration for
// the specified migration version exists.
func (m *Migrate) versionExists(version uint) (result error) {
	// try up migration first
	up, _, err := m.sourceDrv.ReadUp(version)
	if err == nil {
		defer func() {
			if errClose := up.Close(); errClose != nil {
				result = errors.Join(result, errClose)
			}
		}()
	}
	if errors.Is(err, os.ErrExist) {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// then try down migration
	down, _, err := m.sourceDrv.ReadDown(version)
	if err == nil {
		defer func() {
			if errClose := down.Close(); errClose != nil {
				result = errors.Join(result, errClose)
			}
		}()
	}
	if errors.Is(err, os.ErrExist) {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	err = fmt.Errorf("no migration found for version %d: %w", version, err)
	m.logErr(err)
	return err
}

// stop returns true if no more migrations should be run against the database
// because a stop signal was received on the GracefulStop channel.
// Calls are cheap and this function is not blocking.
func (m *Migrate) stop() bool {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()

	if m.isGracefulStop {
		return true
	}

	select {
	case <-m.GracefulStop:
		m.isGracefulStop = true
		return true

	default:
		return false
	}
}

// setGracefulStop records that a graceful stop took effect without
// draining another value from the GracefulStop channel. It is used by
// the stop watcher in lock which already received the channel value.
func (m *Migrate) setGracefulStop() {
	m.stopMu.Lock()
	m.isGracefulStop = true
	m.stopMu.Unlock()
}

// ResetGracefulStop clears the graceful stop state so that the instance
// can be reused for another execution. It also drains a stop value that
// may still be buffered on the GracefulStop channel.
//
// Executions started after a stop took effect and before
// ResetGracefulStop is called fail immediately with an error wrapping
// ErrStop instead of silently doing nothing. After the reset the
// instance behaves like a freshly created one; the GracefulStop channel
// itself is not replaced, so existing references to it keep working.
func (m *Migrate) ResetGracefulStop() {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()

	m.isGracefulStop = false

	for {
		select {
		case <-m.GracefulStop:
			// drain stale stop values
		default:
			return
		}
	}
}

// checkNotStopped fails fast if a previous graceful stop is still in
// effect, i.e. the instance has not been reset for reuse.
func (m *Migrate) checkNotStopped() error {
	if m.stop() {
		return fmt.Errorf("migrate instance is stopped after a graceful stop request; call ResetGracefulStop before running migrations again: %w", ErrStop)
	}
	return nil
}

// lockForRun locks the database like lock does, but maps a stop observed
// during the lock wait to the stopped conclusion. No migration or version
// table write happens in that case.
func (m *Migrate) lockForRun() error {
	if err := m.lock(); err != nil {
		if errors.Is(err, ErrStop) {
			return m.stoppedErr()
		}
		return err
	}
	return nil
}

// stoppedErr builds the distinguishable conclusion for a stopped
// execution: it reads the current version table state, logs a report
// (stopped version, dirty flag and next steps) and returns an
// ErrStopped wrapping ErrStop. If the version table cannot be read,
// the read error is joined with ErrStop and still matches errors.Is.
func (m *Migrate) stoppedErr() error {
	version, dirty, err := m.databaseDrv.Version()
	if err != nil {
		m.logErr(fmt.Errorf("failed to read version after stop: %w", err))
		return errors.Join(ErrStop, fmt.Errorf("read version table after stop: %w", err))
	}

	m.logStopReport(version, dirty)
	return ErrStopped{Version: version, Dirty: dirty}
}

// logStopReport reports the version table state at a stop and explains
// how to continue.
func (m *Migrate) logStopReport(version int, dirty bool) {
	switch {
	case dirty:
		m.logPrintf("Migration stopped at version %v; the database is dirty. The migration to version %v was interrupted before it could be marked clean.\n", version, version)
		m.logPrintf("Next step: inspect the database state, then run 'migrate force %v' once it is safe to clear the dirty flag, and re-run migrate.\n", version)

	case version == database.NilVersion:
		m.logPrintf("Migration stopped before any migration was applied; the version table holds no version and is not dirty.\n")
		m.logPrintf("Next step: re-run migrate to start over.\n")

	default:
		m.logPrintf("Migration stopped at version %v; the database is clean and no partial migration was left behind.\n", version)
		m.logPrintf("Next step: re-run migrate to continue from version %v.\n", version)
	}
}

// newMigration is a helper func that returns a *Migration for the
// specified version and targetVersion.
func (m *Migrate) newMigration(version uint, targetVersion int) (*Migration, error) {
	var migr *Migration

	if targetVersion >= int(version) {
		r, identifier, err := m.sourceDrv.ReadUp(version)
		if errors.Is(err, os.ErrNotExist) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from up source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}

	} else {
		r, identifier, err := m.sourceDrv.ReadDown(version)
		if errors.Is(err, os.ErrNotExist) {
			// create "empty" migration
			migr, err = NewMigration(nil, "", version, targetVersion)
			if err != nil {
				return nil, err
			}

		} else if err != nil {
			return nil, err

		} else {
			// create migration from down source
			migr, err = NewMigration(r, identifier, version, targetVersion)
			if err != nil {
				return nil, err
			}
		}
	}

	if m.PrefetchMigrations > 0 && migr.Body != nil {
		m.logVerbosePrintf("Start buffering %v\n", migr.LogString())
	} else {
		m.logVerbosePrintf("Scheduled %v\n", migr.LogString())
	}

	return migr, nil
}

// lock is a thread safe helper function to lock the database.
// It should be called as late as possible when running migrations.
// A graceful stop observed while waiting for the lock makes lock return
// ErrStop without acquiring the lock; the version table is not touched.
func (m *Migrate) lock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if m.isLocked {
		return ErrLocked
	}

	// create done channel, used in the timeout goroutine
	done := make(chan bool, 1)
	defer func() {
		done <- true
	}()

	// use errchan to signal error back to this context.
	// Capacity covers the lock, timeout and stop senders so that none
	// of them blocks if lock returns before all results are in.
	errchan := make(chan error, 3)

	// start timeout goroutine
	timeout := time.After(m.LockTimeout)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-timeout:
				errchan <- ErrLockTimeout
				return
			}
		}
	}()

	// start graceful stop watcher goroutine
	go func() {
		select {
		case <-done:
			return
		case <-m.GracefulStop:
			m.setGracefulStop()
			errchan <- ErrStop
		}
	}()

	// now try to acquire the lock
	go func() {
		if err := m.databaseDrv.Lock(); err != nil {
			errchan <- err
		} else {
			errchan <- nil
		}
	}()

	// wait until we either receive ErrLockTimeout, ErrStop or error from Lock operation
	err := <-errchan
	if err == nil {
		m.isLocked = true
		return nil
	}

	if errors.Is(err, ErrStop) {
		// The stop can win the race against a concurrent successful
		// lock acquisition. Drain the remaining result of the lock
		// attempt and release the advisory lock if it was acquired,
		// so that subsequent runs are not blocked.
		go func() {
			for lateErr := range errchan {
				if lateErr == nil {
					_ = m.databaseDrv.Unlock()
					return
				}
				if !errors.Is(lateErr, ErrLockTimeout) {
					// terminal result of the Lock attempt itself
					return
				}
				// ErrLockTimeout fired, but the lock attempt is still
				// running and can succeed later; keep waiting for it.
			}
		}()
	}

	return err
}

// unlock is a thread safe helper function to unlock the database.
// It should be called as early as possible when no more migrations are
// expected to be executed.
func (m *Migrate) unlock() error {
	m.isLockedMu.Lock()
	defer m.isLockedMu.Unlock()

	if err := m.databaseDrv.Unlock(); err != nil {
		// BUG: Can potentially create a deadlock. Add a timeout.
		return err
	}

	m.isLocked = false
	return nil
}

// unlockErr calls unlock and returns a combined error
// if a prevErr is not nil.
func (m *Migrate) unlockErr(prevErr error) error {
	if err := m.unlock(); err != nil {
		prevErr = errors.Join(prevErr, err)
	}
	return prevErr
}

// logPrintf writes to m.Log if not nil
func (m *Migrate) logPrintf(format string, v ...interface{}) {
	if m.Log != nil {
		m.Log.Printf(format, v...)
	}
}

// logVerbosePrintf writes to m.Log if not nil. Use for verbose logging output.
func (m *Migrate) logVerbosePrintf(format string, v ...interface{}) {
	if m.Log != nil && m.Log.Verbose() {
		m.Log.Printf(format, v...)
	}
}

// logErr writes error to m.Log if not nil
func (m *Migrate) logErr(err error) {
	if m.Log != nil {
		m.Log.Printf("error: %v", err)
	}
}
