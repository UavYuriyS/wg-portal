package adapters

import (
	"fmt"
	"slices"
	"strings"

	"gorm.io/gorm"
)

// importSkippedTables are owned by the target: its schema version and HA cluster state.
var importSkippedTables = []string{"sys_stats", "ha_nodes", "ha_states", "ha_join_tokens", "ha_probe_peers"}

// ImportDatabase copies all rows of all wg-portal tables from src to dst, for example from a standalone
// SQLite database into the MariaDB of an HA cluster. Both must be migrated to the same schema version.
// Values are copied raw, so encrypted columns stay valid only if both use the same encryption passphrase.
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
		return nil
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
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
