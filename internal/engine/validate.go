package engine

import (
	"math"
	"strconv"
)

// Limits on user supplied identifiers and query parameters.
const (
	MaxIdentifierLength = 100
	DefaultDepth        = 10
	maxSafeInteger      = 1 << 53
)

// validateIdentifier rejects empty or oversized identifiers.
func validateIdentifier(value, field string) error {
	if value == "" {
		return Invalidf("%s must be a non-empty string", field)
	}
	if len(value) > MaxIdentifierLength {
		return Invalidf("%s must be at most %d characters", field, MaxIdentifierLength)
	}
	return nil
}

// validateSymbolName allows a conservative, shell friendly character set so
// symbols are safe to use in logs and query strings.
func validateSymbolName(name string) error {
	if err := validateIdentifier(name, "symbol"); err != nil {
		return err
	}
	for _, char := range name {
		switch {
		case char >= 'A' && char <= 'Z':
		case char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9':
		case char == '-' || char == '_' || char == '.' || char == '/':
		default:
			return Invalidf("symbol may only contain letters, digits, '-', '_', '.', and '/'")
		}
	}
	return nil
}

// AsInt64 converts a decoded JSON value to an integer, rejecting fractions and
// the imprecise range of float64 where an integer would be ambiguous.
func AsInt64(value any, field string) (int64, error) {
	switch number := value.(type) {
	case int:
		return int64(number), nil
	case int64:
		return number, nil
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, Invalidf("%s must be a number", field)
		}
		if number != math.Trunc(number) {
			return 0, Invalidf("%s must be an integer", field)
		}
		if math.Abs(number) > maxSafeInteger {
			return 0, Invalidf("%s is out of the supported integer range", field)
		}
		return int64(number), nil
	default:
		return 0, Invalidf("%s must be an integer", field)
	}
}

// ParseDepth validates an optional depth query parameter.
func ParseDepth(raw string) (int, error) {
	if raw == "" {
		return DefaultDepth, nil
	}
	depth, err := strconv.Atoi(raw)
	if err != nil {
		return 0, Invalidf("depth must be an integer")
	}
	if depth <= 0 || depth > 100 {
		return 0, Invalidf("depth must be an integer between 1 and 100")
	}
	return depth, nil
}

// ParsePage validates the optional limit and offset query parameters.
func ParsePage(limitRaw, offsetRaw string) (int, int, error) {
	limit := 100
	if limitRaw != "" {
		parsed, err := strconv.Atoi(limitRaw)
		if err != nil {
			return 0, 0, Invalidf("limit must be an integer")
		}
		if parsed <= 0 || parsed > 1000 {
			return 0, 0, Invalidf("limit must be an integer between 1 and 1000")
		}
		limit = parsed
	}
	offset := 0
	if offsetRaw != "" {
		parsed, err := strconv.Atoi(offsetRaw)
		if err != nil {
			return 0, 0, Invalidf("offset must be an integer")
		}
		if parsed < 0 {
			return 0, 0, Invalidf("offset must not be negative")
		}
		offset = parsed
	}
	return limit, offset, nil
}
