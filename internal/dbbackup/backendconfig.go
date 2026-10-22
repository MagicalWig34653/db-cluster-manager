// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// BackendConfig is the content of the backend Swarm secret a job reads.
// Exactly one section must match Type.
type BackendConfig struct {
	Type string     `json:"type"` // "s3", "sftp" or "dir" (lab only)
	S3   *S3Section `json:"s3,omitempty"`
	SFTP *SFTPSect  `json:"sftp,omitempty"`
	Dir  string     `json:"dir,omitempty"`
}

// S3Section mirrors backup.S3Config.
type S3Section struct {
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	AddressingStyle string `json:"addressing_style,omitempty"`
	AccessKey       string `json:"access_key"`
	SecretKey       string `json:"secret_key"`
	SessionToken    string `json:"session_token,omitempty"`
	CAPEM           string `json:"ca_pem,omitempty"`
	PartSize        int64  `json:"part_size,omitempty"`
}

// SFTPSect mirrors backup.SFTPConfig. Host key verification is mandatory.
type SFTPSect struct {
	Addr               string `json:"addr"`
	User               string `json:"user"`
	Dir                string `json:"dir"`
	KnownHostsFile     string `json:"known_hosts_file,omitempty"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	Password           string `json:"password,omitempty"`
	PrivateKeyPEM      string `json:"private_key_pem,omitempty"`
	Passphrase         string `json:"passphrase,omitempty"`
}

// ParseBackendConfig decodes a backend secret strictly (unknown fields are
// errors, so a typo cannot silently disable a setting).
func ParseBackendConfig(data []byte) (BackendConfig, error) {
	var c BackendConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%w: backend config: %v", ErrInvalidRequest, err)
	}
	n := 0
	for _, set := range []bool{c.S3 != nil, c.SFTP != nil, c.Dir != ""} {
		if set {
			n++
		}
	}
	ok := n == 1 && ((c.Type == "s3" && c.S3 != nil) || (c.Type == "sftp" && c.SFTP != nil) || (c.Type == "dir" && c.Dir != ""))
	if !ok {
		return c, fmt.Errorf("%w: backend config needs exactly the section of type %q", ErrInvalidRequest, c.Type)
	}
	return c, nil
}

// Open constructs the backend. It does no network I/O.
func (c BackendConfig) Open() (backup.Backend, error) {
	switch c.Type {
	case "s3":
		s := c.S3
		return backup.NewS3Backend(backup.S3Config{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix,
			AddressingStyle: s.AddressingStyle, AccessKey: s.AccessKey, SecretKey: s.SecretKey, SessionToken: s.SessionToken,
			CAPEM: []byte(s.CAPEM), PartSize: s.PartSize})
	case "sftp":
		s := c.SFTP
		return backup.NewSFTPBackend(backup.SFTPConfig{Addr: s.Addr, User: s.User, Dir: s.Dir, KnownHostsFile: s.KnownHostsFile,
			HostKeyFingerprint: s.HostKeyFingerprint, Password: s.Password, PrivateKeyPEM: []byte(s.PrivateKeyPEM), Passphrase: []byte(s.Passphrase)})
	case "dir":
		return backup.NewDirBackend(c.Dir)
	}
	return nil, fmt.Errorf("%w: backend type %q", ErrInvalidRequest, c.Type)
}
