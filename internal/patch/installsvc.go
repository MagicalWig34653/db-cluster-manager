// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// CollInstall holds sealed install drafts (plan and SSH credential).
const CollInstall = "patch_install"

// Sealer stores a document encrypted at rest. The manager implements it with
// its keyring (as for provisioning drafts); tests use an in-memory fake.
type Sealer interface {
	Put(coll, key string, v any) error
	Get(coll, key string, v any) (bool, error)
	Delete(coll, key string) error
}

// installDraft is sealed as one document, including the SSH credential.
type installDraft struct {
	Plan       InstallPlan          `json:"plan"`
	Credential provision.Credential `json:"credential"`
}

// Installer plans and executes native agent installs over SSH (ADR 0018
// approach: preflight, sealed credentials, digest check, explicit approval).
type Installer struct {
	Runner provision.CommandRunner
	Store  Sealer
	// Token returns a fresh one-time enrollment token for the host (the
	// manager's enroll.NewToken flow). It is passed on stdin only.
	Token func(ctx context.Context, hostID string) (string, error)
}

// Plan runs the preflight and stores the sealed draft. The returned plan has
// no credential.
func (i *Installer) Plan(ctx context.Context, req InstallRequest, cred provision.Credential) (InstallPlan, error) {
	if err := req.Validate(); err != nil {
		return InstallPlan{}, err
	}
	if cred.PrivateKey == "" || cred.KnownHosts == "" || i.Runner == nil || i.Store == nil {
		return InstallPlan{}, ErrInstallInvalid
	}
	chk, err := InstallPreflight(ctx, i.Runner, req.Host, cred)
	if err != nil {
		return InstallPlan{}, err
	}
	plan, err := BuildInstallPlan(req, chk)
	if err != nil {
		return InstallPlan{}, err
	}
	if ok, err := i.Store.Get(CollInstall, req.ID, &installDraft{}); err != nil || ok {
		return InstallPlan{}, firstErr(err, fmt.Errorf("%w: install plan ID in use", ErrConflict))
	}
	if err := i.Store.Put(CollInstall, req.ID, installDraft{Plan: plan, Credential: cred}); err != nil {
		return InstallPlan{}, err
	}
	return plan, nil
}

// Execute runs the plan with this digest, then removes the sealed draft so
// the SSH credential does not outlive the install.
func (i *Installer) Execute(ctx context.Context, id, digest string) error {
	var d installDraft
	ok, err := i.Store.Get(CollInstall, id, &d)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if digest == "" || digest != d.Plan.Digest {
		return ErrInstallDigest
	}
	if i.Token == nil {
		return errors.New("patch: no enrollment token source")
	}
	tok, err := i.Token(ctx, d.Plan.Request.Host.ID)
	if err != nil {
		return fmt.Errorf("patch: enrollment token: %w", err)
	}
	if err := ApplyInstall(ctx, i.Runner, d.Plan, digest, tok, d.Credential); err != nil {
		return err
	}
	return i.Store.Delete(CollInstall, id)
}
