package models

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Record struct {
	Slot       string   `yaml:"slot"`
	Model      string   `yaml:"model"`
	ReplacedBy string   `yaml:"replacedBy,omitempty"`
	SwappedAt  string   `yaml:"swappedAt"`
	ExpiresAt  string   `yaml:"expiresAt"`
	LastRun    *LastRun `yaml:"lastRun"`
}

type LastRun struct {
	Date   string `yaml:"date"`
	Route  string `yaml:"route"`
	Result string `yaml:"result"`
}

func splitSlot(slot string) (route, key string, err error) {
	parts := strings.Split(slot, ".")
	if len(parts) != 3 || parts[0] != "routes" || parts[1] == "" || (parts[2] != "primary" && parts[2] != "extras") {
		return "", "", fmt.Errorf("invalid history slot %q", slot)
	}
	return parts[1], parts[2], nil
}

// expiry clamps to the last day of the target month when needed.
func expiry(t time.Time) string {
	month := time.Date(t.Year(), t.Month()+6, 1, 0, 0, 0, 0, time.UTC)
	last := month.AddDate(0, 1, -1).Day()
	return month.AddDate(0, 0, min(t.Day(), last)-1).Format(time.DateOnly)
}

func (r Record) Validate() error {
	route, _, err := splitSlot(r.Slot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("history model is empty")
	}
	date, err := time.Parse(time.DateOnly, r.SwappedAt)
	if err != nil {
		return fmt.Errorf("invalid swappedAt: %w", err)
	}
	if r.ExpiresAt != expiry(date) {
		return fmt.Errorf("expiresAt must be %s", expiry(date))
	}
	if last := r.LastRun; last != nil {
		if _, err := time.Parse(time.DateOnly, last.Date); err != nil {
			return fmt.Errorf("invalid lastRun date: %w", err)
		}
		if last.Route != route || (last.Result != "passed" && last.Result != "failed") {
			return fmt.Errorf("invalid lastRun route or result for %s", r.Slot)
		}
	}
	return nil
}

func ParseHistory(data []byte) ([]Record, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.SequenceNode {
		return nil, errors.New("history must be a YAML list")
	}
	var records []Record
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&records); err != nil {
		return nil, err
	}
	for i, record := range records {
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("history record %d: %w", i+1, err)
		}
	}
	return records, nil
}

// Swap returns validated replacements without modifying either input file.
func Swap(versions, history []byte, slot, old, replacement, date string, summary Summary) ([]byte, []byte, []string, error) {
	fail := func(err error) ([]byte, []byte, []string, error) { return nil, nil, nil, err }
	route, key, err := splitSlot(slot)
	if err != nil {
		return fail(err)
	}
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return fail(err)
	}
	if old == replacement || strings.TrimSpace(replacement) == "" {
		return fail(errors.New("replacement must be nonempty and different from old model"))
	}
	if _, err := Parse(versions); err != nil {
		return fail(err)
	}
	records, err := ParseHistory(history)
	if err != nil {
		return fail(err)
	}
	var catalog, hist yaml.Node
	if err := yaml.Unmarshal(versions, &catalog); err != nil {
		return fail(err)
	}
	// yaml.v3 can attach leading comments to the first record, which may expire.
	header, body := historyHeader(history)
	if err := yaml.Unmarshal(body, &hist); err != nil {
		return fail(err)
	}
	target := field(field(field(catalog.Content[0], "routes"), route), key)
	var matches []*yaml.Node
	if target != nil {
		if key == "primary" && target.Value == old && target.Kind == yaml.ScalarNode {
			matches = append(matches, target)
		}
		if key == "extras" {
			for _, node := range target.Content {
				if node.Kind == yaml.ScalarNode && node.Value == old {
					matches = append(matches, node)
				}
			}
		}
	}
	if len(matches) != 1 {
		return fail(fmt.Errorf("expected exactly one %q in %s, found %d", old, slot, len(matches)))
	}
	matches[0].Value = replacement
	matches[0].Tag = "!!str"
	record := Record{Slot: slot, Model: old, ReplacedBy: replacement, SwappedAt: date, ExpiresAt: expiry(day)}
	for _, row := range summary.Results {
		if row.Route == route && row.Model == old && (record.LastRun == nil || row.Date >= record.LastRun.Date) {
			record.LastRun = &LastRun{row.Date, row.Route, row.Result}
		}
	}
	var warnings []string
	if record.LastRun == nil {
		warnings = append(warnings, fmt.Sprintf("no smoke result for %s on %s", old, route))
	}
	seq := hist.Content[0]
	retained := seq.Content[:0]
	for i, prev := range records {
		if prev.ExpiresAt > date {
			retained = append(retained, seq.Content[i])
		}
	}
	var node yaml.Node
	if err := node.Encode(record); err != nil {
		return fail(err)
	}
	seq.Content = append(retained, &node)
	seq.Style &^= yaml.FlowStyle
	next, err := encode(&catalog)
	if err != nil {
		return fail(err)
	}
	newHistory, err := encode(&hist)
	if err != nil {
		return fail(err)
	}
	if _, err := Parse(next); err != nil {
		return fail(err)
	}
	newHistory = append(bytes.Clone(header), newHistory...)
	if _, err := ParseHistory(newHistory); err != nil {
		return fail(err)
	}
	return next, newHistory, warnings, nil
}

func historyHeader(data []byte) (header, body []byte) {
	body = data
	for len(body) > 0 {
		line, rest, _ := bytes.Cut(body, []byte("\n"))
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] != '#' {
			break
		}
		body = rest
	}
	return data[:len(data)-len(body)], body
}

func encode(node *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	e := yaml.NewEncoder(&b)
	e.SetIndent(2)
	if err := e.Encode(node); err != nil {
		return nil, err
	}
	if err := e.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func CheckHistory(base, head *Catalog, history []Record, today string) []string {
	var warnings []string
	seen := map[[2]string]bool{}
	for _, r := range history {
		seen[[2]string{r.Slot, r.Model}] = true
		if r.ExpiresAt <= today {
			warnings = append(warnings, fmt.Sprintf("expired history record: %s %s (%s)", r.Slot, r.Model, r.ExpiresAt))
		}
	}
	missing := func(slot, model string) {
		key := [2]string{slot, model}
		if !seen[key] {
			warnings = append(warnings, fmt.Sprintf("removed %s from %s without a history record", model, slot))
			seen[key] = true
		}
	}
	if base == nil || head == nil {
		return warnings
	}
	for _, name := range base.order {
		before, after := base.Routes[name], head.Routes[name]
		if before.Primary != after.Primary {
			missing("routes."+name+".primary", before.Primary)
		}
		keep := map[string]bool{}
		for _, model := range after.Extras {
			keep[model] = true
		}
		for _, model := range before.Extras {
			if !keep[model] {
				missing("routes."+name+".extras", model)
			}
		}
	}
	return warnings
}
