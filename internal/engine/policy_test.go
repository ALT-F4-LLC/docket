package engine

import (
	"strings"
	"testing"
)

const minimalPolicyTOML = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }

[executors]
worker = { variant = "tier-a" }
`

func TestParsePolicyAccepts(t *testing.T) {
	doc, err := parsePolicy([]byte(minimalPolicyTOML))
	if err != nil {
		t.Fatalf("parsePolicy: %v", err)
	}
	if doc.Policy.Version != 2 {
		t.Errorf("Version = %d, want 2", doc.Policy.Version)
	}
	if doc.Variants["tier-a"].Model != "opus" {
		t.Errorf("variants[tier-a].model = %q, want opus", doc.Variants["tier-a"].Model)
	}
	if doc.Executors["worker"].Variant != "tier-a" {
		t.Errorf("executors[worker].variant = %q, want tier-a", doc.Executors["worker"].Variant)
	}
}

func TestParsePolicyRefusesBelowVersionFloor(t *testing.T) {
	src := strings.Replace(minimalPolicyTOML, "version = 2", "version = 1", 1)
	if _, err := parsePolicy([]byte(src)); err == nil {
		t.Error("want a refusal for a version below the floor")
	}
}

func TestParsePolicyRefusesEmptyExecutors(t *testing.T) {
	const src = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }
`
	if _, err := parsePolicy([]byte(src)); err == nil {
		t.Error("want a refusal for an empty [executors]")
	}
}

func TestParsePolicyRefusesEmptyVariants(t *testing.T) {
	const src = `
[policy]
version = 2

[executors]
worker = { variant = "tier-a" }
`
	if _, err := parsePolicy([]byte(src)); err == nil {
		t.Error("want a refusal for an empty [variants]")
	}
}

func TestParsePolicyRefusesMalformedTOML(t *testing.T) {
	if _, err := parsePolicy([]byte("this is not [valid toml")); err == nil {
		t.Error("want a refusal for malformed TOML")
	}
}
