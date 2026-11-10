package testkit

import "strings"

// TranscriptCommand is one recorded command with its verb and, for UID forms,
// its UID set.
type TranscriptCommand struct {
	Command
	// Verb is the uppercase command name, followed by the subcommand for UID
	// forms, for example "UID STORE" or "SELECT".
	Verb string
	// Set is the first operand of a UID form as sent, for example "2" or
	// "1:3". It is empty for other commands.
	Set string
}

// ParseTranscriptCommand splits a recorded command into its verb and UID set.
func ParseTranscriptCommand(command Command) TranscriptCommand {
	parsed := TranscriptCommand{Command: command}

	fields := strings.Fields(command.Raw)
	if len(fields) < 2 {
		return parsed
	}

	parsed.Verb = strings.ToUpper(fields[1])
	if parsed.Verb != "UID" || len(fields) < 3 {
		return parsed
	}

	parsed.Verb += " " + strings.ToUpper(fields[2])
	if len(fields) > 3 {
		parsed.Set = fields[3]
	}

	return parsed
}
