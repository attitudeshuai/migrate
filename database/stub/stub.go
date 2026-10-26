package stub

import (
	"io"
	nurl "net/url"
	"reflect"
	"sync/atomic"

	"github.com/golang-migrate/migrate/v4/database"
)

func init() {
	database.Register("stub", &Stub{})
}

type Stub struct {
	Url               string
	Instance          interface{}
	CurrentVersion    int
	MigrationSequence []string
	LastRunMigration  []byte // todo: make []string
	IsDirty           bool
	isLocked          atomic.Bool

	Config *Config
}

func (s *Stub) Open(url string) (database.Driver, error) {
	authz := database.ParseDropAuthorization(nurl.Values{})
	if purl, err := nurl.Parse(url); err == nil {
		authz = database.ParseDropAuthorization(purl.Query())
	}
	authz.InstanceID = url

	return &Stub{
		Url:               url,
		CurrentVersion:    database.NilVersion,
		MigrationSequence: make([]string, 0),
		Config:            &Config{DropAuthorization: authz},
	}, nil
}

type Config struct {
	// DropAuthorization states whether Drop is explicitly authorized for the
	// instance. The zero value denies Drop. Set Authorized to true when
	// creating the driver via WithInstance.
	DropAuthorization database.DropAuthorization
}

func WithInstance(instance interface{}, config *Config) (database.Driver, error) {
	config.DropAuthorization = database.NormalizeDropAuthorization(config.DropAuthorization, "")
	return &Stub{
		Instance:          instance,
		CurrentVersion:    database.NilVersion,
		MigrationSequence: make([]string, 0),
		Config:            config,
	}, nil
}

func (s *Stub) Close() error {
	return nil
}

func (s *Stub) Lock() error {
	if !s.isLocked.CompareAndSwap(false, true) {
		return database.ErrLocked
	}
	return nil
}

func (s *Stub) Unlock() error {
	if !s.isLocked.CompareAndSwap(true, false) {
		return database.ErrNotLocked
	}
	return nil
}

func (s *Stub) Run(migration io.Reader) error {
	m, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	s.LastRunMigration = m
	s.MigrationSequence = append(s.MigrationSequence, string(m[:]))
	return nil
}

func (s *Stub) SetVersion(version int, state bool) error {
	s.CurrentVersion = version
	s.IsDirty = state
	return nil
}

func (s *Stub) Version() (version int, dirty bool, err error) {
	return s.CurrentVersion, s.IsDirty, nil
}

const DROP = "DROP"

// DropAuthorized implements database.DropAuthorizer.
func (s *Stub) DropAuthorized() database.DropAuthorization {
	return s.Config.DropAuthorization
}

var _ database.DropAuthorizer = (*Stub)(nil)

func (s *Stub) Drop() error {
	if err := database.RequireDropAuthorization(s.Config.DropAuthorization); err != nil {
		return err
	}

	s.CurrentVersion = database.NilVersion
	s.LastRunMigration = nil
	s.MigrationSequence = append(s.MigrationSequence, DROP)
	return nil
}

func (s *Stub) EqualSequence(seq []string) bool {
	return reflect.DeepEqual(seq, s.MigrationSequence)
}
