package domain

// JobExecution contains the submitted immutable invocation. It is returned only
// by an authorized job detail read, never by list or event projections.
type JobExecution struct {
	Command          JobCommand `json:"command"`
	WorkingDirectory string     `json:"workingDirectory"`
}

// JobCommand preserves executable identity and ordered argument boundaries.
// A shell executable remains a direct invocation; its arguments are not parsed.
type JobCommand struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
}
