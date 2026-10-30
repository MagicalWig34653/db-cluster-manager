// SPDX-License-Identifier: AGPL-3.0-only

package agent

import "syscall"

// statfsFree returns the bytes available to unprivileged writers on the
// file system holding dir.
func statfsFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
