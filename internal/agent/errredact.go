package agent

// redactedError is an error whose text had stored secrets taken out; the
// cause is kept for errors.As and errors.Is.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr runs an error a run returns through the session's redactor, since
// a provider's body or a tool's message can carry a stored value and the
// error reaches the terminal, the API and the log.
func (l *Loop) redactErr(err error) error {
	if err == nil || l.Recorder == nil {
		return err
	}
	red := l.Recorder.redactor()
	if red == nil {
		return err
	}
	msg := err.Error()
	if out := redactedText(red.Redact, msg); out != msg {
		return &redactedError{msg: out, err: err}
	}
	return err
}
