package api

import (
	"context"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// RegistryAudit compares every project's registered workflow and schema names
// against the shared store root's config.
func (s *Service) RegistryAudit(ctx context.Context, req *connect.Request[docketv1.RegistryAuditRequest]) (*connect.Response[docketv1.RegistryAuditResponse], error) {
	return unary(ctx, s, req, decodeRegistryAudit, verb.RegistryAudit, encodeRegistryAudit)
}

func decodeRegistryAudit(msg *docketv1.RegistryAuditRequest) (verb.RegistryAuditInput, error) {
	return verb.RegistryAuditInput{Project: msg.GetProject()}, nil
}

// encodeRegistryAudit flattens the audit into one drift row per finding. A
// behind name carries its highest registered version; an orphaned name
// carries one row per registered version, marked "orphaned-retired" when
// every version is already deprecated.
func encodeRegistryAudit(out verb.RegistryAuditResult) *docketv1.RegistryAuditResponse {
	res := &docketv1.RegistryAuditResponse{}
	if out.Audit == nil {
		return res
	}
	for _, p := range out.Audit.Projects {
		for _, lag := range p.Behind {
			res.Drift = append(res.Drift, &docketv1.RegistryDrift{
				Project: p.Project, Kind: lag.Kind, Name: lag.Name,
				Version: int32(lag.RegisteredVersion), Drift: "behind",
			})
		}
		for _, orphan := range p.Orphaned {
			drift := "orphaned"
			if orphan.Retired {
				drift = "orphaned-retired"
			}
			for _, version := range orphan.Versions {
				res.Drift = append(res.Drift, &docketv1.RegistryDrift{
					Project: p.Project, Kind: orphan.Kind, Name: orphan.Name,
					Version: int32(version), Drift: drift,
				})
			}
		}
	}
	return res
}
