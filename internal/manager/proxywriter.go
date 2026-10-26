// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"errors"
	"fmt"
	"slices"
)

// WriterChoice is the replicated authority for one set of redundant proxies.
// A pending switch first revokes the old writer. Every configured proxy must
// acknowledge that it has closed its write connections before the new writer
// is published. Members cannot be silently removed to bypass a partition.
type WriterChoice struct {
	Epoch      uint64          `json:"epoch"`
	Writer     string          `json:"writer"`
	Pending    string          `json:"pending,omitempty"`
	Members    []string        `json:"members"`
	Candidates []string        `json:"candidates"`
	Acks       map[string]bool `json:"acks,omitempty"`
}

const collProxyWriters = "proxy-writers"

var ErrWriterSwitchPending = errors.New("writer switch pending")

// ProxyWriterStore coordinates one cluster using the Raft document store.
type ProxyWriterStore struct {
	Store         *Store
	Cluster       string
	InitialWriter string
	Members       []string
	Candidates    []string
}

func (c ProxyWriterStore) validate() error {
	if c.Store == nil || c.Cluster == "" || c.InitialWriter == "" || len(c.Members) == 0 || len(c.Candidates) == 0 || !slices.Contains(c.Candidates, c.InitialWriter) {
		return errors.New("writer coordinator needs store, cluster, initial writer, members and candidates")
	}
	seen := map[string]bool{}
	for _, id := range c.Members {
		if id == "" || seen[id] {
			return fmt.Errorf("invalid or duplicate proxy ID %q", id)
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range c.Candidates {
		if id == "" || seen[id] {
			return fmt.Errorf("invalid or duplicate writer candidate %q", id)
		}
		seen[id] = true
	}
	return nil
}

// Current performs a leader read barrier. It creates the initial choice once
// and refuses a later configuration with a different membership.
func (c ProxyWriterStore) Current() (WriterChoice, error) {
	if err := c.validate(); err != nil {
		return WriterChoice{}, err
	}
	for i := 0; i < 4; i++ {
		d, ok, err := Get[WriterChoice](c.Store, collProxyWriters, c.Cluster, Consistent())
		if err != nil {
			return WriterChoice{}, err
		}
		if ok {
			want, have := slices.Clone(c.Members), slices.Clone(d.Value.Members)
			slices.Sort(want)
			slices.Sort(have)
			if !slices.Equal(want, have) {
				return WriterChoice{}, errors.New("proxy membership differs from replicated choice")
			}
			want, have = slices.Clone(c.Candidates), slices.Clone(d.Value.Candidates)
			slices.Sort(want)
			slices.Sort(have)
			if !slices.Equal(want, have) {
				return WriterChoice{}, errors.New("writer candidates differ from replicated choice")
			}
			return d.Value, nil
		}
		initial := WriterChoice{Epoch: 1, Writer: c.InitialWriter, Members: slices.Clone(c.Members), Candidates: slices.Clone(c.Candidates)}
		if _, err = Put(c.Store, collProxyWriters, c.Cluster, initial, IfVersion(0)); err == nil {
			return initial, nil
		}
		if !errors.Is(err, ErrConflict) {
			return WriterChoice{}, err
		}
	}
	return WriterChoice{}, ErrConflict
}

// Switch publishes a freeze epoch. The old writer is never published again
// after the switch starts; only an all-member acknowledgement permits commit.
func (c ProxyWriterStore) Switch(next string) (WriterChoice, error) {
	if next == "" {
		return WriterChoice{}, errors.New("next writer is required")
	}
	if !slices.Contains(c.Candidates, next) {
		return WriterChoice{}, errors.New("writer is not a configured candidate")
	}
	for i := 0; i < 8; i++ {
		if _, err := c.Current(); err != nil {
			return WriterChoice{}, err
		}
		d, _, err := Get[WriterChoice](c.Store, collProxyWriters, c.Cluster, Consistent())
		if err != nil {
			return WriterChoice{}, err
		}
		v := d.Value
		if v.Pending != "" {
			return v, ErrWriterSwitchPending
		}
		if v.Writer == next {
			return v, nil
		}
		v.Epoch++
		v.Writer, v.Pending, v.Acks = "", next, map[string]bool{}
		if _, err = Put(c.Store, collProxyWriters, c.Cluster, v, IfVersion(d.Version)); err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrConflict) {
			return WriterChoice{}, err
		}
	}
	return WriterChoice{}, ErrConflict
}

// Ack records that a member closed all write connections for the freeze
// epoch. The last acknowledgement atomically publishes the new writer.
func (c ProxyWriterStore) Ack(proxyID string, epoch uint64) (WriterChoice, error) {
	for i := 0; i < 8; i++ {
		d, ok, err := Get[WriterChoice](c.Store, collProxyWriters, c.Cluster, Consistent())
		if err != nil {
			return WriterChoice{}, err
		}
		if !ok {
			return WriterChoice{}, ErrNotFound
		}
		v := d.Value
		if !slices.Contains(v.Members, proxyID) {
			return WriterChoice{}, errors.New("unregistered proxy")
		}
		if v.Epoch != epoch || v.Pending == "" {
			return v, ErrConflict
		}
		if v.Acks[proxyID] {
			return v, nil
		}
		if v.Acks == nil {
			v.Acks = map[string]bool{}
		}
		v.Acks[proxyID] = true
		complete := true
		for _, id := range v.Members {
			if !v.Acks[id] {
				complete = false
				break
			}
		}
		if complete {
			v.Writer, v.Pending, v.Acks = v.Pending, "", nil
		}
		if _, err = Put(c.Store, collProxyWriters, c.Cluster, v, IfVersion(d.Version)); err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrConflict) {
			return WriterChoice{}, err
		}
	}
	return WriterChoice{}, ErrConflict
}
