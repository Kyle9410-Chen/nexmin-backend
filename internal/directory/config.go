package directory

type Config struct {
	// The service account key is deliberately NOT a field here. It is the same
	// credential internal/googlegroup uses, so it stays in google_group and is passed
	// to NewService by main.go -- a second copy under google_sheet could be set in one
	// place and not the other, and the sync would then silently never run.

	// SpreadsheetID is the sheet holding the club's form responses, taken from its URL
	// (docs.google.com/spreadsheets/d/<id>/edit). Empty disables the startup sync
	// entirely.
	SpreadsheetID string `yaml:"spreadsheet_id"`

	// SheetName is the tab within that spreadsheet. Google Forms names the response tab
	// after the form, so this is routinely Chinese and contains spaces.
	SheetName string `yaml:"sheet_name"`

	// The columns each field lives in, as spreadsheet letters ("B", "AA").
	//
	// A form response sheet's layout is decided by the form: column A is the timestamp,
	// column B the respondent's address when the form collects it, and the answers
	// follow in question order. There is no fixed position for a name, so the mapping
	// has to be declared rather than assumed. Required once SpreadsheetID is set.
	EmailColumn    string `yaml:"email_column"`
	NameColumn     string `yaml:"name_column"`
	NicknameColumn string `yaml:"nickname_column"`

	// HeaderRows is how many leading rows to skip.
	//
	// It cannot be set to 0: configutil.Merge treats a zero int as "unset" and would
	// restore the default. That costs nothing here -- a sheet produced by Google Forms
	// always carries a header row.
	HeaderRows int `yaml:"header_rows"`
}
