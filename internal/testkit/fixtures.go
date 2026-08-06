package testkit

import _ "embed"

var (
	//go:embed testdata/welcome.eml
	welcomeMIME string

	//go:embed testdata/status-report.eml
	statusReportMIME string
)

// SyntheticMIMESeeds returns only invented MIME messages addressed within reserved .test domains.
func SyntheticMIMESeeds() []string {
	return []string{welcomeMIME, statusReportMIME}
}
