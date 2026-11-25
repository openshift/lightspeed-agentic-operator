// Package models owns the product E2E model catalog and swap history.
package models

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Keep these keys in sync with the real-provider fixtures in test/e2e.
var supported = map[string]string{
	"claude": "anthropic", "anthropic": "anthropic", "gemini": "google", "openai": "openai", "azure-openai": "openai",
	"bedrock-claude": "anthropic", "bedrock-openai": "openai",
}

type Route struct {
	Family  string   `yaml:"family"`
	Primary string   `yaml:"primary"`
	Smoke   *bool    `yaml:"smoke"`
	Extras  []string `yaml:"extras"`
}

func (r Route) Enabled() bool { return r.Smoke == nil || *r.Smoke }

type Catalog struct {
	Routes map[string]Route `yaml:"routes"`
	order  []string
}

type Pair struct {
	Route string `json:"route"`
	Model string `json:"model"`
}

func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	catalog, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return catalog, nil
}

func Parse(data []byte) (*Catalog, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var c Catalog
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return nil, err
	}
	if len(doc.Content) > 0 {
		if routes := field(doc.Content[0], "routes"); routes != nil {
			for i := 0; i+1 < len(routes.Content); i += 2 {
				c.order = append(c.order, routes.Content[i].Value)
			}
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func field(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func (c *Catalog) Validate() error {
	var errs []error
	if len(c.order) == 0 {
		errs = append(errs, errors.New("routes must not be empty"))
	}
	for _, name := range c.order {
		route := c.Routes[name]
		if family, ok := supported[name]; !ok {
			errs = append(errs, fmt.Errorf("unknown route %q", name))
		} else if route.Family != family {
			errs = append(errs, fmt.Errorf("routes.%s.family must be %q", name, family))
		}
		validateModel := func(slot, model string) {
			if strings.TrimSpace(model) == "" {
				errs = append(errs, fmt.Errorf("routes.%s.%s is empty", name, slot))
			} else if name == "bedrock-openai" && !strings.HasPrefix(model, "openai.") {
				errs = append(errs, fmt.Errorf("routes.%s.%s must start with openai.", name, slot))
			}
		}
		validateModel("primary", route.Primary)
		if !route.Enabled() && len(route.Extras) != 0 {
			errs = append(errs, fmt.Errorf("routes.%s has extras but smoke is disabled", name))
		}
		for i, extra := range route.Extras {
			validateModel(fmt.Sprintf("extras[%d]", i), extra)
		}
	}
	return errors.Join(errs...)
}

func (c *Catalog) Primary(route string) (string, error) {
	r, ok := c.Routes[route]
	if !ok {
		return "", fmt.Errorf("unknown route %q", route)
	}
	return r.Primary, nil
}

// SmokeMatrix returns primaries first, then extras, both in file order.
func (c *Catalog) SmokeMatrix() []Pair {
	pairs := []Pair{}
	seen := make(map[Pair]bool)
	add := func(name, model string) {
		p := Pair{name, model}
		if !seen[p] {
			pairs = append(pairs, p)
			seen[p] = true
		}
	}
	for _, name := range c.order {
		if r := c.Routes[name]; r.Enabled() {
			add(name, r.Primary)
		}
	}
	for _, name := range c.order {
		if r := c.Routes[name]; r.Enabled() {
			for _, extra := range r.Extras {
				add(name, extra)
			}
		}
	}
	return pairs
}
