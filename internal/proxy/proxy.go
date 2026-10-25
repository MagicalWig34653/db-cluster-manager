// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/routing"
)

// Endpoint identifies a listener.
type Endpoint int

// Endpoints.
const (
	EndpointWrite Endpoint = iota
	EndpointRead
)

func (e Endpoint) String() string {
	if e == EndpointRead {
		return "read"
	}
	return "write"
}

// Options are the non-file dependencies of a Proxy.
type Options struct {
	// Prober overrides the prober built from the configuration (tests,
	// embedding). It then also survives reloads.
	Prober Prober
	// Status supplies the Galera sync state for "probe.sync": "galera". It
	// survives reloads. Without it (and without Prober) every node stays
	// sync-unknown and nothing is routed. internal/galeraprobe provides an
	// implementation that queries wsrep status with a monitoring account.
	Status GaleraStatusProber
	// Certs overrides the FileCertSource built from Config.TLS.
	Certs CertSource
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
	// WriterSource overrides the manager HTTP source in tests.
	WriterSource WriterSource
}

// nodeState is the proxy's view of one configured node. Guarded by Proxy.mu.
type nodeState struct {
	cfg  NodeConfig
	role routing.Role

	inflight bool
	removed  bool
	reported bool
	lastSeen time.Time
	healthy  bool
	sync     routing.SyncState
	samples  []time.Duration
	detail   string
	lastErr  string
}

// Proxy is a layer-4 database proxy with a write and an optional read
// endpoint. Create it with New, then Start, and stop it with Shutdown.
type Proxy struct {
	log          *slog.Logger
	customProber bool
	status       GaleraStatusProber
	writerSource WriterSource
	hint         WriterHint

	mu        sync.Mutex
	cfg       Config
	prober    Prober
	certs     CertSource
	tlsCfg    *tls.Config
	router    *routing.Router
	nodes     []*nodeState
	decision  routing.Decision
	decidedAt time.Time
	writer    string
	readers   []string
	rr        uint64
	changes   int
	conns     map[*tconn]struct{}
	epCount   [2]int
	perClient [2]map[string]int
	closing   bool
	lastRound time.Time

	listeners [2]net.Listener
	controlLn net.Listener
	control   *http.Server
	ctx       context.Context
	cancel    context.CancelFunc
	kick      chan struct{}

	wg      sync.WaitGroup // accept loops, probe loop, control server
	probeWG sync.WaitGroup // individual probes
	connWG  sync.WaitGroup // connection handlers
}

// New validates cfg and builds a Proxy without opening sockets.
func New(cfg Config, opts Options) (*Proxy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	policy, err := cfg.RoutingPolicy()
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p := &Proxy{
		log:          log,
		customProber: opts.Prober != nil,
		status:       opts.Status,
		writerSource: opts.WriterSource,
		cfg:          cfg,
		prober:       opts.Prober,
		certs:        opts.Certs,
		router:       routing.New(policy),
		conns:        map[*tconn]struct{}{},
		perClient:    [2]map[string]int{{}, {}},
		kick:         make(chan struct{}, 1),
	}
	if p.prober == nil {
		p.prober = proberFromConfig(cfg.Probe, p.status)
	}
	if cfg.Coordination != nil && p.writerSource == nil {
		p.writerSource = HTTPWriterSource{}
	}
	if cfg.TLS != nil {
		if p.certs == nil {
			src, err := NewFileCertSource(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("tls: %w", err)
			}
			p.certs = src
		}
		p.tlsCfg = serverTLSConfig(p.certs)
	}
	p.nodes = buildNodes(cfg.Nodes, nil)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	if cfg.Probe.Sync == SyncGalera && !p.customProber && p.status == nil {
		log.Warn("probe.sync=galera without a Galera status source (Options.Status): every node stays in sync state unknown and no connection will be routed")
	}
	return p, nil
}

func buildNodes(cfgs []NodeConfig, old []*nodeState) []*nodeState {
	out := make([]*nodeState, 0, len(cfgs))
	for _, nc := range cfgs {
		role, _ := ParseRole(nc.Role) // validated
		var ns *nodeState
		for _, o := range old {
			if o.cfg.ID == nc.ID && o.cfg.Address == nc.Address {
				ns = o
			}
		}
		if ns == nil {
			ns = &nodeState{}
		}
		ns.cfg, ns.role = nc, role
		out = append(out, ns)
	}
	for _, o := range old {
		if !slices.Contains(out, o) {
			o.removed = true
		}
	}
	return out
}

// Start opens the listeners and starts probing. The first probe round runs
// immediately; until it completes, connections are refused (no writer).
func (p *Proxy) Start() error {
	p.mu.Lock()
	cfg := p.cfg
	p.mu.Unlock()
	var lc net.ListenConfig
	listen := func(addr string) (net.Listener, error) { return lc.Listen(p.ctx, "tcp", addr) }
	var err error
	closeAll := func() {
		for _, l := range p.listeners {
			if l != nil {
				_ = l.Close()
			}
		}
		if p.controlLn != nil {
			_ = p.controlLn.Close()
		}
	}
	if p.listeners[EndpointWrite], err = listen(cfg.WriteListen); err != nil {
		return fmt.Errorf("write listener: %w", err)
	}
	if cfg.ReadListen != "" {
		if p.listeners[EndpointRead], err = listen(cfg.ReadListen); err != nil {
			closeAll()
			return fmt.Errorf("read listener: %w", err)
		}
	}
	if cfg.ControlListen != "" {
		if p.controlLn, err = listen(cfg.ControlListen); err != nil {
			closeAll()
			return fmt.Errorf("control listener: %w", err)
		}
		p.control = &http.Server{Handler: p.StatusHandler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			_ = p.control.Serve(p.controlLn)
		}()
	}
	for ep, l := range p.listeners {
		if l == nil {
			continue
		}
		p.wg.Add(1)
		go p.acceptLoop(Endpoint(ep), l)
	}
	p.wg.Add(1)
	go p.probeLoop()
	p.log.Info("proxy started", "write", p.Addr(EndpointWrite), "read", p.Addr(EndpointRead), "control", p.ControlAddr())
	return nil
}

// Addr returns the bound address of an endpoint, or "" if not listening.
func (p *Proxy) Addr(ep Endpoint) string {
	if l := p.listeners[ep]; l != nil {
		return l.Addr().String()
	}
	return ""
}

// ControlAddr returns the bound status address, or "".
func (p *Proxy) ControlAddr() string {
	if p.controlLn != nil {
		return p.controlLn.Addr().String()
	}
	return ""
}

// Shutdown stops accepting, stops probing, waits for open connections until
// ctx is done and then closes the remaining ones. It always waits for all
// goroutines of the proxy before returning.
func (p *Proxy) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	p.closing = true
	p.mu.Unlock()

	for _, l := range p.listeners {
		if l != nil {
			_ = l.Close()
		}
	}
	p.cancel()
	if p.control != nil {
		sctx, cancel := context.WithTimeout(ctx, time.Second)
		_ = p.control.Shutdown(sctx)
		cancel()
		_ = p.control.Close()
	}
	p.wg.Wait()
	p.probeWG.Wait()

	done := make(chan struct{})
	go func() { p.connWG.Wait(); close(done) }()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = ctx.Err()
		p.mu.Lock()
		n := len(p.conns)
		for c := range p.conns {
			c.close()
		}
		p.mu.Unlock()
		p.log.Info("shutdown grace expired; closed connections", "count", n)
		<-done
	}
	p.log.Info("proxy stopped")
	return err
}

// Reload applies a new configuration: nodes, roles, policy, probe settings
// and limits. Listen addresses and enabling/disabling TLS require a
// restart and are rejected. The current writer is kept if still usable;
// connections to removed nodes are drained like an old writer.
func (p *Proxy) Reload(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	policy, err := cfg.RoutingPolicy()
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.cfg
	if cfg.WriteListen != old.WriteListen || cfg.ReadListen != old.ReadListen || cfg.ControlListen != old.ControlListen {
		return errors.New("listen addresses changed: restart required")
	}
	if (cfg.TLS == nil) != (old.TLS == nil) {
		return errors.New("enabling or disabling TLS requires a restart")
	}
	if (cfg.Coordination == nil) != (old.Coordination == nil) {
		return errors.New("changing coordination mode requires a restart")
	}
	if cfg.Coordination != nil && (cfg.Coordination.ClusterID != old.Coordination.ClusterID || cfg.Coordination.ProxyID != old.Coordination.ProxyID || !reflect.DeepEqual(cfg.Coordination.ManagerURLs, old.Coordination.ManagerURLs)) {
		return errors.New("changing coordination identity or managers requires a restart")
	}
	if fs, ok := p.certs.(*FileCertSource); ok && cfg.TLS != nil {
		if fs.certFile != cfg.TLS.CertFile || fs.keyFile != cfg.TLS.KeyFile {
			nfs, err := NewFileCertSource(cfg.TLS.CertFile, cfg.TLS.KeyFile)
			if err != nil {
				return fmt.Errorf("tls: %w", err)
			}
			fs.certFile, fs.keyFile = nfs.certFile, nfs.keyFile
			fs.Set(*nfs.v.Load())
		} else if err := fs.Reload(); err != nil {
			return fmt.Errorf("tls reload (keeping previous certificate): %w", err)
		}
	}
	p.cfg = cfg
	p.router.SetPolicy(policy)
	p.nodes = buildNodes(cfg.Nodes, p.nodes)
	if !p.customProber {
		p.prober = proberFromConfig(cfg.Probe, p.status)
	}
	p.log.Info("configuration reloaded", "nodes", len(cfg.Nodes), "mode", policy.Mode.String())
	p.kickProbe()
	return nil
}

// kickProbe requests an early probe round (non-blocking, coalesced).
func (p *Proxy) kickProbe() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

func (p *Proxy) acceptLoop(ep Endpoint, l net.Listener) {
	defer p.wg.Done()
	backoff := 5 * time.Millisecond
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.log.Warn("accept failed", "endpoint", ep, "err", err)
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		p.admit(ep, c)
	}
}

func clientIP(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.IP.String()
	}
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

// reject answers with an ERR packet and closes. The write is a few bytes on
// a fresh socket, bounded by a short deadline, so it does not stall Accept.
func reject(c net.Conn, code uint16, state, msg string) {
	_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	sendInitialErr(c, code, state, msg)
	_ = c.Close()
}

func (p *Proxy) admit(ep Endpoint, c net.Conn) {
	ip := clientIP(c.RemoteAddr())
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	lim := p.cfg.Limits
	if p.epCount[ep] >= lim.MaxConnections {
		p.mu.Unlock()
		reject(c, erConCountError, "08004", "Too many connections (dbcm-proxy limit)")
		return
	}
	if lim.MaxConnectionsPerClient > 0 && p.perClient[ep][ip] >= lim.MaxConnectionsPerClient {
		p.mu.Unlock()
		reject(c, erConCountError, "08004", "Too many connections from this client (dbcm-proxy limit)")
		return
	}
	id, addr, ok := p.pickLocked(ep)
	if !ok {
		p.mu.Unlock()
		msg := "dbcm-proxy: no usable writer"
		if ep == EndpointRead {
			msg = "dbcm-proxy: no usable read node"
		}
		reject(c, erUnknownError, "HY000", msg)
		return
	}
	tc := &tconn{ep: ep, node: id, client: c, ip: ip, started: time.Now()}
	p.conns[tc] = struct{}{}
	p.epCount[ep]++
	p.perClient[ep][ip]++
	p.connWG.Add(1)
	tlsCfg, allowPlain := p.tlsCfg, p.cfg.TLS != nil && p.cfg.TLS.AllowPlaintext
	p.mu.Unlock()
	go p.serve(tc, addr, lim, tlsCfg, allowPlain)
}

// pickLocked chooses the target node for a new connection.
func (p *Proxy) pickLocked(ep Endpoint) (id, addr string, ok bool) {
	if ep == EndpointWrite {
		id = p.writer
	} else if len(p.readers) > 0 {
		id = p.readers[p.rr%uint64(len(p.readers))]
		p.rr++
	}
	if id == "" {
		return "", "", false
	}
	for _, n := range p.nodes {
		if n.cfg.ID == id {
			return id, n.cfg.Address, true
		}
	}
	return "", "", false
}

func (p *Proxy) release(tc *tconn) {
	tc.close()
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, tc)
	p.epCount[tc.ep]--
	if p.perClient[tc.ep][tc.ip]--; p.perClient[tc.ep][tc.ip] <= 0 {
		delete(p.perClient[tc.ep], tc.ip)
	}
}

func (p *Proxy) serve(tc *tconn, addr string, lim LimitsConfig, tlsCfg *tls.Config, allowPlain bool) {
	defer p.connWG.Done()
	defer p.release(tc)

	dctx, cancel := context.WithTimeout(p.ctx, lim.DialTimeout.D())
	var d net.Dialer
	b, err := d.DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		p.log.Warn("backend dial failed", "endpoint", tc.ep, "node", tc.node, "err", err)
		p.kickProbe()
		_ = tc.client.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		sendInitialErr(tc.client, erUnknownError, "HY000", "dbcm-proxy: backend "+tc.node+" unreachable")
		return
	}
	if !tc.attach(b) {
		return // closed meanwhile (drain deadline, shutdown)
	}
	client := tc.client
	if tlsCfg != nil {
		dl := time.Now().Add(lim.HandshakeTimeout.D())
		_ = client.SetDeadline(dl)
		_ = b.SetDeadline(dl)
		cc, err := mysqlTLSUpgrade(client, b, tlsCfg, allowPlain)
		if err != nil {
			p.log.Info("client handshake failed", "endpoint", tc.ep, "node", tc.node, "client", tc.ip, "err", err)
			return
		}
		client = cc
		_ = client.SetDeadline(time.Time{})
		_ = b.SetDeadline(time.Time{})
	}
	pipe(client, b, lim.IdleTimeout.D())
}

// Status-related helpers and the probe loop live in probeloop.go and status.go.
