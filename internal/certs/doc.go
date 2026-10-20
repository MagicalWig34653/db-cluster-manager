// SPDX-License-Identifier: AGPL-3.0-only

// Package certs implements certificate management logic for requirements
// N05/N06: a manager-operated internal CA, renewal, validation of imported
// certificates and trust distribution helpers.
//
// Scope and limits (R1):
//   - Only the standard library is used.
//   - ACME DNS-01 is declared as an interface (ACMEDNS01Issuer) but not
//     implemented; it needs a library decision and is planned for R2.
//   - Revocation is a simple serial deny list. OCSP and CRL distribution are
//     future work.
//   - Private keys are never serialized in plain text by this package, except
//     through the explicit in-memory handoff methods Issued.KeyPEM and
//     Issued.TLSCertificate, which exist for configuring a live TLS listener.
//     Persisted keys use Issued.SealKey / CA.Seal (PBKDF2-SHA256 + AES-256-GCM).
package certs
