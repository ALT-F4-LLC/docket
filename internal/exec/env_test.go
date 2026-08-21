package exec

import (
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestNetworkEnvReachesOnlyDeclaringGates is the env half.
//
// The forwarding is keyed off the DECLARATION, so the two directions matter
// equally: a gate that declared nothing must see the environment it always
// saw, and a gate that declared a need must additionally get the proxy
// variables it cannot otherwise use.
func TestNetworkEnvReachesOnlyDeclaringGates(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.internal:3128")
	t.Setenv("no_proxy", "localhost")

	t.Run("no declaration forwards nothing", func(t *testing.T) {
		env, err := BuildEnv(EnvPolicy{Gate: "tests"})
		testsupport.Must(t, err, "BuildEnv: %v", err)
		for _, name := range networkEnv {
			if envHas(env, name) {
				t.Errorf("%s reached a gate that declared no network need", name)
			}
		}
		if envHas(env, "DOCKET_GATE_NETWORK") {
			t.Error("DOCKET_GATE_NETWORK set on a gate that declared no hosts")
		}
	})

	t.Run("a declaration forwards the proxy variables", func(t *testing.T) {
		env, err := BuildEnv(EnvPolicy{
			Gate: "vuln-scan", Network: []string{"vuln.go.dev"},
		})
		testsupport.Must(t, err, "BuildEnv: %v", err)
		if !envHas(env, "HTTPS_PROXY") {
			t.Error("HTTPS_PROXY did not reach a gate that declared a network need")
		}
		if !envHas(env, "no_proxy") {
			t.Error("the lowercase spelling did not reach the child; the " +
				"convention is split and forwarding one casing serves half the tools")
		}
		if !envValueIs(env, "DOCKET_GATE_NETWORK", "vuln.go.dev") {
			t.Error("DOCKET_GATE_NETWORK does not carry the declared hosts")
		}
	})

	t.Run("an unset proxy variable is omitted, not set empty", func(t *testing.T) {
		t.Setenv("ALL_PROXY", "")
		os.Unsetenv("ALL_PROXY")
		env, err := BuildEnv(EnvPolicy{Gate: "g", Network: []string{"example.test"}})
		testsupport.Must(t, err, "BuildEnv: %v", err)
		if envHas(env, "ALL_PROXY") {
			t.Error("an unset proxy variable was invented as empty; absent and " +
				"empty are different facts")
		}
	})

	// The denied set is unaffected: declaring a network need must not become a
	// route for DOCKET_TOKEN or DOCKET_PATH.
	t.Run("the denied set still holds", func(t *testing.T) {
		t.Setenv("DOCKET_TOKEN", "secret")
		t.Setenv("DOCKET_PATH", "/somewhere")
		env, err := BuildEnv(EnvPolicy{Gate: "g", Network: []string{"example.test"}})
		testsupport.Must(t, err, "BuildEnv: %v", err)
		for _, name := range deniedEnv {
			if envHas(env, name) {
				t.Errorf("%s reached a network-declaring child", name)
			}
		}
	})
}

func envHas(env []string, name string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return true
		}
	}
	return false
}

func envValueIs(env []string, name, want string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, name+"=") {
			return strings.TrimPrefix(kv, name+"=") == want
		}
	}
	return false
}
