package app

import (
	"flag"

	"gorm.io/gorm"

	"github.com/h44z/wg-portal/internal/config"
)

// ProgramArgs are the parsed command line flags.
type ProgramArgs struct {
	MigrationSource string
	MigrationDbType string
	HaInit          bool   // prepare HA certificates/Galera config before the database starts, then exit
	HaExportCa      string // write the HA cluster CA from the database into this directory, then exit
	HaImport        string // copy a standalone SQLite database into the (empty) configured database, then exit
}

// ParseProgramArgs parses the command line flags. It must be called once, before any other flag use.
func ParseProgramArgs() ProgramArgs {
	var a ProgramArgs
	flag.StringVar(&a.MigrationSource, "migrateFrom", "", "path to v1 database file or DSN")
	flag.StringVar(&a.MigrationDbType, "migrateFromType", string(config.DatabaseSQLite),
		"old database type, either mysql, mssql, postgres or sqlite")
	flag.BoolVar(&a.HaInit, "ha-init", false,
		"HA: create certificates (orchestrator) or join the cluster (node) and write galera.cnf, then exit")
	flag.StringVar(&a.HaExportCa, "ha-export-ca", "",
		"HA: export the cluster CA from the database into the given directory, then exit")
	flag.StringVar(&a.HaImport, "ha-import", "",
		"HA: import a standalone wg-portal SQLite database file into the empty cluster database, then exit")
	flag.Parse()
	return a
}

// HandleProgramArgs handles program arguments and returns true if the program should exit.
func HandleProgramArgs(db *gorm.DB, args ProgramArgs) (exit bool, err error) {
	if args.MigrationSource != "" {
		err = migrateFromV1(db, args.MigrationSource, args.MigrationDbType)
		exit = true
	}

	return
}
