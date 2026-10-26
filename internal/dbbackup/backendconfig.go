// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

// BackendConfig is the content of the backend Swarm secret a job reads.
// Exactly one section must match Type.
type BackendConfig struct {
	Type string     `json:"type"` // "s3", "sftp", "local" or "dir" (alias of local, lab wording)
	S3   *S3Section `json:"s3,omitempty"`
	SFTP *SFTPSect  `json:"sftp,omitempty"`
	// Dir is the directory of a "local" or "dir" target.
	Dir string `json:"dir,omitempty"`
	// Encryption turns on client-side encryption of every object written to
	// the target (backup.EncryptedBackend). Streams are already encrypted
	// for the recovery key; this additionally protects manifests, status
	// files and transfer records and hides their content from the target.
	Encryption *EncSection `json:"encryption,omitempty"`
}

// EncSection names the data key of client-side target encryption. KeyID is
// stored in each object header; Key is the base64 (standard) 32-byte key.
// Older keys stay readable through Previous (key ID to base64 key).
type EncSection struct {
	KeyID    string            `json:"key_id"`
	Key      string            `json:"key"`
	Previous map[string]string `json:"previous,omitempty"`
}

func (e EncSection) keys() (backup.KeySource, error) {
	dec := func(id, v string) ([]byte, error) {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != 32 || id == "" || len(id) > 255 {
			return nil, fmt.Errorf("%w: encryption key %q must be a 32-byte base64 value with a key ID", ErrInvalidRequest, id)
		}
		return b, nil
	}
	ks := backup.StaticKeys{CurrentID: e.KeyID, Keys: map[string][]byte{}}
	for id, v := range e.Previous {
		b, err := dec(id, v)
		if err != nil {
			return nil, err
		}
		ks.Keys[id] = b
	}
	b, err := dec(e.KeyID, e.Key)
	if err != nil {
		return nil, err
	}
	ks.Keys[e.KeyID] = b
	return ks, nil
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
	ok := n == 1 && ((c.Type == "s3" && c.S3 != nil) || (c.Type == "sftp" && c.SFTP != nil) || ((c.Type == "dir" || c.Type == "local") && c.Dir != ""))
	if !ok {
		return c, fmt.Errorf("%w: backend config needs exactly the section of type %q", ErrInvalidRequest, c.Type)
	}
	if c.Encryption != nil {
		if _, err := c.Encryption.keys(); err != nil {
			return c, err
		}
	}
	return c, nil
}

// Open constructs the backend. It does no network I/O.
func (c BackendConfig) Open() (backup.Backend, error) {
	be, err := c.openRaw()
	if err != nil || c.Encryption == nil {
		return be, err
	}
	ks, kerr := c.Encryption.keys()
	if kerr != nil {
		return nil, kerr
	}
	return &backup.EncryptedBackend{Inner: be, Keys: ks}, nil
}

func (c BackendConfig) openRaw() (backup.Backend, error) {
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
	case "dir", "local":
		return backup.NewDirBackend(c.Dir)
	}
	return nil, fmt.Errorf("%w: backend type %q", ErrInvalidRequest, c.Type)
}
