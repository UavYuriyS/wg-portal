package adapters

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/h44z/wg-portal/internal/domain"
)

// importSkippedTables are owned by the target: its schema version and HA cluster state.
var importSkippedTables = []string{"sys_stats", "ha_nodes", "ha_states", "ha_join_tokens", "ha_probe_peers"}

// ImportDatabase copies all rows of all wg-portal tables from src to dst, for example from a standalone
// SQLite database into the MariaDB of an HA cluster. Both must be migrated to the same schema version.
// Values are copied raw, so encrypted columns stay valid only if both use the same encryption passphrase.
// Values the source stored in plain text are encrypted afterwards if the target has a passphrase.
// The copy runs in one transaction and refuses to write into tables that already contain rows.
func ImportDatabase(src, dst *gorm.DB) (map[string]int, error) {
	tables, err := dst.Migrator().GetTables()
	if err != nil {
		return nil, fmt.Errorf("list target tables: %w", err)
	}
	var copyTables []string
	for _, t := range tables {
		if !slices.Contains(importSkippedTables, t) && src.Migrator().HasTable(t) {
			copyTables = append(copyTables, t)
		}
	}

	counts := map[string]int{}
	err = dst.Transaction(func(tx *gorm.DB) error {
		for _, t := range copyTables {
			var n int64
			if err := tx.Table(t).Count(&n).Error; err != nil {
				return fmt.Errorf("count %s: %w", t, err)
			}
			if n > 0 {
				return fmt.Errorf("target table %s is not empty (%d rows): import into a fresh database, "+
					"before the orchestrator's first start", t, n)
			}
		}

		if tx.Dialector.Name() == "mysql" { // rows reference each other, the order of tables is arbitrary
			if err := tx.Exec("SET FOREIGN_KEY_CHECKS=0").Error; err != nil {
				return err
			}
			defer tx.Exec("SET FOREIGN_KEY_CHECKS=1")
		}

		for _, t := range copyTables {
			var rows []map[string]any
			if err := src.Table(t).Find(&rows).Error; err != nil {
				return fmt.Errorf("read %s: %w", t, err)
			}
			if len(rows) == 0 {
				continue
			}
			if err := tx.Table(t).CreateInBatches(rows, 100).Error; err != nil {
				return fmt.Errorf("write %s: %w", t, err)
			}
			counts[t] = len(rows)
		}

		n, err := encryptPlaintextColumns(tx)
		if err != nil {
			return err
		}
		if n > 0 {
			counts["encrypted_values"] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

// encryptPlaintextColumns encrypts the values of "encstr" columns (private keys, preshared keys, API tokens)
// that are still plain text, for example after importing an install without encryption passphrase. It uses
// the registered serializer, which returns values unchanged when no passphrase is configured.
func encryptPlaintextColumns(tx *gorm.DB) (int, error) {
	ser, ok := schema.GetSerializer("encstr")
	if !ok {
		return 0, nil
	}
	count := 0
	for _, model := range []any{&domain.Interface{}, &domain.Peer{}, &domain.User{}} {
		stmt := &gorm.Statement{DB: tx}
		if err := stmt.Parse(model); err != nil {
			return count, err
		}
		pk := stmt.Schema.PrioritizedPrimaryField.DBName
		for _, f := range stmt.Schema.Fields {
			if f.TagSettings["SERIALIZER"] != "encstr" || f.DBName == "" {
				continue
			}
			var rows []map[string]any
			if err := tx.Table(stmt.Schema.Table).Select(pk, f.DBName).Where(f.DBName + " <> ''").
				Find(&rows).Error; err != nil {
				return count, fmt.Errorf("read %s.%s: %w", stmt.Schema.Table, f.DBName, err)
			}
			for _, row := range rows {
				plain := fmt.Sprintf("%s", row[f.DBName]) // string or []byte, depending on the driver
				if strings.HasPrefix(plain, "WG_ENC_") {  // already encrypted, see app.GormEncryptedStringSerializer
					continue
				}
				enc, err := ser.Value(context.Background(), f, reflect.Value{}, plain)
				if err != nil {
					return count, fmt.Errorf("encrypt %s.%s: %w", stmt.Schema.Table, f.DBName, err)
				}
				if enc == plain {
					continue // no passphrase configured
				}
				if err := tx.Table(stmt.Schema.Table).Where(pk+" = ?", row[pk]).Update(f.DBName, enc).Error; err != nil {
					return count, fmt.Errorf("write %s.%s: %w", stmt.Schema.Table, f.DBName, err)
				}
				count++
			}
		}
	}
	return count, nil
}

// FormatImportCounts renders the per-table row counts for log output.
func FormatImportCounts(counts map[string]int) string {
	var parts []string
	for t, n := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", t, n))
	}
	slices.Sort(parts)
	return strings.Join(parts, " ")
}
