package linkoerr

import (
	"errors"
	"fmt"
)

// errWithAttrs wraps an error with structured attributes
type errWithAttrs struct {
	err   error
	attrs []any
}

// Error implements the error interface
func (e *errWithAttrs) Error() string {
	return e.err.Error()
}

// Unwrap returns the underlying error
func (e *errWithAttrs) Unwrap() error {
	return e.err
}

// Attrs returns the attributes stored in the error
func (e *errWithAttrs) Attrs() []any {
	return e.attrs
}

// WithAttrs attaches key-value attributes to an error
func WithAttrs(err error, attrs ...any) error {
	if err == nil {
		return nil
	}
	// Ensure attrs come in pairs (key, value)
	if len(attrs)%2 != 0 {
		return fmt.Errorf("linkoerr: uneven number of attrs: %v", attrs)
	}
	return &errWithAttrs{
		err:   err,
		attrs: attrs,
	}
}

// Attrs extracts attributes from an error if it's wrapped with linkoerr
func Attrs(err error) []any {
	var e *errWithAttrs
	if errors.As(err, &e) {
		return e.Attrs()
	}
	return nil
}