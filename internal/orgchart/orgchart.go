// Package orgchart holds the display metadata for the club's mailing lists: Chinese
// names, which section each list belongs to, what order they appear in, and which
// officer role is responsible for each one.
//
// It deliberately does NOT define the organizational hierarchy. That lives in Google as
// nested group membership and is what actually decides who receives mail; keeping a
// second copy here would drift silently. See docs/organization.md.
//
// The package imports nothing from this module, which keeps it usable from any layer
// without risking an import cycle.
package orgchart

import (
	_ "embed"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// chart.yaml is embedded rather than read from disk so the binary carries its own
// display metadata: no deploy-time file to forget, and a malformed edit fails the build
// or the startup check rather than the first request that needs it.
//
//go:embed chart.yaml
var chartYAML []byte

// unlistedOrder sorts groups that chart.yaml does not mention after everything it does.
// New mailing lists therefore show up at the end instead of vanishing.
const unlistedOrder = 1 << 30

// UnsectionedKey is the section reported for a group chart.yaml does not mention.
const UnsectionedKey = "unsectioned"

type Group struct {
	Key  string `yaml:"-"`
	Name string `yaml:"name"`
}

type Section struct {
	Key    string   `yaml:"key"`
	Name   string   `yaml:"name"`
	Hidden bool     `yaml:"hidden"`
	Groups []string `yaml:"groups"`
}

// Role is an officer position and the mailing lists it is responsible for.
type Role struct {
	Key   string   `yaml:"key"`
	Name  string   `yaml:"name"`
	Group string   `yaml:"group"`
	Owns  []string `yaml:"owns"`
}

type file struct {
	Sections []Section        `yaml:"sections"`
	Groups   map[string]Group `yaml:"groups"`
	Roles    []Role           `yaml:"roles"`
}

type Chart struct {
	sections []Section
	groups   map[string]Group
	roles    []Role

	order     map[string]int
	sectionOf map[string]Section
	ownerOf   map[string]Role
}

// Load parses and validates the embedded chart.
//
// Every failure it reports is a typo in a committed file, so callers should treat an
// error as fatal at startup rather than degrading: a chart that references a group it
// does not name would otherwise render blank labels at request time.
func Load() (*Chart, error) {
	var f file
	if err := yaml.Unmarshal(chartYAML, &f); err != nil {
		return nil, fmt.Errorf("failed to parse chart.yaml: %w", err)
	}

	c := &Chart{
		sections:  f.Sections,
		groups:    make(map[string]Group, len(f.Groups)),
		roles:     f.Roles,
		order:     make(map[string]int, len(f.Groups)),
		sectionOf: make(map[string]Section, len(f.Groups)),
		ownerOf:   make(map[string]Role, len(f.Groups)),
	}

	for key, g := range f.Groups {
		g.Key = key
		c.groups[key] = g
	}

	if err := c.index(); err != nil {
		return nil, err
	}

	return c, nil
}

// index builds the lookup tables and checks the chart against itself.
func (c *Chart) index() error {
	seenSection := make(map[string]bool, len(c.sections))

	next := 0
	for _, s := range c.sections {
		if s.Key == "" {
			return fmt.Errorf("section %q has no key", s.Name)
		}
		if seenSection[s.Key] {
			return fmt.Errorf("section %q is declared twice", s.Key)
		}
		seenSection[s.Key] = true

		for _, key := range s.Groups {
			if _, ok := c.groups[key]; !ok {
				return fmt.Errorf("section %q lists group %q, which has no entry under groups", s.Key, key)
			}
			if prev, ok := c.sectionOf[key]; ok {
				return fmt.Errorf("group %q is in both section %q and section %q", key, prev.Key, s.Key)
			}
			c.sectionOf[key] = s
			c.order[key] = next
			next++
		}
	}

	for key := range c.groups {
		if _, ok := c.sectionOf[key]; !ok {
			return fmt.Errorf("group %q has a name but no section lists it", key)
		}
	}

	seenRole := make(map[string]bool, len(c.roles))
	for _, r := range c.roles {
		if seenRole[r.Key] {
			return fmt.Errorf("role %q is declared twice", r.Key)
		}
		seenRole[r.Key] = true

		if _, ok := c.groups[r.Group]; !ok {
			return fmt.Errorf("role %q is held by group %q, which has no entry under groups", r.Key, r.Group)
		}
		for _, key := range r.Owns {
			if _, ok := c.groups[key]; !ok {
				return fmt.Errorf("role %q owns group %q, which has no entry under groups", r.Key, key)
			}
			if prev, ok := c.ownerOf[key]; ok {
				return fmt.Errorf("group %q is owned by both role %q and role %q", key, prev.Key, r.Key)
			}
			c.ownerOf[key] = r
		}
	}

	return nil
}

// Order is the position of a group in the chart. Groups the chart does not mention sort
// last, in a stable order among themselves.
func (c *Chart) Order(key string) int {
	if order, ok := c.order[key]; ok {
		return order
	}
	return unlistedOrder
}

// Display returns the group's display metadata. ok is false for a group the chart does
// not mention, in which case callers should fall back to the raw key and log it -- a
// new mailing list nobody has classified yet must stay visible.
func (c *Chart) Display(key string) (Group, bool) {
	g, ok := c.groups[key]
	return g, ok
}

// SectionOf returns the section a group belongs to, or a synthetic "unsectioned"
// section for groups the chart does not mention.
func (c *Chart) SectionOf(key string) Section {
	if s, ok := c.sectionOf[key]; ok {
		return s
	}
	return Section{Key: UnsectionedKey, Name: "未分類"}
}

// Sections returns the sections in display order.
func (c *Chart) Sections() []Section {
	return c.sections
}

// OwnerOf returns the officer role responsible for a group, if any.
func (c *Chart) OwnerOf(key string) (Role, bool) {
	r, ok := c.ownerOf[key]
	return r, ok
}

// Roles returns every officer role, in declaration order.
func (c *Chart) Roles() []Role {
	return c.roles
}

// LeadershipGroups is the set of groups whose members hold an officer role.
//
// It answers "is this person an officer" and nothing more. Which of the six positions
// someone holds cannot be derived from Google -- all of them are MANAGER in every
// department group -- so anything finer needs a separate source of truth.
func (c *Chart) LeadershipGroups() []string {
	seen := make(map[string]bool, len(c.roles))
	groups := make([]string, 0, len(c.roles))
	for _, r := range c.roles {
		if !seen[r.Group] {
			seen[r.Group] = true
			groups = append(groups, r.Group)
		}
	}
	return groups
}

// Name is the display name for a group, falling back to the key itself so an
// unclassified mailing list still renders as something.
func (c *Chart) Name(key string) string {
	if g, ok := c.groups[key]; ok && strings.TrimSpace(g.Name) != "" {
		return g.Name
	}
	return key
}
