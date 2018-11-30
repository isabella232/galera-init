package db_helper

import (
	"fmt"
	"os/exec"

	"database/sql"

	"io/ioutil"

	"code.cloudfoundry.org/lager"
	"github.com/cloudfoundry/galera-init/config"
	s "github.com/cloudfoundry/galera-init/db_helper/seeder"
	"github.com/cloudfoundry/galera-init/os_helper"
	"github.com/go-sql-driver/mysql"
)

//go:generate counterfeiter . DBHelper

type DBHelper interface {
	StartMysqldInStandAlone()
	StartMysqldInJoin() (*exec.Cmd, error)
	StartMysqldInBootstrap() (*exec.Cmd, error)
	StopMysqld()
	Upgrade() (output string, err error)
	IsDatabaseReachable() bool
	IsProcessRunning() bool
	Seed() error
	RunPostStartSQL() error
	TestDatabaseCleanup() error
}

type GaleraDBHelper struct {
	osHelper        os_helper.OsHelper
	dbSeeder        s.Seeder
	logFileLocation string
	logger          lager.Logger
	config          *config.DBHelper
}

func NewDBHelper(
	osHelper os_helper.OsHelper,
	config *config.DBHelper,
	logFileLocation string,
	logger lager.Logger) *GaleraDBHelper {
	return &GaleraDBHelper{
		osHelper:        osHelper,
		config:          config,
		logFileLocation: logFileLocation,
		logger:          logger,
	}
}

var BuildSeeder = func(db *sql.DB, config config.PreseededDatabase, logger lager.Logger) s.Seeder {
	return s.NewSeeder(db, config, logger)
}

var OpenDBConnection = func(config *config.DBHelper) (*sql.DB, error) {
	var err error

	db := new(sql.DB)
	c := mysql.Config{
		User:   config.User,
		Passwd: config.Password,
		Net:    "unix",
		Addr:   config.Socket,
	}

	db, err = sql.Open("mysql", c.FormatDSN())
	if err != nil {
		return nil, err
	}

	return db, nil
}
var CloseDBConnection = func(db *sql.DB) error {
	return db.Close()
}

func (m GaleraDBHelper) IsProcessRunning() bool {
	_, err := m.osHelper.RunCommand(
		"mysqladmin",
		"--defaults-file=/var/vcap/jobs/pxc-mysql/config/mylogin.cnf",
		"status")
	return err == nil
}

func (m GaleraDBHelper) StartMysqldInStandAlone() {
	_, err := m.osHelper.RunCommand(
		"mysqld",
		"--defaults-file=/var/vcap/jobs/pxc-mysql/config/my.cnf",
		"--wsrep-on=OFF",
		"--wsrep-desync=ON",
		"--wsrep-OSU-method=RSU",
		"--wsrep-provider=none",
		"--skip-networking",
		"--daemonize",
	)

	if err != nil {
		m.logger.Fatal("Error starting mysqld in stand-alone", err)
	}
}

func (m GaleraDBHelper) StartMysqldInJoin() (*exec.Cmd, error) {
	m.logger.Info("Starting mysqld with 'join'.")
	cmd, err := m.startMysqldAsChildProcess("--defaults-file=/var/vcap/jobs/pxc-mysql/config/my.cnf")

	if err != nil {
		m.logger.Info(fmt.Sprintf("Error starting mysqld: %s", err.Error()))
		return nil, err
	}
	return cmd, nil
}

func (m GaleraDBHelper) StartMysqldInBootstrap() (*exec.Cmd, error) {
	m.logger.Info("Starting mysql with 'bootstrap'.")
	cmd, err := m.startMysqldAsChildProcess("--defaults-file=/var/vcap/jobs/pxc-mysql/config/my.cnf", "--wsrep-new-cluster")

	if err != nil {
		m.logger.Info(fmt.Sprintf("Error starting node with 'bootstrap': %s", err.Error()))
		return nil, err
	}
	return cmd, nil
}

func (m GaleraDBHelper) StopMysqld() {
	m.logger.Info("Stopping node")
	_, err := m.osHelper.RunCommand(
		"mysqladmin",
		"--defaults-file=/var/vcap/jobs/pxc-mysql/config/mylogin.cnf",
		"shutdown")
	if err != nil {
		m.logger.Fatal("Error stopping mysqld", err)
	}
}

func (m GaleraDBHelper) startMysqldAsChildProcess(mysqlArgs ...string) (*exec.Cmd, error) {
	return m.osHelper.StartCommand(
		m.logFileLocation,
		"mysqld",
		mysqlArgs...)
}

func (m GaleraDBHelper) Upgrade() (output string, err error) {
	return m.osHelper.RunCommand(
		m.config.UpgradePath,
		"--defaults-file=/var/vcap/jobs/pxc-mysql/config/mylogin.cnf",
	)
}

func (m GaleraDBHelper) rescue() {

	r := recover()
	if r != nil {
		m.logger.Info("recovered from panic")
	}
}
func (m GaleraDBHelper) IsDatabaseReachable() bool {
	m.logger.Info(fmt.Sprintf("Determining if database is reachable"))

	db, err := OpenDBConnection(m.config)
	if err != nil {
		m.logger.Info("database not reachable", lager.Data{"err": err})
		return false
	}
	defer CloseDBConnection(db)

	var (
		unused string
		value  string
	)
	m.logger.Info(fmt.Sprintf("about to check that we can show global variables like wsrep_on. DB is: %#v", db))
	err = db.QueryRow(`SHOW GLOBAL VARIABLES LIKE 'wsrep\_on'`).Scan(&unused, &value)
	if err != nil {
		if err == sql.ErrNoRows {
			m.logger.Info(fmt.Sprintf("Database is reachable, Galera is off"))
			return true
		}
		m.logger.Info(fmt.Sprintf("got an error showing variables like wsrep_on. Err:%s", err.Error()))
		return false
	}

	m.logger.Info("finished showing global variables with no error")
	if value == "OFF" {
		m.logger.Info(fmt.Sprintf("Database is reachable, Galera is off"))
		return true
	}

	err = db.QueryRow(`SHOW STATUS LIKE 'wsrep\_ready'`).Scan(&unused, &value)
	if err != nil {
		m.logger.Info("scanning global status like wsrep_ready failed")
		return false
	}

	m.logger.Info(fmt.Sprintf("Database is reachable, Galera is %s", value))
	return value == "ON"
}

func (m GaleraDBHelper) Seed() error {
	if m.config.PreseededDatabases == nil || len(m.config.PreseededDatabases) == 0 {
		m.logger.Info("No preseeded databases specified, skipping seeding.")
		return nil
	}

	m.logger.Info("Preseeding Databases")

	db, err := OpenDBConnection(m.config)
	if err != nil {
		m.logger.Error("database not reachable", err)
		return err
	}
	defer CloseDBConnection(db)

	for _, dbToCreate := range m.config.PreseededDatabases {
		seeder := BuildSeeder(db, dbToCreate, m.logger)

		if err := seeder.CreateDBIfNeeded(); err != nil {
			return err
		}

		userAlreadyExists, err := seeder.IsExistingUser()
		if err != nil {
			return err
		}

		if userAlreadyExists == false {
			if err := seeder.CreateUser(); err != nil {
				return err
			}
		} else {
			if err := seeder.UpdateUser(); err != nil {
				return err
			}
		}

		if err := seeder.GrantUserPrivileges(); err != nil {
			return err
		}
	}

	if err := m.flushPrivileges(db); err != nil {
		return err
	}

	return nil
}

func (m GaleraDBHelper) flushPrivileges(db *sql.DB) error {
	if _, err := db.Exec("FLUSH PRIVILEGES"); err != nil {
		m.logger.Error("Error flushing privileges", err)
		return err
	}

	return nil
}

func (m GaleraDBHelper) RunPostStartSQL() error {
	m.logger.Info("Running Post Start SQL Queries")

	db, err := OpenDBConnection(m.config)
	if err != nil {
		m.logger.Error("database not reachable", err)
		return err
	}
	defer CloseDBConnection(db)

	for _, file := range m.config.PostStartSQLFiles {
		sqlString, err := ioutil.ReadFile(file)
		if err != nil {
			m.logger.Error("error reading PostStartSQL file", err, lager.Data{
				"filePath": file,
			})
		} else {
			if _, err := db.Exec(string(sqlString)); err != nil {
				return err
			}

		}
	}

	return nil
}

func (m GaleraDBHelper) TestDatabaseCleanup() error {
	m.logger.Info("Cleaning up databases")
	db, err := OpenDBConnection(m.config)
	if err != nil {
		panic("")
	}
	defer CloseDBConnection(db)

	err = m.deletePermissionsToCreateTestDatabases(db)
	if err != nil {
		return err
	}

	err = m.flushPrivileges(db)
	if err != nil {
		return err
	}

	names, err := m.testDatabaseNames(db)
	if err != nil {
		return err
	}

	return m.dropDatabasesNamed(db, names)
}

func (m GaleraDBHelper) deletePermissionsToCreateTestDatabases(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM mysql.db WHERE Db IN('test', 'test\_%')`)
	if err != nil {
		m.logger.Error("error deleting permissions for test databases", err)
		return err
	}

	return nil
}

func (m GaleraDBHelper) testDatabaseNames(db *sql.DB) ([]string, error) {
	var allTestDatabaseNames []string
	testDatabaseNames, err := m.showDatabaseNamesLike("test", db)
	if err != nil {
		m.logger.Error("error searching for 'test' databases", err)
		return nil, err
	}
	allTestDatabaseNames = append(allTestDatabaseNames, testDatabaseNames...)

	testUnderscoreDatabaseNames, err := m.showDatabaseNamesLike(`test\_%`, db)
	if err != nil {
		m.logger.Error("error searching for 'test_%' databases", err)
		return nil, err
	}
	allTestDatabaseNames = append(allTestDatabaseNames, testUnderscoreDatabaseNames...)
	return allTestDatabaseNames, nil
}

func (m GaleraDBHelper) showDatabaseNamesLike(pattern string, db *sql.DB) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf("SHOW DATABASES LIKE '%s'", pattern))
	if err != nil {
		return nil, err
	}

	var dbNames []string
	defer rows.Close()
	for rows.Next() {
		var name string
		err = rows.Scan(&name)
		if err != nil {
			return nil, err
		}

		dbNames = append(dbNames, name)
	}
	err = rows.Err() // get any error encountered during iteration
	if err != nil {
		return nil, err
	}

	return dbNames, nil
}

func (m GaleraDBHelper) dropDatabasesNamed(db *sql.DB, names []string) error {
	for _, n := range names {
		_, err := db.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", n))
		if err != nil {
			return err
		}
	}

	return nil
}
