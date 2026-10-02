// Package engine implements the Matchbook limit order book matching engine and
// its deterministic event log replay.
package engine

import (
	"errors"
	"fmt"
)

// Sentinel errors. The HTTP layer maps them onto the public error codes
// validation_error (400), not_found (404) and conflict (409).
var (
	ErrValidation = errors.New("validation_error")
	ErrNotFound   = errors.New("not_found")
	ErrConflict   = errors.New("conflict")
)

// ValidationError reports a rejected input document.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Unwrap lets callers test with errors.Is(err, ErrValidation).
func (e *ValidationError) Unwrap() error { return ErrValidation }

// NotFoundError reports a missing resource.
type NotFoundError struct {
	Message string
}

func (e *NotFoundError) Error() string { return e.Message }

// Unwrap lets callers test with errors.Is(err, ErrNotFound).
func (e *NotFoundError) Unwrap() error { return ErrNotFound }

// ConflictError reports a request that is valid but not allowed in the current
// state, such as cancelling an order that already terminated.
type ConflictError struct {
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// Unwrap lets callers test with errors.Is(err, ErrConflict).
func (e *ConflictError) Unwrap() error { return ErrConflict }

// Invalidf returns a ValidationError with a formatted message.
func Invalidf(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// NotFoundf returns a NotFoundError with a formatted message.
func NotFoundf(format string, args ...any) error {
	return &NotFoundError{Message: fmt.Sprintf(format, args...)}
}

// Conflictf returns a ConflictError with a formatted message.
func Conflictf(format string, args ...any) error {
	return &ConflictError{Message: fmt.Sprintf(format, args...)}
}
