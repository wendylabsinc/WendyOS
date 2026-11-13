package commands

// ReportedExitError requests exit status 1 after a command has printed its
// failure report. Returning it lets main finish analytics and cleanup without
// adding a second error message or JSON envelope.
type ReportedExitError struct{}

func (*ReportedExitError) Error() string { return "command reported a failure" }

func (*ReportedExitError) ExitCode() int { return 1 }
