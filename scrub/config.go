// Package scrub turns a user's scrub configuration into the SELECT list used to export a table.
//
// The design principle is that the user decides what is sensitive. Columns that the configuration
// does not mention are exported as-is; the tool never guesses at what looks like PII. What it does
// guarantee is that a rule the user *did* write is either applied or reported -- a rule that
// silently fails to apply is the one outcome worse than no rule at all.
package scrub

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-multierror/multierror"
	"gopkg.in/yaml.v3"
)

// ConfigVersion is the only scrub config schema version this build understands
const ConfigVersion = 1

type Config struct {
	Version int `yaml:"version"`

	// FKMode controls how foreign keys are recreated during the post-data phase of a restore.
	//
	// This is global rather than per-table because a `where` filter on one table orphans rows in
	// another: filtering parents leaves children pointing at rows that no longer exist, and the
	// FK that spans them cannot be validated no matter which of the two was filtered.
	FKMode FKMode `yaml:"fk_mode"`

	// Tables is keyed by the schema-qualified table name, e.g. "public.users"
	Tables map[string]TableConfig `yaml:"tables"`
}

type TableConfig struct {
	// Mode defaults to TableModeFull
	Mode TableMode `yaml:"mode"`

	// Where is a raw SQL predicate restricting the exported rows. See Config.FKMode.
	Where string `yaml:"where"`

	// Columns maps a column name to either a builtin transform name or a raw SQL expression.
	// A name is read as a builtin only when it matches one exactly; everything else is passed
	// through to postgres untouched.
	Columns map[string]string `yaml:"columns"`
}

type TableMode string

const (
	// TableModeFull exports every row the Where clause admits
	TableModeFull TableMode = ""
	// TableModeSkip exports the table's structure but none of its rows
	TableModeSkip TableMode = "skip"
)

type FKMode string

const (
	// FKModeValidate recreates foreign keys normally, which scans the data and fails on a
	// dangling reference
	FKModeValidate FKMode = ""
	// FKModeNotValid recreates foreign keys as NOT VALID, so a restore survives row filtering
	// that broke referential integrity. The constraint still applies to future writes.
	FKModeNotValid FKMode = "not_valid"
)

// Parse reads a scrub configuration.
//
// Unknown fields are rejected rather than ignored: a misspelled key means a rule the user believes
// is in force is not, which is exactly the failure this package exists to prevent.
func Parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("error parsing scrub config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks the configuration against itself. Checking it against a real database -- that
// every table and column named here still exists -- happens in the export preflight.
func (c Config) Validate() error {
	errs := make([]error, 0)

	if c.Version != ConfigVersion {
		errs = append(errs, fmt.Errorf("unsupported scrub config version %d, expected %d", c.Version, ConfigVersion))
	}

	switch c.FKMode {
	case FKModeValidate, FKModeNotValid:
	default:
		errs = append(errs, fmt.Errorf("invalid fk_mode %q, expected one of: validate, not_valid", c.FKMode))
	}

	for _, name := range c.TableNames() {
		tc := c.Tables[name]
		if _, _, err := SplitQualified(name); err != nil {
			errs = append(errs, err)
		}

		switch tc.Mode {
		case TableModeFull, TableModeSkip:
		default:
			errs = append(errs, fmt.Errorf("table %q: invalid mode %q, expected one of: skip", name, tc.Mode))
		}

		// A skipped table exports no rows, so a filter or a column rule on it would silently do
		// nothing -- report the contradiction instead of picking a winner
		if tc.Mode == TableModeSkip {
			if tc.Where != "" {
				errs = append(errs, fmt.Errorf("table %q: `where` has no effect with mode: skip", name))
			}
			if len(tc.Columns) > 0 {
				errs = append(errs, fmt.Errorf("table %q: `columns` has no effect with mode: skip", name))
			}
		}

		for _, col := range sortedKeys(tc.Columns) {
			if strings.TrimSpace(tc.Columns[col]) == "" {
				errs = append(errs, fmt.Errorf("table %q column %q: transform is empty", name, col))
			}
		}
	}

	if len(errs) > 0 {
		return multierror.New(errs)
	}
	return nil
}

// TableConfigFor returns the configuration for a table, and whether one was declared
func (c Config) TableConfigFor(schema, name string) (TableConfig, bool) {
	tc, ok := c.Tables[fmt.Sprintf("%s.%s", schema, name)]
	return tc, ok
}

// TableNames lists the configured tables in a stable order.
func (c Config) TableNames() []string {
	return sortedKeys(c.Tables)
}

// SplitQualified splits "public.users" into its schema and table parts.
//
// Qualification is required: an unqualified name would resolve against search_path, and the
// export runs with a search_path the user never sees.
func SplitQualified(name string) (schema string, table string, err error) {
	parts := strings.Split(name, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("table %q must be schema-qualified, e.g. \"public.%s\"", name, name)
	}
	return parts[0], parts[1], nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
