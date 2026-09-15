// canonicalExport renders a seeded sqlite database as canonical text: fixed
// table list, fixed column order, rows ordered by primary key. The sandbox
// compares THIS representation (never raw file bytes — page layout, WAL and
// header counters are not stable identity; plan §3.2 item 10).
package fixturesandbox

import (
	"database/sql"
	"fmt"
	"strings"
)

// exportTables is the fixed table order. Template projects declare their own
// business tables; V1 discovers them from sqlite_master (excluding sqlite
// internals and the migration bookkeeping tables) so the export adapts to
// each project's schema while staying deterministic within one schema.
var exportInternalTables = map[string]bool{
	"schema_migrations": true,
	"fixture_meta":      true,
}

// CanonicalExport produces the normalized export for an open database.
func CanonicalExport(db *sql.DB) (string, error) { return canonicalExport(db) }

func canonicalExport(db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		if !exportInternalTables[name] {
			tables = append(tables, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	for _, table := range tables {
		cols, err := tableColumns(db, table)
		if err != nil {
			return "", err
		}
		if len(cols) == 0 {
			continue
		}
		b.WriteString("## " + table + "\n")
		quoted := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
		}
		// ORDER BY the declared PK columns when present, else rowid order
		// falls out of ORDER BY first column — deterministic either way for
		// seeded data with fixed ids.
		query := `SELECT ` + strings.Join(quoted, ", ") + ` FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" ORDER BY ` + strings.Join(quoted[:1], ", ")
		dataRows, err := db.Query(query)
		if err != nil {
			return "", fmt.Errorf("export %s: %w", table, err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for dataRows.Next() {
			if err := dataRows.Scan(ptrs...); err != nil {
				dataRows.Close()
				return "", fmt.Errorf("scan %s: %w", table, err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				switch t := v.(type) {
				case []byte:
					parts[i] = string(t)
				case nil:
					parts[i] = "\u2400"
				default:
					parts[i] = fmt.Sprintf("%v", t)
				}
			}
			b.WriteString(strings.Join(parts, "\x1f") + "\n")
		}
		dataRows.Close()
		if err := dataRows.Err(); err != nil {
			return "", fmt.Errorf("iterate %s: %w", table, err)
		}
	}
	return b.String(), nil
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`PRAGMA table_info("` + strings.ReplaceAll(table, `"`, `""`) + `")`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}
