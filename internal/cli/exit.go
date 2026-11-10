package cli

import "errors"

// Process exit codes. They are part of resonate's stable contract (see
// docs/usage.md#exit-codes): CI pipelines and the resonate-fleet worker branch
// on them.
const (
	ExitOK        = 0 // the run completed and every assertion held
	ExitError     = 1 // the run could not complete (runtime failure, I/O error, ...)
	ExitUsage     = 2 // bad flags, wrong arguments, or an unreadable/invalid scenario file
	ExitAssertion = 3 // the run completed (or was aborted) and an assertion failed
)

// exitError carries a specific process exit code along with the error to print.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: ExitUsage, err: err}
}

func assertionError(err error) error {
	if err == nil {
		return nil
	}
	return &exitError{code: ExitAssertion, err: err}
}

// ExitCode maps an error returned by Execute to the process exit code: 0 for
// nil, the specific code for a usage or assertion failure, 1 for anything else.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return ExitError
}
