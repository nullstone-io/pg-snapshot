package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ArtifactVersion is the layout of the snapshot directory and the shape of this manifest.
//
// The restore refuses an artifact it does not recognise. Snapshot and restore ship at one version
// precisely so this never has to tolerate a mismatch in practice, but the check stays because the
// two run in different environments and nothing physically stops an old restore from finding a
// new artifact in the bucket.
const ArtifactVersion = 1

const ManifestFile = "manifest.json"

type Manifest struct {
	ArtifactVersion int       `json:"artifactVersion"`
	Tool            string    `json:"tool"`
	CreatedAt       time.Time `json:"createdAt"`

	Source Source `json:"source"`

	// Scrubbed is the gate the restore checks. Nothing writes this false -- an artifact that was
	// not scrubbed is never produced -- but its absence is what stops a hand-made pg_dump from
	// being restored through this tool by mistake.
	Scrubbed bool `json:"scrubbed"`

	// ScrubConfigSHA256 identifies the configuration without reproducing it. The config names
	// the columns someone considered sensitive, which is not something to copy into an artifact
	// that outlives the run.
	ScrubConfigSHA256 string `json:"scrubConfigSha256"`

	FKMode string `json:"fkMode"`

	Tables    []TableEntry `json:"tables"`
	Sequences []Sequence   `json:"sequences"`
}

type Source struct {
	ServerMajor int    `json:"serverMajor"`
	Database    string `json:"database"`
}

type TableEntry struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`

	// Skipped means no rows were exported -- mode: skip or mode: skip-data
	Skipped bool `json:"skipped"`

	// Excluded means mode: skip: the structure is not in the schema dump either, so the table
	// does not exist in a restored database. Recorded because "the table is missing" and "the
	// table is empty" are different questions, and the manifest is where both are answered.
	// Implies Skipped.
	Excluded bool `json:"excluded,omitempty"`

	// Where is the row filter that was applied, if any. Recorded because it explains a row count
	// that does not match production, and because it is what makes fkMode meaningful.
	Where string `json:"where,omitempty"`

	// Columns is the exported column list in COPY order
	Columns []string `json:"columns"`

	// Transforms maps a column to the *configured* transform, never the generated SQL -- that
	// embeds the run's salt, and this file ships to the bucket next to the data.
	Transforms map[string]string `json:"transforms,omitempty"`

	// Tail records the heap-tail sampling this entry's rows came from, when `tail_rows` was
	// configured -- including a run where the window fell back to the whole table, which reads
	// as PagesRead == TotalPages. Absent for a table exported in full without tail_rows.
	Tail *TailReport `json:"tail,omitempty"`

	RowCount int64  `json:"rowCount"`
	File     string `json:"file,omitempty"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256,omitempty"`
}

// TailReport is what a tail-sampled table leaves behind for an operator.
//
// RequestedRows against the entry's RowCount shows the deliberate overshoot; Min/Max of the
// report column is how the tail-is-newest assumption is watched -- it degrades silently under
// heavy UPDATE traffic or after a VACUUM FULL, and a reported window that stops looking recent
// is the visible symptom. This field is additive, so the artifact version is unchanged: an older
// restore ignores it, and the rows load the same either way.
type TailReport struct {
	// RequestedRows is the configured tail_rows
	RequestedRows int64 `json:"requestedRows"`

	// TotalPages and PagesRead describe the heap window: how big the table was, and how little
	// of it the export read
	TotalPages int64 `json:"totalPages"`
	PagesRead  int64 `json:"pagesRead"`

	// ReportColumn is the configured tail_report_column, with the exported window's range of it
	// rendered as text. Empty when none was named, or when the window held no rows.
	ReportColumn string `json:"reportColumn,omitempty"`
	Min          string `json:"min,omitempty"`
	Max          string `json:"max,omitempty"`
}

func (t TableEntry) Qualified() string {
	return fmt.Sprintf("%s.%s", t.Schema, t.Name)
}

// CopyIn renders the statement that loads this table's data file.
//
// The column list comes from the manifest rather than from the target's catalog, which is what
// lets a restore survive schema drift: a migration that added a column in the middle of the table
// changes attribute order, and an implicit column list would silently load values into the wrong
// columns.
func (t TableEntry) CopyIn() string {
	quoted := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		quoted = append(quoted, pq.QuoteIdentifier(c))
	}
	return fmt.Sprintf("COPY %s.%s (%s) FROM STDIN",
		pq.QuoteIdentifier(t.Schema), pq.QuoteIdentifier(t.Name), strings.Join(quoted, ", "))
}

func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error encoding manifest: %w", err)
	}
	return append(b, '\n'), nil
}

func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("error parsing manifest: %w", err)
	}
	return &m, nil
}

// Validate decides whether this artifact may be restored into a server of the given major.
func (m Manifest) Validate(targetMajor int) error {
	if m.ArtifactVersion != ArtifactVersion {
		return fmt.Errorf("snapshot uses artifact version %d, this build understands %d; "+
			"upgrade the restore module to match the snapshot module",
			m.ArtifactVersion, ArtifactVersion)
	}

	// The one check that exists purely to stop an operator mistake rather than a bug
	if !m.Scrubbed {
		return fmt.Errorf("snapshot is not marked scrubbed and will not be restored")
	}

	if m.Source.ServerMajor > targetMajor {
		return fmt.Errorf("snapshot came from postgres %d and the target runs %d; "+
			"a dump cannot be restored into an older major version",
			m.Source.ServerMajor, targetMajor)
	}
	if targetMajor < MinServerMajor {
		return fmt.Errorf("target runs postgres %d; pg-snapshot requires %d or newer",
			targetMajor, MinServerMajor)
	}
	return nil
}

// ColumnsAdded reports columns present in this manifest that the previous one did not have.
//
// This is drift *reporting*, not drift blocking: the user decides what is sensitive, so a new
// column exports as-is. Surfacing it in the run output is what gives them the chance to notice.
func (m Manifest) ColumnsAdded(previous *Manifest) []string {
	if previous == nil {
		return nil
	}

	before := map[string]bool{}
	for _, t := range previous.Tables {
		for _, c := range t.Columns {
			before[fmt.Sprintf("%s.%s", t.Qualified(), c)] = true
		}
	}

	added := make([]string, 0)
	for _, t := range m.Tables {
		for _, c := range t.Columns {
			if key := fmt.Sprintf("%s.%s", t.Qualified(), c); !before[key] {
				added = append(added, key)
			}
		}
	}
	sort.Strings(added)
	return added
}

// TotalRows sums the exported row counts, for the run summary
func (m Manifest) TotalRows() int64 {
	var total int64
	for _, t := range m.Tables {
		total += t.RowCount
	}
	return total
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
