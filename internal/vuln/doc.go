// SPDX-License-Identifier: AGPL-3.0-only

// Package vuln is the deterministic part of the AI-assisted vulnerability
// response (X13, ADR 0015, proposal): advisory feed polling, an SBOM-style
// inventory, a deterministic matcher, findings with VEX-like status and
// prioritisation, remediation proposals that always end in an approvable
// plan, policy modes and an audit trail.
//
// Status: groundwork tested with fakes and httptest fixtures only. No real
// feed was contacted, no real AI provider was called, nothing was built or
// rolled out. Approval of a proposal hands it to an Executor; the only
// executor that exists starts an image build through internal/imagebuild,
// which itself is fakes-only.
//
// Trust model: advisory text (summary, details, references) is untrusted
// data. It is stored with its source and fetch time, size-capped, shown to
// the admin and passed to an AI provider only as delimited data (package
// internal/ai). Nothing in an advisory or in AI output is ever executed;
// every change that reaches a plan is a typed object validated by the same
// validators as a manual change (imagebuild.WebServerSpec.Validate).
package vuln
