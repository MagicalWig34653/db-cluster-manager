// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// KeyLister is optionally implemented by a Sealer so that Installer.Sweep
// can find expired drafts.
type KeyLister interface {
	List(coll string) ([]string, error)
}

// InstallDraftTTL is how long a sealed install draft (and with it the SSH
// credential) is kept without being executed.
const InstallDraftTTL = time.Hour

// installDraft is sealed as one document, including the SSH credential.
type installDraft struct {
	Plan       InstallPlan          `json:"plan"`
	Credential provision.Credential `json:"credential"`
	Expires    time.Time            `json:"expires"`
}

// Installer plans and executes native agent installs over SSH (ADR 0018
// approach: preflight, sealed credentials, digest check, explicit approval).
type Installer struct {
	Runner provision.CommandRunner
	Store  Sealer
	// Token returns a fresh one-time enrollment token for the host (the
	// manager's enroll.NewToken flow). It is passed on stdin only.
	Token func(ctx context.Context, hostID string) (string, error)
	// TTL is the draft lifetime (default InstallDraftTTL); Now is the clock.
	TTL time.Duration
	Now func() time.Time
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Installer) ttl() time.Duration {
	if i.TTL > 0 {
		return i.TTL
	}
	return InstallDraftTTL
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
	if err := i.Store.Put(CollInstall, req.ID, installDraft{Plan: plan, Credential: cred, Expires: i.now().Add(i.ttl())}); err != nil {
		return InstallPlan{}, err
	}
	return plan, nil
}

// Sweep deletes the sealed drafts that expired and returns how many. It needs
// a Sealer that implements KeyLister; the manager calls it periodically.
func (i *Installer) Sweep(ctx context.Context) (int, error) {
	l, ok := i.Store.(KeyLister)
	if !ok || i.Store == nil {
		return 0, nil
	}
	keys, err := l.List(CollInstall)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		var d installDraft
		ok, err := i.Store.Get(CollInstall, k, &d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok && i.now().After(d.Expires) {
			if err := i.Store.Delete(CollInstall, k); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// Execute runs the plan with this digest, then removes the sealed draft so
// the SSH credential does not outlive the install. The draft is removed after
// any attempt that reached the host, successful or not (plan again with a
// fresh credential to retry); a failure before that, such as a missing
// enrollment token, keeps it until it expires.
func (i *Installer) Execute(ctx context.Context, id, digest string) error {
	var d installDraft
	ok, err := i.Store.Get(CollInstall, id, &d)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if i.now().After(d.Expires) {
		if err := i.Store.Delete(CollInstall, id); err != nil {
			return err
		}
		return fmt.Errorf("%w: install draft expired; plan again", ErrNotFound)
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
	applyErr := ApplyInstall(ctx, i.Runner, d.Plan, digest, tok, d.Credential)
	delErr := i.Store.Delete(CollInstall, id)
	if applyErr != nil {
		return errors.Join(applyErr, delErr)
	}
	return delErr
}
