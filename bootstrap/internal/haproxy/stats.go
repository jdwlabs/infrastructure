package haproxy

import (
	"encoding/csv"
	"fmt"
	"sort"
	"strings"
)

// ServerStat is one server row from the HAProxy runtime stats socket.
//
// The raw CSV column names are kept rather than mapped onto a fixed struct:
// HAProxy appends columns between releases, and a caller asking for a column
// this code has never heard of should get it instead of a silent blank.
type ServerStat struct {
	columns map[string]string
}

// friendly names accepted by --fields, mapped onto the CSV column that carries
// them. The CSV names are also accepted directly.
var statFieldAliases = map[string]string{
	"name":     "svname",
	"proxy":    "pxname",
	"addr":     "addr",
	"status":   "status",
	"check":    "check_status",
	"weight":   "weight",
	"downtime": "downtime",
	"rate":     "rate",
	"since":    "lastchg",
}

// DefaultStatFields is the schema every backend row prints unless the caller
// asks for more. Four columns is enough to decide what to do next: which
// server, where it points, whether it is serving, and why the health check
// says what it does.
var DefaultStatFields = []string{"name", "addr", "status", "check"}

// aggregate rows the stats socket emits alongside the per-server rows. They
// duplicate information the caller already has and would inflate the count.
var aggregateRows = map[string]bool{"FRONTEND": true, "BACKEND": true}

// ParseStats parses the CSV emitted by "show stat" on the runtime socket and
// returns the per-server rows.
//
// Column positions are resolved from the header rather than hardcoded, because
// the index of everything after a newly inserted column shifts on a HAProxy
// upgrade and a silently misaligned "status" column would report health that
// belongs to a different field.
func ParseStats(raw string) ([]ServerStat, error) {
	header, body, err := splitStatsCSV(raw)
	if err != nil {
		return nil, err
	}

	reader := csv.NewReader(strings.NewReader(body))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse stats CSV: %w", err)
	}

	var stats []ServerStat
	for _, record := range records {
		columns := make(map[string]string, len(header))
		for i, name := range header {
			if i < len(record) {
				columns[name] = strings.TrimSpace(record[i])
			}
		}
		svname := columns["svname"]
		if svname == "" || aggregateRows[svname] {
			continue
		}
		stats = append(stats, ServerStat{columns: columns})
	}
	return stats, nil
}

// splitStatsCSV separates the "# pxname,svname,..." header from the data rows.
func splitStatsCSV(raw string) ([]string, string, error) {
	var header []string
	var body strings.Builder

	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if header != nil {
				continue
			}
			for _, name := range strings.Split(strings.TrimPrefix(trimmed, "#"), ",") {
				header = append(header, strings.TrimSpace(name))
			}
			continue
		}
		body.WriteString(trimmed)
		body.WriteString("\n")
	}

	if len(header) == 0 {
		return nil, "", fmt.Errorf("stats output carried no CSV header — the socket answered, but not with stats")
	}
	return header, body.String(), nil
}

// Field returns the display value of a column, or "-" when the running HAProxy
// did not populate it. An empty cell and an absent column are the same answer
// to the caller and both are better shown than dropped.
func (s ServerStat) Field(name string) string {
	if v, ok := s.columns[canonicalStatField(name)]; ok && v != "" {
		return v
	}
	return "-"
}

// Name is the server's name in the generated config (e.g. talos-cp-201).
func (s ServerStat) Name() string { return s.Field("name") }

// Status is the health state reported by HAProxy (UP, DOWN, MAINT, no check).
func (s ServerStat) Status() string { return s.columns["status"] }

// IsUp reports whether the server is currently taking traffic. HAProxy
// qualifies a transitional state as "UP 2/3", so this is a prefix test.
func (s ServerStat) IsUp() bool {
	return strings.HasPrefix(strings.ToUpper(s.Status()), "UP")
}

// Columns lists every column the running HAProxy populated for this row.
func (s ServerStat) Columns() []string {
	names := make([]string, 0, len(s.columns))
	for name := range s.columns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func canonicalStatField(name string) string {
	if csvName, ok := statFieldAliases[strings.ToLower(name)]; ok {
		return csvName
	}
	return name
}

// UpCount returns how many of the parsed servers are currently up.
func UpCount(stats []ServerStat) int {
	up := 0
	for _, s := range stats {
		if s.IsUp() {
			up++
		}
	}
	return up
}

// ResolveStatFields turns a --fields request into the column list to print.
// An unrecognised name is rejected by name rather than dropped: a silently
// ignored field returns a table the caller believes is what they asked for.
func ResolveStatFields(requested []string, stats []ServerStat) ([]string, error) {
	if len(requested) == 0 {
		return DefaultStatFields, nil
	}

	available := map[string]bool{}
	for _, s := range stats {
		for _, name := range s.Columns() {
			available[name] = true
		}
	}

	fields := make([]string, 0, len(requested))
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		canonical := canonicalStatField(name)
		if _, isAlias := statFieldAliases[strings.ToLower(name)]; !isAlias && len(available) > 0 && !available[canonical] {
			return nil, fmt.Errorf("unknown backend field %q — valid names: %s",
				name, strings.Join(KnownStatFields(stats), ", "))
		}
		fields = append(fields, name)
	}
	if len(fields) == 0 {
		return DefaultStatFields, nil
	}
	return fields, nil
}

// AllStatFields is every column the running HAProxy emitted, for --full.
func AllStatFields(stats []ServerStat) []string {
	if len(stats) == 0 {
		return DefaultStatFields
	}
	return stats[0].Columns()
}

// KnownStatFields lists the friendly aliases plus whatever the running HAProxy
// actually emitted, so an error message names every value that would work.
func KnownStatFields(stats []ServerStat) []string {
	seen := map[string]bool{}
	var names []string
	for alias := range statFieldAliases {
		if !seen[alias] {
			seen[alias] = true
			names = append(names, alias)
		}
	}
	for _, name := range AllStatFields(stats) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
