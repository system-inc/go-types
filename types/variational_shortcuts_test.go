package types_test

import (
	"strings"
	"testing"
)

// abandonedErrorFiles report an error in a body before a read that splits it: the run for every build is abandoned
// at the read, and only the runs for each group of builds that answer alike may report what they find. The array's
// length is negative on darwin and linux alone.
var abandonedErrorFiles = []fixtureFile{
	{"shared.go", everyBuild, `package fork

func Report() {
	_ = missing
	_ = N
	var _ [N - 2]int
}
`},
	{"unix.go", buildDarwin | buildLinux, "package fork\n\nconst N = 1\n"},
	{"windows.go", buildWindows, "package fork\n\nconst N = 2\n"},
}

// TestVariationalPublishesOnlyTheErrorsOfRunsThatSurvive is the projection test over abandonedErrorFiles, which
// requires each error once for each run that survives: undefined missing for darwin and linux and for windows, and
// the negative length for darwin and linux, and nothing from the run abandoned at the split.
func TestVariationalPublishesOnlyTheErrorsOfRunsThatSurvive(t *testing.T) {
	t.Parallel()
	_, published := projectAndCompareWithDiagnostics(t, abandonedErrorFiles, true)
	counts := map[string]int{}
	for _, diagnostic := range published {
		switch {
		case strings.Contains(diagnostic, "undefined: missing"):
			counts["undefined"]++
		case strings.Contains(diagnostic, "invalid array length"):
			counts["negative"]++
		}
	}
	if len(published) != 3 || counts["undefined"] != 2 || counts["negative"] != 1 {
		t.Errorf("published %d diagnostics, want undefined missing twice and the negative length once: %s",
			len(published), strings.Join(published, "; "))
	}
}
