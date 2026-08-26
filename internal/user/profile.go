package user

import (
	"fmt"
	"strings"
	"unicode/utf8"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
)

// Length limits for the fields a user maintains themselves.
//
// They live here beside the normalizer rather than in `validate:"max=..."` struct tags
// for the same reason googlegroup.NormalizeRole exists: the legal range is defined
// exactly once, next to the code that enforces it.
const (
	MaxNameLength       = 100
	MaxNicknameLength   = 50
	MaxDepartmentLength = 100
)

// Department is free text on purpose. NYCU students turn up with double majors,
// exchange placements and departments that were renamed mid-degree, so an enum would
// reject real answers; the club can tidy the wording later if it ever matters.

// NormalizeProfileField trims surrounding whitespace and enforces the length limit.
//
// Length is counted in runes, not bytes, so a Chinese name is not rejected for being
// three times its apparent length.
func NormalizeProfileField(field, value string, max int) (string, error) {
	trimmed := strings.TrimSpace(value)

	if utf8.RuneCountInString(trimmed) > max {
		return "", handlerutil.NewValidationError(field, value, fmt.Sprintf("%s must be at most %d characters", field, max))
	}

	return trimmed, nil
}

// NormalizeName is NormalizeProfileField plus a non-empty check.
//
// Nickname and department may be cleared by sending an empty string, but a blank name
// would leave the member unidentifiable in every roster that displays it, so clearing
// it is refused rather than silently accepted.
func NormalizeName(value string) (string, error) {
	trimmed, err := NormalizeProfileField("name", value, MaxNameLength)
	if err != nil {
		return "", err
	}

	if trimmed == "" {
		return "", handlerutil.NewValidationError("name", value, "name must not be empty")
	}

	return trimmed, nil
}
