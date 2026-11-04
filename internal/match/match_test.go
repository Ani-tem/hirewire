package match

import "testing"

func TestMatch(t *testing.T) {
	ravi := Filter{
		Include:   []string{"backend", "sde"},
		Exclude:   []string{"senior", "staff"},
		Locations: []string{"bengaluru", "remote"},
	}

	tests := []struct {
		name     string
		filter   Filter
		title    string
		location string
		want     bool
	}{
		// Ravi's subscription end to end.
		{"plain match", ravi, "Software Engineer, Backend", "Bengaluru", true},
		{"case does not matter", ravi, "BACKEND ENGINEER", "BENGALURU, KARNATAKA", true},
		{"exclude wins over include", ravi, "Senior Backend Engineer", "Bengaluru", false},
		{"abbreviation is excluded too", ravi, "Sr. Backend Engineer", "Bengaluru", false},
		{"no include keyword", ravi, "Product Designer", "Bengaluru", false},
		{"wrong city", ravi, "Backend Engineer", "San Francisco, CA", false},
		{"old city name", ravi, "Backend Engineer", "Bangalore, Karnataka", true},
		{"secondary location counts", ravi, "Backend Engineer", "SF; Bangalore", true},
		{"remote", ravi, "SDE II", "Remote, India", true},
		{"a phrase narrows remote", Filter{Locations: []string{"remote india"}}, "SDE", "US - Remote", false},
		{"a phrase keeps the right remote", Filter{Locations: []string{"remote india"}}, "SDE", "Remote - India", true},
		{"a city is in its country", Filter{Locations: []string{"india"}}, "SDE", "Bengaluru, Karnataka", true},
		{"old city name is in its country", Filter{Locations: []string{"india"}}, "SDE", "Gurgaon", true},
		{"a state is in its country", Filter{Locations: []string{"india"}}, "SDE", "Pune, Maharashtra", true},
		{"IND means India", Filter{Locations: []string{"india"}}, "SDE", "Bangalore, IND", true},
		{"Indiana is not India", Filter{Locations: []string{"india"}}, "SDE", "Indianapolis, IN", false},
		{"a city doesn't make remote india", Filter{Locations: []string{"remote india"}}, "SDE", "Bengaluru", false},

		// Whole words, not substrings.
		{"intern is not internal", Filter{Include: []string{"intern"}}, "Internal Tools Engineer", "", false},
		{"intern matches intern", Filter{Include: []string{"intern"}}, "Software Engineering Intern", "", true},
		{"java is not javascript", Filter{Include: []string{"java"}}, "JavaScript Developer", "", false},
		{"sde is not sdet", Filter{Include: []string{"sde"}}, "SDET", "", false},
		{"go is not google", Filter{Include: []string{"go"}}, "Google Ads Specialist", "", false},

		// Phrases and levels.
		{"phrase in order", Filter{Include: []string{"machine learning"}}, "Machine Learning Engineer", "", true},
		{"phrase out of order", Filter{Include: []string{"machine learning"}}, "Learning Platform, Machine Team", "", false},
		{"roman level equals digit", Filter{Include: []string{"sde 2"}}, "SDE II", "", true},
		{"excluding a level", Filter{Include: []string{"sde"}, Exclude: []string{"3"}}, "SDE III", "", false},

		// Symbols that are part of the word.
		{"c++", Filter{Include: []string{"c++"}}, "C++ Engineer, Trading", "", true},
		{"c# is not c++", Filter{Include: []string{"c#"}}, "C++ Engineer", "", false},
		{"staff+ is still staff", ravi, "Staff+ Software Engineer, Backend", "Remote", false},
		{"dotted names", Filter{Include: []string{"node.js"}}, "Node.js Developer", "", true},

		// Empty means anything.
		{"empty filter matches all", Filter{}, "Anything", "Anywhere", true},
		{"only excludes", Filter{Exclude: []string{"manager"}}, "Engineering Manager", "", false},
		{"blank keywords are ignored", Filter{Include: []string{"", " , "}}, "Anything", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Compile(tt.filter).Match(tt.title, tt.location); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.title, tt.location, got, tt.want)
			}
		})
	}
}
