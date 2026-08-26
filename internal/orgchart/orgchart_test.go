package orgchart

import (
	"strings"
	"testing"
)

func load(t *testing.T) *Chart {
	t.Helper()

	c, err := Load()
	if err != nil {
		t.Fatalf("the committed chart.yaml must load: %v", err)
	}
	return c
}

// chart.yaml is validated at startup, so a broken edit has to fail here rather than in
// production.
func TestCommittedChartIsValid(t *testing.T) {
	c := load(t)

	if len(c.Sections()) == 0 {
		t.Fatal("expected at least one section")
	}
	if len(c.Roles()) == 0 {
		t.Fatal("expected the officer roles to be declared")
	}
}

func TestOrderFollowsSectionThenPosition(t *testing.T) {
	c := load(t)

	// general is in the first section, departments come after governance.
	if c.Order("general") >= c.Order("presidents") {
		t.Fatal("general must sort before governance")
	}
	if c.Order("presidents") >= c.Order("administration") {
		t.Fatal("governance must sort before the departments")
	}
	// Departments, then the three technical arms in their own order, then the courses.
	for _, pair := range [][2]string{
		{"administration", "infrastructure"},
		{"infrastructure", "core-system"},
		{"core-system", "hpc"},
		{"hpc", "hpc-training"},
		{"hpc-training", "react-credit-program-spring-2026"},
		{"react-credit-program-spring-2026", "welcome-home"},
		{"welcome-home", "info"},
	} {
		if c.Order(pair[0]) >= c.Order(pair[1]) {
			t.Fatalf("%s must sort before %s", pair[0], pair[1])
		}
	}
}

// The classification is only useful if it is exhaustive: a group nobody assigned falls
// into "unsectioned", which is a fallback for newly created lists, not a parking spot.
func TestEverySectionIsNonEmpty(t *testing.T) {
	c := load(t)

	for _, s := range c.Sections() {
		if len(s.Groups) == 0 {
			t.Errorf("section %q lists no groups", s.Key)
		}
	}
}

// A mailing list nobody has classified yet must still appear, at the end.
func TestUnlistedGroupSortsLastAndKeepsItsKey(t *testing.T) {
	c := load(t)

	if c.Order("brand-new-group") <= c.Order("classroom_teachers") {
		t.Fatal("an unlisted group must sort after every listed one")
	}
	if got := c.Name("brand-new-group"); got != "brand-new-group" {
		t.Fatalf("got name %q, want the key as fallback", got)
	}
	if _, ok := c.Display("brand-new-group"); ok {
		t.Fatal("Display must report an unlisted group as unknown")
	}
	if got := c.SectionOf("brand-new-group"); got.Key != UnsectionedKey {
		t.Fatalf("got section %q, want %q", got.Key, UnsectionedKey)
	}
}

func TestOwnerOf(t *testing.T) {
	c := load(t)

	role, ok := c.OwnerOf("engineering")
	if !ok {
		t.Fatal("engineering must have an owning role")
	}
	if role.Key != "engineering_vp" {
		t.Fatalf("got role %q, want engineering_vp", role.Key)
	}

	if _, ok := c.OwnerOf("welcome-home"); ok {
		t.Fatal("welcome-home is owned by no officer role")
	}
}

func TestLeadershipGroups(t *testing.T) {
	c := load(t)

	got := c.LeadershipGroups()
	if len(got) != 1 || got[0] != "presidents" {
		t.Fatalf("got %v, want [presidents]", got)
	}
}

func TestSystemSectionIsHidden(t *testing.T) {
	c := load(t)

	if !c.SectionOf("info").Hidden {
		t.Fatal("the system section must be hidden from profile pages")
	}
	if c.SectionOf("general").Hidden {
		t.Fatal("general must not be hidden")
	}
}

// index is what stops a typo from reaching production, so each rule it enforces needs a
// case proving it fires.
func TestIndexRejectsInconsistentCharts(t *testing.T) {
	tests := []struct {
		name  string
		chart file
		want  string
	}{
		{
			name: "section lists an unnamed group",
			chart: file{
				Sections: []Section{{Key: "a", Groups: []string{"ghost"}}},
				Groups:   map[string]Group{},
			},
			want: "no entry under groups",
		},
		{
			name: "group in two sections",
			chart: file{
				Sections: []Section{{Key: "a", Groups: []string{"x"}}, {Key: "b", Groups: []string{"x"}}},
				Groups:   map[string]Group{"x": {Name: "X"}},
			},
			want: "in both section",
		},
		{
			name: "named group no section lists",
			chart: file{
				Sections: []Section{{Key: "a", Groups: []string{"x"}}},
				Groups:   map[string]Group{"x": {Name: "X"}, "orphan": {Name: "O"}},
			},
			want: "no section lists it",
		},
		{
			name: "duplicate section key",
			chart: file{
				Sections: []Section{{Key: "a"}, {Key: "a"}},
				Groups:   map[string]Group{},
			},
			want: "declared twice",
		},
		{
			name: "role owns an unnamed group",
			chart: file{
				Sections: []Section{{Key: "a", Groups: []string{"x"}}},
				Groups:   map[string]Group{"x": {Name: "X"}},
				Roles:    []Role{{Key: "r", Group: "x", Owns: []string{"ghost"}}},
			},
			want: "no entry under groups",
		},
		{
			name: "two roles own the same group",
			chart: file{
				Sections: []Section{{Key: "a", Groups: []string{"x", "y"}}},
				Groups:   map[string]Group{"x": {Name: "X"}, "y": {Name: "Y"}},
				Roles: []Role{
					{Key: "r1", Group: "x", Owns: []string{"y"}},
					{Key: "r2", Group: "x", Owns: []string{"y"}},
				},
			},
			want: "owned by both",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Chart{
				sections:  tt.chart.Sections,
				groups:    make(map[string]Group, len(tt.chart.Groups)),
				roles:     tt.chart.Roles,
				order:     map[string]int{},
				sectionOf: map[string]Section{},
				ownerOf:   map[string]Role{},
			}
			for k, g := range tt.chart.Groups {
				g.Key = k
				c.groups[k] = g
			}

			err := c.index()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
