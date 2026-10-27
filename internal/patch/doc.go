// SPDX-License-Identifier: AGPL-3.0-only

// Package patch implements general OS patch management for servers (X15,
// ADR 0023): package-manager adapters (apt, dnf, yum, zypper, apk, pacman)
// behind one interface, a host report model with diff and history, policy
// and maintenance-window evaluation, the Galera guard, the
// plan/approve/apply state machine journaled through opjournal, the vuln
// inventory bridge and the SSH install plan of the native agent mode.
//
// Everything runs through a Runner and an FS so that tests use fakes; no
// test in this package executes a package manager or touches a host. The
// default mode is report-only: nothing is changed without an approved plan.
package patch
