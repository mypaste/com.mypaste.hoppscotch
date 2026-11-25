package state

import (
	"strings"
	"testing"
)

func TestValidateAcceptsThePageDocument(t *testing.T) {
	raw := []byte(`{"version":1,"collections":[],"history":[],"environments":[]}`)
	if err := Validate(raw); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRefusesWhatIsNotAWorkspace(t *testing.T) {
	cases := map[string]string{
		"empty":     "",
		"array":     `[]`,
		"null":      `null`,
		"broken":    `{"collections":`,
		"not array": `{"collections":{}}`,
		"bad utf8":  "{\"name\":\"\xff\"}",
	}
	for name, raw := range cases {
		if err := Validate([]byte(raw)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestValidateBoundsTheDocument(t *testing.T) {
	tooBig := `{"name":"` + strings.Repeat("a", MaxBytes) + `"}`
	if err := Validate([]byte(tooBig)); err == nil {
		t.Fatal("oversized document was accepted")
	}
}
