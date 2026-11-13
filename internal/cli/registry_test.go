package cli

import "testing"

// registry audit resolves its project through verb.RegistryAudit, so the root
// hook must not resolve one for it.
func TestRegistryAuditOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(registryAuditCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", registryAuditCmd.CommandPath())
	}
}
