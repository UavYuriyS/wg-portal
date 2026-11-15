package ha

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gorm.io/gorm"

	"github.com/h44z/wg-portal/internal/adapters"
	"github.com/h44z/wg-portal/internal/config"
)

// ImportStandalone copies the database of a standalone wg-portal (SQLite file) into dst, the cluster
// database ("wg-portal -ha-import <file>"). A temporary copy of the file is migrated to the current schema
// first, so the original stays untouched and older versions can be imported.
// shortcut: only SQLite sources; other engines can be dumped to the cluster with their own tools.
func ImportStandalone(cfg *config.Config, dst *gorm.DB, sqliteFile string) error {
	if _, err := os.Stat(sqliteFile); err != nil {
		return fmt.Errorf("source database: %w", err)
	}
	tmpDir, err := os.MkdirTemp("", "wg-portal-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "source.db")
	for _, suffix := range []string{"", "-wal"} { // the WAL file holds committed data if the source was not shut down cleanly
		if err := copyFile(sqliteFile+suffix, tmpFile+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			return fmt.Errorf("copy source database: %w", err)
		}
	}

	src, err := adapters.NewDatabase(config.DatabaseConfig{Type: config.DatabaseSQLite, DSN: tmpFile})
	if err != nil {
		return err
	}
	if _, err := adapters.NewSqlRepository(src, cfg); err != nil { // brings the copy to the current schema
		return fmt.Errorf("migrate source database: %w", err)
	}
	if sqlDb, err := src.DB(); err == nil {
		defer sqlDb.Close()
	}

	counts, err := adapters.ImportDatabase(src, dst)
	if err != nil {
		return err
	}
	slog.Info("standalone database imported", "source", sqliteFile, "rows", adapters.FormatImportCounts(counts))
	slog.Warn("delete the source file once the import is verified: it is left unchanged and may hold keys in plain text",
		"source", sqliteFile)
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
