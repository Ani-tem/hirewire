// Package data holds files compiled into the binary, so a deployed hirewire needs nothing beside it.
package data

import _ "embed"

// CompaniesCSV is companies.csv: name,ats,slug with a header row.
//
//go:embed companies.csv
var CompaniesCSV string
