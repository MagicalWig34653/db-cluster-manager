// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import "errors"

// Ingestion and limit errors. They are wrapped with details; use errors.Is.
var (
	ErrInvalidLabels     = errors.New("metrics: invalid labels")
	ErrTooOld            = errors.New("metrics: sample too old")
	ErrTooNew            = errors.New("metrics: sample timestamp too far in the future")
	ErrDuplicateSample   = errors.New("metrics: duplicate timestamp with different value")
	ErrMaxSeries         = errors.New("metrics: series limit reached")
	ErrMaxLabelValues    = errors.New("metrics: label value limit reached")
	ErrBatchTooLarge     = errors.New("metrics: batch too large")
	ErrHeadFull          = errors.New("metrics: in-memory head is full")
	ErrDiskBudget        = errors.New("metrics: disk budget exhausted")
	ErrClosed            = errors.New("metrics: store is closed")
	ErrTooManySamples    = errors.New("metrics: query exceeds sample limit")
	ErrTooManySteps      = errors.New("metrics: query exceeds step limit")
	ErrBusy              = errors.New("metrics: too many concurrent queries")
	ErrNoAuthorizer      = errors.New("metrics: query requires an Authorizer")
	ErrInvalidQuery      = errors.New("metrics: invalid query")
	ErrInvalidSelector   = errors.New("metrics: invalid selector")
	ErrInvalidDashboard  = errors.New("metrics: invalid dashboard")
	ErrArchiveNotFound   = errors.New("metrics: archive block not found")
	ErrCorruptBlock      = errors.New("metrics: corrupt block")
	ErrInvalidOptions    = errors.New("metrics: invalid options")
	ErrArchiveBlockLarge = errors.New("metrics: archive block exceeds fetch limit")
)
