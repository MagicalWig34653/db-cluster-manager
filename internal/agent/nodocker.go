// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
)

// errNoDocker is returned by the Docker stub of a native agent. It is never
// reached through Handle: the capability check refuses every command that
// needs Docker first. It guards against a table mistake.
var errNoDocker = errors.New("agent: this native agent has no Docker Engine API")

// noDocker is the DockerAPI of a native agent (ADR 0024).
type noDocker struct{}

var _ DockerAPI = noDocker{}

func (noDocker) CreateConfig(context.Context, ConfigSpec) (string, error) { return "", errNoDocker }
func (noDocker) RemoveConfig(context.Context, string) error               { return errNoDocker }
func (noDocker) CreateService(context.Context, ServiceSpec) (string, error) {
	return "", errNoDocker
}
func (noDocker) RemoveService(context.Context, string) error              { return errNoDocker }
func (noDocker) ScaleService(context.Context, string, uint64) error       { return errNoDocker }
func (noDocker) RemoveVolume(context.Context, string) error               { return errNoDocker }
func (noDocker) RemoveSecret(context.Context, string) error               { return errNoDocker }
func (noDocker) CreateVolume(context.Context, VolumeSpec) error           { return errNoDocker }
func (noDocker) UpdateService(context.Context, string, ServiceSpec) error { return errNoDocker }
func (noDocker) Inspect(context.Context, ObjectKind, string) (Object, error) {
	return Object{}, errNoDocker
}
func (noDocker) ListByLabel(context.Context, ObjectKind, string, string) ([]Object, error) {
	return nil, errNoDocker
}
