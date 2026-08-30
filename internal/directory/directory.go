// Package directory seeds local user profiles from the club's Google Form response
// sheet, once, at startup.
//
// The problem it solves: the roster comes from the login mailing list, but the Admin
// SDK only reports names for identities inside the Workspace account, and most of the
// club signs up with @nycu.edu.tw or @gmail.com addresses. Until somebody signs in,
// this service knows nothing about them but their address, so the roster shows a column
// of bare emails. The names exist -- they were collected by the sign-up form -- just not
// anywhere the API could reach.
//
// So the sheet is read once at startup and turned into user rows for people who do not
// have one yet. It is a source of names at row-creation time and nothing more: rows
// that already exist are never touched, nothing is ever deleted, and nothing is written
// back to the sheet.
package directory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"nycu-sdc/nexmin/internal/googlegroup"
	"nycu-sdc/nexmin/internal/user"

	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// ProfileSeeder is the consumer-side view of the local user store.
//
// It is deliberately narrow and speaks plain strings: internal/user must not learn
// about Google, and this package must not need the whole store to write three columns.
type ProfileSeeder interface {
	SeedProfile(ctx context.Context, email, name, nickname string) (bool, error)
}

// LoginGroupSource lists the mailing list that gates sign-in.
type LoginGroupSource interface {
	ListMembers(ctx context.Context, groupKey string) ([]googlegroup.Member, error)
}

// Report is what one sync did, in enough detail to diagnose a missing name from the
// startup log alone.
//
// The counts partition the rows read:
//
//	RowsRead == Invalid + Duplicate + NotOnLoginGroup + Seeded + Skipped
type Report struct {
	// RowsRead is the number of data rows in the sheet, headers excluded.
	RowsRead int

	// Invalid rows carry no usable address or no name. Usually a partly filled row at
	// the end of the sheet.
	Invalid int

	// Duplicate counts rows superseded by a later row for the same address: somebody
	// filled the form more than once. The last response wins.
	Duplicate int

	// NotOnLoginGroup counts people who answered the form but are not on the mailing
	// list -- applicants who were never added, or members who have since left. They are
	// not part of the club, so they get no row here.
	NotOnLoginGroup int

	// Seeded is the number of rows actually created.
	Seeded int

	// Skipped is the number of people who already had a row, which is the steady state
	// on every restart after the first.
	Skipped int
}

type Service struct {
	logger *zap.Logger
	tracer trace.Tracer

	svc     *sheets.Service
	groups  LoginGroupSource
	seeder  ProfileSeeder
	cfg     Config
	columns columns

	// loginGroup is the mailing list that decides who is a member. Without it there is
	// nothing to match the sheet against, so the sync does not run.
	loginGroup string

	// configured reports whether the sync has everything it needs. When false SyncOnce
	// is a no-op, so the service starts and serves normally without a sheet -- the same
	// posture internal/googlegroup takes towards absent credentials.
	configured bool
}

// columns holds the resolved zero-based positions of the three fields read.
type columns struct {
	email    int
	name     int
	nickname int
}

// NewService builds a Sheets client authenticated as the service account itself.
//
// Note what is missing: there is no jwtConfig.Subject here, and that is deliberate.
// internal/googlegroup impersonates a Workspace admin through domain-wide delegation,
// and a delegation grant is matched against an exact scope list rather than merged --
// so adding the Sheets scope to it means re-saving the console entry with every scope
// spelled out, and getting that wrong breaks every Google-backed route including login.
// Reading one spreadsheet does not need any of that: sharing the sheet with the service
// account's own address is enough, and it cannot affect the delegation grant at all.
//
// An absent sheet, key or login group leaves the service unconfigured rather than
// failing, because none of them are needed to run the rest of the API. A malformed one
// is a hard error, on the same principle as googlegroup's cache_ttl.
func NewService(logger *zap.Logger, cfg Config, serviceAccountKey, loginGroup string, groups LoginGroupSource, seeder ProfileSeeder) (*Service, error) {
	s := &Service{
		logger:     logger,
		tracer:     otel.Tracer("directory/service"),
		groups:     groups,
		seeder:     seeder,
		cfg:        cfg,
		loginGroup: loginGroup,
	}

	if cfg.SpreadsheetID == "" {
		logger.Info("No directory spreadsheet configured, skipping the startup profile sync")
		return s, nil
	}

	// Past this point a sheet was asked for, so anything missing is a misconfiguration
	// worth reporting rather than a feature the operator opted out of.
	cols, err := resolveColumns(cfg)
	if err != nil {
		return nil, err
	}
	s.columns = cols

	if cfg.SheetName == "" {
		return nil, errors.New("google_sheet.sheet_name is required when a spreadsheet ID is set")
	}

	if serviceAccountKey == "" {
		logger.Warn("A directory spreadsheet is configured but no service account key is, skipping the startup profile sync")
		return s, nil
	}

	if loginGroup == "" {
		logger.Warn("A directory spreadsheet is configured but no login group is, skipping the startup profile sync")
		return s, nil
	}

	keyJSON, err := base64.StdEncoding.DecodeString(serviceAccountKey)
	if err != nil {
		return nil, fmt.Errorf("failed to base64-decode google service account key: %w", err)
	}

	jwtConfig, err := google.JWTConfigFromJSON(keyJSON, sheets.SpreadsheetsReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("failed to parse google service account key: %w", err)
	}

	ctx := context.Background()
	sheetsService, err := sheets.NewService(ctx, option.WithHTTPClient(jwtConfig.Client(ctx)))
	if err != nil {
		return nil, fmt.Errorf("failed to create google sheets service: %w", err)
	}

	s.svc = sheetsService
	s.configured = true

	logger.Info("Directory sync initialized",
		zap.String("spreadsheet_id", cfg.SpreadsheetID),
		zap.String("sheet_name", cfg.SheetName))

	return s, nil
}

// Enabled reports whether the sync has everything it needs to run. Callers use it to
// tell "nothing to do" apart from "ran and found nothing", which read identically in an
// all-zero Report.
func (s *Service) Enabled() bool { return s.configured }

// SyncOnce reads the sheet and creates a user row for every club member who does not
// have one, reporting what it did.
//
// It is safe to run repeatedly: creating a row that exists is a no-op, so a restart
// costs one read and changes nothing. An unconfigured service does nothing at all.
func (s *Service) SyncOnce(ctx context.Context) (Report, error) {
	traceCtx, span := s.tracer.Start(ctx, "SyncOnce")
	defer span.End()

	if !s.configured {
		return Report{}, nil
	}

	rows, err := s.read(traceCtx)
	if err != nil {
		span.RecordError(err)
		return Report{}, err
	}

	report, err := s.seed(traceCtx, rows)
	if err != nil {
		span.RecordError(err)
		return report, err
	}

	return report, nil
}

// seed matches parsed sheet rows against the login group and creates the rows that are
// missing. It is separate from SyncOnce so the matching and counting can be tested
// without a spreadsheet.
func (s *Service) seed(ctx context.Context, rows [][]interface{}) (Report, error) {
	logger := logutil.WithContext(ctx, s.logger)

	// The mailing list is read before anything is written, so a failure here costs
	// nothing: without it there is no way to tell a member from a stale form response,
	// and seeding the whole sheet would put strangers on the roster.
	members, err := s.groups.ListMembers(ctx, s.loginGroup)
	if err != nil {
		return Report{}, fmt.Errorf("failed to list the login group's members: %w", err)
	}

	onLoginGroup := make(map[string]struct{}, len(members))
	for _, m := range members {
		onLoginGroup[strings.ToLower(strings.TrimSpace(m.Email))] = struct{}{}
	}

	report := Report{RowsRead: len(rows)}
	entries, duplicate, invalid := parseRows(rows, s.columns)
	report.Duplicate = duplicate
	report.Invalid = invalid

	for _, e := range entries {
		if _, ok := onLoginGroup[e.email]; !ok {
			report.NotOnLoginGroup++
			continue
		}

		seeded, err := s.seeder.SeedProfile(ctx, e.email, e.name, e.nickname)
		if err != nil {
			return report, err
		}

		if seeded {
			report.Seeded++
			logger.Debug("Seeded a profile from the directory sheet", zap.String("email", e.email))
		} else {
			report.Skipped++
		}
	}

	return report, nil
}

// entry is one usable sheet row.
type entry struct {
	email    string
	name     string
	nickname string
}

// parseRows turns raw sheet rows into unique, usable entries.
//
// A repeated address keeps the last row rather than the first: somebody filling the
// form again is correcting themselves.
func parseRows(rows [][]interface{}, cols columns) (entries []entry, duplicate, invalid int) {
	seen := make(map[string]int, len(rows))

	for _, row := range rows {
		email := strings.ToLower(strings.TrimSpace(cell(row, cols.email)))
		name := strings.TrimSpace(cell(row, cols.name))
		nickname := strings.TrimSpace(cell(row, cols.nickname))

		// A row with no address cannot be matched to anyone, and one with no name
		// carries nothing worth creating a row for -- the missing name is the entire
		// problem this sync exists to fix.
		if !strings.Contains(email, "@") || name == "" {
			invalid++
			continue
		}

		if _, err := user.NormalizeProfileField("name", name, user.MaxNameLength); err != nil {
			invalid++
			continue
		}
		if _, err := user.NormalizeProfileField("nickname", nickname, user.MaxNicknameLength); err != nil {
			invalid++
			continue
		}

		e := entry{email: email, name: name, nickname: nickname}
		if index, ok := seen[email]; ok {
			entries[index] = e
			duplicate++
			continue
		}

		seen[email] = len(entries)
		entries = append(entries, e)
	}

	return entries, duplicate, invalid
}

// read fetches the sheet and returns its data rows, logging the header row so a wrong
// column mapping is visible in the startup log.
//
// Everything from column A is requested even though the fields may start later, so a
// value's position in the row is its column index and no offset arithmetic is needed.
func (s *Service) read(ctx context.Context) ([][]interface{}, error) {
	logger := logutil.WithContext(ctx, s.logger)

	last := max(s.columns.email, max(s.columns.name, s.columns.nickname))
	readRange := fmt.Sprintf("%s!A:%s", quoteSheetName(s.cfg.SheetName), columnLetter(last))

	resp, err := s.svc.Spreadsheets.Values.Get(s.cfg.SpreadsheetID, readRange).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to read the directory spreadsheet: %w", err)
	}

	headerRows := s.cfg.HeaderRows
	if headerRows < 0 {
		headerRows = 0
	}

	if len(resp.Values) > 0 {
		// Not used to decide anything -- the mapping is declared in config, not
		// detected. Logging it is what makes a mis-set column obvious at a glance
		// instead of showing up later as a roster full of the wrong field.
		header := resp.Values[0]
		logger.Info("Read the directory spreadsheet",
			zap.String("range", readRange),
			zap.Int("rows", len(resp.Values)),
			zap.String("header_email", cell(header, s.columns.email)),
			zap.String("header_name", cell(header, s.columns.name)),
			zap.String("header_nickname", cell(header, s.columns.nickname)))
	}

	if len(resp.Values) <= headerRows {
		return nil, nil
	}

	return resp.Values[headerRows:], nil
}

// cell reads one value from a row, tolerating rows shorter than the index.
//
// The Sheets API truncates trailing empty cells, so a row whose last answers were left
// blank comes back shorter than the others -- indexing it directly is the easiest way
// to panic in this package.
func cell(row []interface{}, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}

	switch v := row[index].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

func resolveColumns(cfg Config) (columns, error) {
	email, err := columnIndex("google_sheet.email_column", cfg.EmailColumn)
	if err != nil {
		return columns{}, err
	}

	name, err := columnIndex("google_sheet.name_column", cfg.NameColumn)
	if err != nil {
		return columns{}, err
	}

	nickname, err := columnIndex("google_sheet.nickname_column", cfg.NicknameColumn)
	if err != nil {
		return columns{}, err
	}

	return columns{email: email, name: name, nickname: nickname}, nil
}

// columnIndex converts a spreadsheet column letter into a zero-based index.
func columnIndex(field, letter string) (int, error) {
	trimmed := strings.ToUpper(strings.TrimSpace(letter))
	if trimmed == "" {
		return 0, fmt.Errorf("%s is required when a spreadsheet ID is set", field)
	}

	index := 0
	for _, r := range trimmed {
		if r < 'A' || r > 'Z' {
			return 0, fmt.Errorf("%s must be a column letter such as \"B\", got %q", field, letter)
		}
		index = index*26 + int(r-'A') + 1
	}

	return index - 1, nil
}

// columnLetter is columnIndex's inverse, used to bound the requested range.
func columnLetter(index int) string {
	letters := ""
	for index >= 0 {
		letters = string(rune('A'+index%26)) + letters
		index = index/26 - 1
	}

	return letters
}

// quoteSheetName wraps a tab name for A1 notation. Google Forms names response tabs
// after the form, so they routinely contain spaces and non-ASCII characters, which an
// unquoted range cannot express.
func quoteSheetName(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}
