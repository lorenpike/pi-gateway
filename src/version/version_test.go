package version

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	got := String()
	if !regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(got) {
		t.Fatalf("String() = %q, want semantic version", got)
	}
	data, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("String() = %q, VERSION = %q", got, want)
	}
}
