// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
)

// Executor runs SQL against one MariaDB node. Implementations bind args as
// statement parameters. Query returns rows as strings (NULL becomes "").
type Executor interface {
	Exec(ctx context.Context, stmt string, args ...any) error
	Query(ctx context.Context, stmt string, args ...any) ([][]string, error)
}

// WriterResolver reports the node that is currently the Galera writer.
type WriterResolver interface {
	CurrentWriter(ctx context.Context) (nodeID string, err error)
}

// SecretRef identifies a generated credential.
type SecretRef struct {
	Kind string // "db-user-password"
	User string
	Host string
}

// SecretSink receives generated credentials. It is the only place a password
// leaves this package; implementations show it once and/or store it in the
// secret store. Store is called before the statement that sets the password.
type SecretSink interface {
	Store(ctx context.Context, ref SecretRef, value Secret) error
}

// Errors returned by Admin.
var (
	ErrNotWriter   = errors.New("dbadmin: executor is not connected to the current writer")
	ErrWriterMoved = errors.New("dbadmin: writer changed since the plan was created")
	ErrNoSink      = errors.New("dbadmin: plan delivers a generated secret but no SecretSink is configured")
	ErrNotFound    = errors.New("dbadmin: object does not exist")
	ErrConflict    = errors.New("dbadmin: object already exists")
	ErrConfirm     = errors.New("dbadmin: missing or wrong confirmation token")
)

// Admin plans and applies management operations.
type Admin struct {
	Exec   Executor
	NodeID string         // node the Executor is connected to
	Writer WriterResolver // required for all mutating operations
	Sink   SecretSink     // required when passwords are generated

	// ProtectedUsers extends the built-in set of accounts that cannot be
	// dropped, renamed or have grants changed (e.g. the manager's own accounts).
	ProtectedUsers []string
	Password       PasswordOptions
}

func (a *Admin) userProtected(u string) bool {
	if protectedUsers[u] {
		return true
	}
	for _, p := range a.ProtectedUsers {
		if p == u {
			return true
		}
	}
	return false
}

// requireWriter verifies that the Executor is attached to the current writer
// and returns its node ID.
func (a *Admin) requireWriter(ctx context.Context) (string, error) {
	if a.Exec == nil || a.Writer == nil {
		return "", errors.New("dbadmin: Admin needs Exec and Writer")
	}
	w, err := a.Writer.CurrentWriter(ctx)
	if err != nil {
		return "", err
	}
	if w != a.NodeID {
		return "", ErrNotWriter
	}
	return w, nil
}
