// Tests live in the package itself so they can build a Service directly. Everything
// worth pinning here happens after the spreadsheet has been read, and reaching that
// point through NewService would mean holding real Google credentials.
package directory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/user"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap/zaptest"
)

// fakeSeeder stands in for the local user store. existing names the addresses that
// already have a row, which is what SeedProfile reports by returning false.
type fakeSeeder struct {
	existing map[string]bool
	err      error
	seeded   []entry
}

func (f *fakeSeeder) SeedProfile(_ context.Context, email, name, nickname string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}

	f.seeded = append(f.seeded, entry{email: email, name: name, nickname: nickname})

	return !f.existing[email], nil
}

type fakeGroups struct {
	members []googlegroup.Member
	err     error
	gotKey  string
}

func (f *fakeGroups) ListMembers(_ context.Context, groupKey string) ([]googlegroup.Member, error) {
	f.gotKey = groupKey
	if f.err != nil {
		return nil, f.err
	}

	return f.members, nil
}

func membersOf(emails ...string) []googlegroup.Member {
	members := make([]googlegroup.Member, 0, len(emails))
	for _, email := range emails {
		members = append(members, googlegroup.Member{Email: email, Role: googlegroup.RoleMember})
	}

	return members
}

// newTestService wires a Service with the column layout of a typical form response
// sheet: A is the timestamp Forms writes, B the collected address, then the answers.
func newTestService(t *testing.T, groups LoginGroupSource, seeder ProfileSeeder) *Service {
	t.Helper()

	return &Service{
		logger:     zaptest.NewLogger(t),
		tracer:     otel.Tracer("directory/test"),
		groups:     groups,
		seeder:     seeder,
		columns:    columns{email: 1, name: 2, nickname: 3},
		loginGroup: "general",
		configured: true,
	}
}

func row(cells ...interface{}) []interface{} { return cells }

func TestSeedCreatesRowsForMembersOnly(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com", "mei@example.com")}
	seeder := &fakeSeeder{existing: map[string]bool{"mei@example.com": true}}

	rows := [][]interface{}{
		row("2026/01/02", "kai@example.com", "Kai Chen", "Kai"),
		row("2026/01/03", "mei@example.com", "Mei Lin", "Mei"),
		// Answered the form but was never added to the mailing list: not a member, so
		// this service knows nothing about them and creates nothing.
		row("2026/01/04", "outsider@example.com", "Someone Else", ""),
	}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if groups.gotKey != "general" {
		t.Fatalf("expected the login group to be read, got %q", groups.gotKey)
	}

	if report.Seeded != 1 || report.Skipped != 1 || report.NotOnLoginGroup != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}

	if len(seeder.seeded) != 2 {
		t.Fatalf("expected only members to be written, got %+v", seeder.seeded)
	}

	if seeder.seeded[0] != (entry{email: "kai@example.com", name: "Kai Chen", nickname: "Kai"}) {
		t.Fatalf("unexpected write: %+v", seeder.seeded[0])
	}
}

// The sheet is filled in by people and the mailing list is filled in by Google, so the
// two spellings of an address routinely differ in case.
func TestSeedMatchesAddressesCaseInsensitively(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com")}
	seeder := &fakeSeeder{}

	rows := [][]interface{}{row("2026/01/02", "  Kai@Example.com ", "Kai Chen", "Kai")}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if report.Seeded != 1 {
		t.Fatalf("expected the member to match despite the casing, got %+v", report)
	}

	// The address is also lowered on the way into the store, so it matches the rows
	// the OAuth path creates.
	if seeder.seeded[0].email != "kai@example.com" {
		t.Fatalf("expected a normalized address, got %q", seeder.seeded[0].email)
	}
}

// The Sheets API truncates trailing empty cells, so a row whose last answers were left
// blank comes back shorter than the header. Indexing it directly would panic.
func TestSeedToleratesShortRows(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com", "mei@example.com")}
	seeder := &fakeSeeder{}

	rows := [][]interface{}{
		// No nickname column at all.
		row("2026/01/02", "kai@example.com", "Kai Chen"),
		// Nothing past the timestamp: nobody to attach a name to.
		row("2026/01/03"),
		row("2026/01/04", "mei@example.com", "Mei Lin", nil),
	}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if report.Seeded != 2 || report.Invalid != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}

	if seeder.seeded[0].nickname != "" || seeder.seeded[1].nickname != "" {
		t.Fatalf("expected empty nicknames, got %+v", seeder.seeded)
	}
}

// Filling the form again is how somebody corrects an answer, so the last response wins.
func TestSeedKeepsTheLastResponseForARepeatedAddress(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com")}
	seeder := &fakeSeeder{}

	rows := [][]interface{}{
		row("2026/01/02", "kai@example.com", "Kai", "typo"),
		row("2026/01/09", "kai@example.com", "Kai Chen", "Kai"),
	}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if report.Duplicate != 1 || report.Seeded != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}

	if len(seeder.seeded) != 1 || seeder.seeded[0].nickname != "Kai" {
		t.Fatalf("expected the later response to win, got %+v", seeder.seeded)
	}
}

// A row with no address cannot be matched to anyone, and one with no name carries
// nothing worth creating a row for -- an unnamed row is the problem, not the fix.
func TestSeedRejectsUnusableRows(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com")}
	seeder := &fakeSeeder{}

	rows := [][]interface{}{
		row("2026/01/02", "", "No Address", ""),
		row("2026/01/03", "not-an-address", "Malformed", ""),
		row("2026/01/04", "kai@example.com", "   ", "Kai"),
		row("2026/01/05", "kai@example.com", strings.Repeat("x", user.MaxNameLength+1), ""),
		row("2026/01/06", "kai@example.com", "Kai Chen", strings.Repeat("x", user.MaxNicknameLength+1)),
	}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	if report.Invalid != len(rows) || report.Seeded != 0 {
		t.Fatalf("unexpected report: %+v", report)
	}

	if len(seeder.seeded) != 0 {
		t.Fatalf("expected nothing to be written, got %+v", seeder.seeded)
	}
}

// The counts have to add up, or the startup log stops being a way to account for a
// missing name.
func TestReportCountsPartitionTheRowsRead(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com", "mei@example.com")}
	seeder := &fakeSeeder{existing: map[string]bool{"mei@example.com": true}}

	rows := [][]interface{}{
		row("2026/01/02", "kai@example.com", "Kai", ""),
		row("2026/01/03", "kai@example.com", "Kai Chen", "Kai"),
		row("2026/01/04", "mei@example.com", "Mei Lin", "Mei"),
		row("2026/01/05", "outsider@example.com", "Someone Else", ""),
		row("2026/01/06", "", "", ""),
	}

	report, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	sum := report.Invalid + report.Duplicate + report.NotOnLoginGroup + report.Seeded + report.Skipped
	if sum != report.RowsRead {
		t.Fatalf("counts do not partition %d rows: %+v", report.RowsRead, report)
	}
}

// Without the mailing list there is no way to tell a member from a stale form response,
// and seeding the sheet wholesale would put strangers on the roster. So a failure there
// has to stop the sync before the first write, not fall back to trusting the sheet.
func TestSeedWritesNothingWhenTheLoginGroupCannotBeRead(t *testing.T) {
	groups := &fakeGroups{err: errors.New("google is unreachable")}
	seeder := &fakeSeeder{}

	rows := [][]interface{}{row("2026/01/02", "kai@example.com", "Kai Chen", "Kai")}

	_, err := newTestService(t, groups, seeder).seed(t.Context(), rows)
	if err == nil {
		t.Fatal("expected an error when the login group cannot be read")
	}

	if len(seeder.seeded) != 0 {
		t.Fatalf("expected nothing to be written, got %+v", seeder.seeded)
	}
}

// Google credentials are optional everywhere else in this service, and a missing sheet
// must not be different: the sync simply does not run.
func TestSyncOnceIsANoOpWhenUnconfigured(t *testing.T) {
	groups := &fakeGroups{members: membersOf("kai@example.com")}
	seeder := &fakeSeeder{}

	s := newTestService(t, groups, seeder)
	s.configured = false

	report, err := s.SyncOnce(t.Context())
	if err != nil {
		t.Fatalf("an unconfigured sync must not fail: %v", err)
	}

	if report != (Report{}) {
		t.Fatalf("expected an empty report, got %+v", report)
	}

	if groups.gotKey != "" || len(seeder.seeded) != 0 {
		t.Fatal("an unconfigured sync must not touch Google or the store")
	}
}

func TestNewServiceStaysUnconfiguredWithoutASpreadsheet(t *testing.T) {
	s, err := NewService(zaptest.NewLogger(t), Config{}, "", "general", &fakeGroups{}, &fakeSeeder{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if s.configured {
		t.Fatal("expected an unconfigured service")
	}
}

// The column letters have no defaults, because a form's layout decides them. Asking for
// a sheet without saying where the fields are is a misconfiguration, and reading the
// wrong column silently would be worse than refusing.
func TestNewServiceRejectsAnIncompleteColumnMapping(t *testing.T) {
	cfg := Config{SpreadsheetID: "sheet-id", SheetName: "Responses", EmailColumn: "B"}

	if _, err := NewService(zaptest.NewLogger(t), cfg, "", "general", &fakeGroups{}, &fakeSeeder{}); err == nil {
		t.Fatal("expected an error when a column letter is missing")
	}
}

func TestColumnIndex(t *testing.T) {
	cases := map[string]int{"A": 0, "b": 1, " C ": 2, "Z": 25, "AA": 26, "AB": 27, "BA": 52}

	for letter, want := range cases {
		got, err := columnIndex("field", letter)
		if err != nil {
			t.Fatalf("columnIndex(%q): %v", letter, err)
		}
		if got != want {
			t.Fatalf("columnIndex(%q) = %d, want %d", letter, got, want)
		}

		if back := columnLetter(got); back != strings.ToUpper(strings.TrimSpace(letter)) {
			t.Fatalf("columnLetter(%d) = %q, want %q", got, back, letter)
		}
	}

	for _, bad := range []string{"", "1", "A1", "$"} {
		if _, err := columnIndex("field", bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

// Google Forms names the response tab after the form, so the names carry spaces and
// non-ASCII characters that an unquoted A1 range cannot express.
func TestQuoteSheetName(t *testing.T) {
	if got := quoteSheetName("表單回應 1"); got != "'表單回應 1'" {
		t.Fatalf("unexpected quoting: %s", got)
	}

	if got := quoteSheetName("Kai's sheet"); got != "'Kai''s sheet'" {
		t.Fatalf("expected the inner quote to be doubled, got %s", got)
	}
}
