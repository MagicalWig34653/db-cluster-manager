// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DefaultAdminTimeout bounds one Admin.Exec call (connect plus all
// statements) when Admin.Timeout is zero.
const DefaultAdminTimeout = 30 * time.Second

// Admin runs administrative statements on the local node over its unix
// socket. It implements the agent's exec-sql port: the agent renders the
// statements from allow-listed templates (internal/dbadmin) and Admin only
// executes them, one at a time, on a single connection, without client-side
// interpolation and without multi-statement support.
type Admin struct {
	Timeout time.Duration
}

// Exec connects to socket as user and runs statements in order, stopping at
// the first failure. Errors name the failing position and the redacted
// server message, never the statement text or the password.
func (a Admin) Exec(ctx context.Context, socket, user, password string, statements []string) error {
	if len(statements) == 0 {
		return errors.New("galeraprobe: no statements")
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultAdminTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	mc := mysql.NewConfig()
	mc.User, mc.Passwd = user, password
	mc.Net, mc.Addr = "unix", socket
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = timeout, timeout, timeout
	mc.AllowNativePasswords = true
	mc.AllowCleartextPasswords = false
	mc.AllowOldPasswords = false
	mc.MultiStatements = false
	mc.InterpolateParams = false
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		return &probeError{msg: "galeraprobe: admin connector: " + redact(err.Error(), password)}
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	c, err := db.Conn(ctx)
	if err != nil {
		return (&Prober{}).classify(socket, err, password)
	}
	defer c.Close()
	for i, s := range statements {
		if _, err := c.ExecContext(ctx, s); err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) && (me.Number == 1045 || me.Number == 1698) {
				return (&Prober{}).classify(socket, err, password)
			}
			return &probeError{msg: fmt.Sprintf("galeraprobe: %s: statement %d of %d failed: %s", socket, i+1, len(statements), redact(err.Error(), password))}
		}
	}
	return nil
}
