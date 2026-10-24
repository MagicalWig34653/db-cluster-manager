// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TypeContainer is the registry name of the generic container type (X06).
const TypeContainer = "container"

var (
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	identRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// secretEnvRe matches names of variables that normally hold secrets.
	// Their value must come from a Swarm secret through the "_FILE"
	// convention; a literal value would be stored in the spec, shown in the
	// preview and sent in commands.
	secretEnvRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIAL)S?$`)
)

func validName(s string) bool { return nameRe.MatchString(s) }

// EnvVar is a literal environment variable. Secrets are not env values:
// reference a Swarm secret and use the "<NAME>_FILE" convention.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Volume is a managed local volume: a Docker volume bound to a directory
// below the agent's volume base, with an identity marker (A10). Host paths
// cannot be mounted.
type Volume struct {
	// Name is unique in the service; the Docker volume is
	// dbcm-svc-<service>-<name>.
	Name     string `json:"name"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// Placement restricts where tasks run. A service with volumes needs HostID.
type Placement struct {
	// HostID pins the service to one Swarm node ID.
	HostID string `json:"hostId,omitempty"`
	// Role is "manager" or "worker"; empty is any.
	Role string `json:"role,omitempty"`
	// NodeLabels are required node labels (key -> value).
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
}

// ContainerSpec is the desired state of a generic Docker service.
type ContainerSpec struct {
	// Image must be pinned: a tag other than "latest", or a digest.
	Image string `json:"image"`
	// Replicas defaults to 1.
	Replicas int      `json:"replicas,omitempty"`
	Env      []EnvVar `json:"env,omitempty"`
	Command  []string `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	// User overrides the image user ("uid:gid").
	User     string   `json:"user,omitempty"`
	Ports    []Port   `json:"ports,omitempty"`
	Volumes  []Volume `json:"volumes,omitempty"`
	Networks []string `json:"networks,omitempty"`
	// Secrets are names of Swarm secrets the operator created; they are
	// mounted below /run/secrets.
	Secrets   []string  `json:"secrets,omitempty"`
	Resources Resources `json:"resources"`
	Placement Placement `json:"placement"`
	// Restart is "any" (default), "on-failure" or "none".
	Restart string `json:"restart,omitempty"`
	// StopGraceSeconds is the time between SIGTERM and SIGKILL (0: Docker
	// default).
	StopGraceSeconds int `json:"stopGraceSeconds,omitempty"`
}

// Container is the generic container service type.
type Container struct{}

var _ Type = Container{}

// Name implements Type.
func (Container) Name() string { return TypeContainer }

func decodeContainer(raw json.RawMessage) (ContainerSpec, error) {
	var c ContainerSpec
	if err := decodeStrict(raw, &c); err != nil {
		return ContainerSpec{}, err
	}
	return c, c.validate()
}

// Validate implements Type.
func (Container) Validate(spec json.RawMessage) error {
	_, err := decodeContainer(spec)
	return err
}

func validImage(img string) error {
	if img == "" || len(img) > 255 || strings.ContainsAny(img, " \t\r\n\x00") {
		return fmt.Errorf("image %q must be a non-empty reference without whitespace", img)
	}
	if strings.Contains(img, "@sha256:") {
		return nil
	}
	last := img[strings.LastIndex(img, "/")+1:]
	i := strings.LastIndex(last, ":")
	if i < 0 || i == len(last)-1 {
		return fmt.Errorf("image %q must be pinned with a tag or a digest", img)
	}
	if last[i+1:] == "latest" {
		return fmt.Errorf("image %q: the tag latest is not pinned", img)
	}
	return nil
}

func (c ContainerSpec) validate() error {
	var p problems
	if err := validImage(c.Image); err != nil {
		p.addf("%v", err)
	}
	if c.Replicas < 0 || c.Replicas > 50 {
		p.addf("replicas %d out of range 0..50", c.Replicas)
	}
	seenEnv := map[string]bool{}
	for _, e := range c.Env {
		switch {
		case !envNameRe.MatchString(e.Name):
			p.addf("env name %q is invalid", e.Name)
		case seenEnv[e.Name]:
			p.addf("env %s set twice", e.Name)
		case secretEnvRe.MatchString(e.Name):
			p.addf("env %s looks like a secret: use a Swarm secret and %s_FILE instead of a literal value", e.Name, e.Name)
		case strings.ContainsAny(e.Value, "\x00\r\n"):
			p.addf("env %s value contains a control character", e.Name)
		}
		seenEnv[e.Name] = true
	}
	for _, s := range slices.Concat(c.Command, c.Args) {
		if strings.ContainsRune(s, 0) {
			p.addf("command or argument contains a NUL byte")
			break
		}
	}
	if c.User != "" && !regexp.MustCompile(`^[0-9]{1,10}(:[0-9]{1,10})?$`).MatchString(c.User) {
		p.addf("user %q must be uid or uid:gid", c.User)
	}
	c.validatePorts(&p)
	c.validateVolumes(&p)
	for _, refs := range []struct {
		what string
		list []string
	}{{"network", c.Networks}, {"secret", c.Secrets}} {
		seen := map[string]bool{}
		for _, r := range refs.list {
			if !identRe.MatchString(r) {
				p.addf("%s name %q is invalid", refs.what, r)
			}
			if seen[r] {
				p.addf("%s %q listed twice", refs.what, r)
			}
			seen[r] = true
		}
	}
	for _, v := range []int64{c.Resources.Limits.MilliCPU, c.Resources.Limits.MemoryBytes, c.Resources.Reservations.MilliCPU, c.Resources.Reservations.MemoryBytes} {
		if v < 0 {
			p.addf("negative resource value")
			break
		}
	}
	if l, r := c.Resources.Limits, c.Resources.Reservations; (l.MilliCPU > 0 && r.MilliCPU > l.MilliCPU) || (l.MemoryBytes > 0 && r.MemoryBytes > l.MemoryBytes) {
		p.addf("a reservation is above its limit")
	}
	switch c.Restart {
	case "", RestartAny, RestartOnFailure, RestartNone:
	default:
		p.addf("restart %q (want any, on-failure or none)", c.Restart)
	}
	if c.StopGraceSeconds < 0 || c.StopGraceSeconds > 3600 {
		p.addf("stopGraceSeconds %d out of range 0..3600", c.StopGraceSeconds)
	}
	c.validatePlacement(&p)
	return p.err()
}

func (c ContainerSpec) validatePorts(p *problems) {
	type key struct {
		port  int
		proto string
	}
	seen := map[key]bool{}
	for _, pt := range c.Ports {
		proto := pt.Protocol
		if proto == "" {
			proto = "tcp"
		}
		switch {
		case pt.Target < 1 || pt.Target > 65535:
			p.addf("port target %d out of range", pt.Target)
		case pt.Published < 0 || pt.Published > 65535:
			p.addf("published port %d out of range", pt.Published)
		case proto != "tcp" && proto != "udp":
			p.addf("port %d protocol %q (want tcp or udp)", pt.Target, pt.Protocol)
		case pt.Mode != "" && pt.Mode != "ingress" && pt.Mode != "host":
			p.addf("port %d mode %q (want ingress or host)", pt.Target, pt.Mode)
		case pt.Published == 0 && pt.Mode != "":
			p.addf("port %d: mode needs a published port", pt.Target)
		}
		if pt.Published > 0 {
			k := key{pt.Published, proto}
			if seen[k] {
				p.addf("published port %d/%s used twice", pt.Published, proto)
			}
			seen[k] = true
		}
	}
}

func (c ContainerSpec) validateVolumes(p *problems) {
	names, targets := map[string]bool{}, map[string]bool{}
	for _, v := range c.Volumes {
		switch {
		case !nameRe.MatchString(v.Name):
			p.addf("volume name %q must match %s", v.Name, nameRe)
		case names[v.Name]:
			p.addf("volume %q listed twice", v.Name)
		}
		names[v.Name] = true
		t := v.Target
		switch {
		case !path.IsAbs(t) || path.Clean(t) != t || t == "/":
			p.addf("volume %q target %q must be a clean absolute path below /", v.Name, t)
		case t == SecretsDir || strings.HasPrefix(t, SecretsDir+"/"):
			p.addf("volume %q target %q is inside %s", v.Name, t, SecretsDir)
		case targets[t]:
			p.addf("volume target %q used twice", t)
		}
		targets[t] = true
	}
	if len(c.Volumes) > 0 {
		// Local storage ties the data to one host (ARCHITEKTUR: local
		// storage and recovery): a volume needs a pinned host and one task.
		if c.Placement.HostID == "" {
			p.addf("a service with volumes must be pinned to a host (placement.hostId)")
		}
		if c.replicas() != 1 {
			p.addf("a service with local volumes runs exactly one replica")
		}
	}
}

func (c ContainerSpec) validatePlacement(p *problems) {
	pl := c.Placement
	switch pl.Role {
	case "", "manager", "worker":
	default:
		p.addf("placement role %q (want manager or worker)", pl.Role)
	}
	if pl.HostID != "" && !identRe.MatchString(pl.HostID) {
		p.addf("placement host ID %q is invalid", pl.HostID)
	}
	for k, v := range pl.NodeLabels {
		if !identRe.MatchString(k) || strings.ContainsAny(v, "=\x00\r\n \t") || v == "" {
			p.addf("placement node label %q=%q is invalid", k, v)
		}
	}
}

func (c ContainerSpec) replicas() int {
	if c.Replicas == 0 {
		return 1
	}
	return c.Replicas
}

func (c ContainerSpec) constraints() []string {
	var out []string
	if c.Placement.HostID != "" {
		out = append(out, "node.id=="+c.Placement.HostID)
	}
	if c.Placement.Role != "" {
		out = append(out, "node.role=="+c.Placement.Role)
	}
	for _, k := range slices.Sorted(maps.Keys(c.Placement.NodeLabels)) {
		out = append(out, "node.labels."+k+"=="+c.Placement.NodeLabels[k])
	}
	return out
}

// serviceSpec builds the Swarm service of the container.
func (c ContainerSpec) serviceSpec(ctx Context, typ string) ServiceSpec {
	s := ServiceSpec{
		Name:  SwarmServiceName(ctx.Name),
		Image: c.Image,
		Labels: map[string]string{
			LabelCluster: ctx.ServiceID, LabelServiceType: typ, LabelServiceName: ctx.Name,
		},
		Command:          slices.Clone(c.Command),
		Args:             slices.Clone(c.Args),
		User:             c.User,
		Constraints:      c.constraints(),
		Replicas:         uint64(c.replicas()),
		Networks:         sortedUnique(c.Networks),
		Secrets:          sortedUnique(c.Secrets),
		RestartCondition: c.Restart,
		StopGracePeriod:  time.Duration(c.StopGraceSeconds) * time.Second,
		Resources:        c.Resources,
	}
	env := slices.Clone(c.Env)
	slices.SortFunc(env, func(a, b EnvVar) int { return strings.Compare(a.Name, b.Name) })
	for _, e := range env {
		s.Env = append(s.Env, e.Name+"="+e.Value)
	}
	vols := slices.Clone(c.Volumes)
	slices.SortFunc(vols, func(a, b Volume) int { return strings.Compare(a.Name, b.Name) })
	for _, v := range vols {
		s.Mounts = append(s.Mounts, Mount{Type: "volume", Source: VolumeName(ctx.Name, v.Name), Target: v.Target, ReadOnly: v.ReadOnly})
	}
	for _, pt := range c.Ports {
		if pt.Protocol == "" {
			pt.Protocol = "tcp"
		}
		if pt.Published > 0 && pt.Mode == "" {
			pt.Mode = "ingress"
		}
		s.Ports = append(s.Ports, pt)
	}
	slices.SortFunc(s.Ports, func(a, b Port) int {
		return strings.Compare(fmt.Sprintf("%05d/%05d/%s", a.Published, a.Target, a.Protocol), fmt.Sprintf("%05d/%05d/%s", b.Published, b.Target, b.Protocol))
	})
	return s
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func checkHost(c ContainerSpec, hosts []Host) error {
	id := c.Placement.HostID
	if id == "" || hosts == nil {
		return nil
	}
	i := slices.IndexFunc(hosts, func(h Host) bool { return h.ID == id })
	switch {
	case i < 0:
		return fmt.Errorf("%w: placement host %q is not an enrolled host of the Swarm", ErrInvalidSpec, id)
	case !hosts[i].Ready || !hosts[i].Active:
		return fmt.Errorf("%w: placement host %q is not ready and active", ErrInvalidSpec, id)
	}
	return nil
}

// Plan implements Type.
func (Container) Plan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	c, err := decodeContainer(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	return c.plan(in, TypeContainer)
}

// plan is the create plan shared by the types built on a container.
func (c ContainerSpec) plan(in PlanInput, typ string) (Plan, error) {
	if err := checkHost(c, in.Hosts); err != nil {
		return Plan{}, err
	}
	spec := c.serviceSpec(in.Context, typ)
	p := Plan{Kind: PlanCreate, Type: typ, ServiceID: in.ServiceID, Name: in.Name, RequiresApproval: true}
	var volSteps []string
	vols := slices.Clone(c.Volumes)
	slices.SortFunc(vols, func(a, b Volume) int { return strings.Compare(a.Name, b.Name) })
	add := func(s Step) string {
		s.ID = fmt.Sprintf("%02d-%s", len(p.Steps)+1, s.ID)
		p.Steps = append(p.Steps, s)
		return s.ID
	}
	for _, v := range vols {
		vn, uuid := VolumeName(in.Name, v.Name), VolumeIdentity(in.ServiceID, v.Name)
		cv := add(Step{ID: "create-volume-" + v.Name, Kind: StepCreateVolume, HostID: c.Placement.HostID, Volume: vn, NodeUUID: uuid,
			Note: fmt.Sprintf("create the local volume %s on host %s below the agent's volume base (no identity marker yet)", vn, c.Placement.HostID)})
		volSteps = append(volSteps, add(Step{ID: "init-identity-" + v.Name, Kind: StepInitIdentity, HostID: c.Placement.HostID, Volume: vn, NodeUUID: uuid,
			After: []string{cv},
			Note:  fmt.Sprintf("write the identity marker %s for service %s on the new volume %s; a directory with content or another identity is refused", MarkerFileName, in.ServiceID, vn)}))
	}
	add(Step{ID: "create-service-" + in.Name, Kind: StepCreateService, HostID: "", After: volSteps, Spec: &spec,
		Note: describeService(spec)})
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return Plan{}, err
	}
	p.Artifacts = append(p.Artifacts, Artifact{Name: "swarm-service.json", MediaType: "application/json", Content: string(raw) + "\n"})
	if spec.Resources.Limits.MemoryBytes == 0 {
		p.Warnings = append(p.Warnings, "no memory limit is set; a runaway container can use all memory of its host")
	}
	if !strings.Contains(spec.Image, "@sha256:") {
		p.Warnings = append(p.Warnings, "the image is pinned by tag only; a tag can move, prefer a digest")
	}
	return p, nil
}

func describeService(s ServiceSpec) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("create the Swarm service %s from image %s with %d replica(s)", s.Name, s.Image, s.Replicas))
	if len(s.Constraints) > 0 {
		parts = append(parts, "placement "+strings.Join(s.Constraints, ", "))
	}
	if len(s.Mounts) > 0 {
		var m []string
		for _, x := range s.Mounts {
			m = append(m, x.Source+":"+x.Target)
		}
		parts = append(parts, "volumes "+strings.Join(m, ", "))
	}
	if len(s.Networks) > 0 {
		parts = append(parts, "networks "+strings.Join(s.Networks, ", "))
	}
	if len(s.Secrets) > 0 {
		parts = append(parts, "secrets "+strings.Join(s.Secrets, ", ")+" (mounted below "+SecretsDir+")")
	}
	return strings.Join(parts, "; ")
}

// DeletePlan implements Type. The service is removed through the agent's
// ownership guard (a missing service counts as removed); volumes are removed
// only on request and only after their identity marker matched. A host
// directory below the volume base is not deleted by Docker.
func (Container) DeletePlan(in PlanInput) (Plan, error) {
	if err := in.check(); err != nil {
		return Plan{}, err
	}
	c, err := decodeContainer(in.Spec)
	if err != nil {
		return Plan{}, err
	}
	return c.deletePlan(in, TypeContainer), nil
}

func (c ContainerSpec) deletePlan(in PlanInput, typ string) Plan {
	p := Plan{Kind: PlanDelete, Type: typ, ServiceID: in.ServiceID, Name: in.Name, RequiresApproval: true}
	sn := SwarmServiceName(in.Name)
	p.Steps = append(p.Steps, Step{ID: "01-remove-service-" + in.Name, Kind: StepRemoveService, ServiceName: sn, ServiceID: in.ServiceID, Destructive: true,
		Note: fmt.Sprintf("remove the Swarm service %s if it carries the ownership label of service %s (a missing service counts as removed)", sn, in.ServiceID)})
	vols := slices.Clone(c.Volumes)
	slices.SortFunc(vols, func(a, b Volume) int { return strings.Compare(a.Name, b.Name) })
	for _, v := range vols {
		vn := VolumeName(in.Name, v.Name)
		if !in.RemoveVolumes {
			p.Warnings = append(p.Warnings, fmt.Sprintf("volume %s and its data on host %s are kept; remove them by hand or delete with volume removal", vn, c.Placement.HostID))
			continue
		}
		p.Steps = append(p.Steps, Step{ID: fmt.Sprintf("%02d-remove-volume-%s", len(p.Steps)+1, v.Name), Kind: StepRemoveVolume, HostID: c.Placement.HostID,
			After: []string{p.Steps[0].ID}, Volume: vn, NodeUUID: VolumeIdentity(in.ServiceID, v.Name), Destructive: true,
			Note: fmt.Sprintf("remove the Docker volume %s on host %s after its identity marker matched service %s (a missing or empty unmarked volume counts as removed)", vn, c.Placement.HostID, in.ServiceID)})
	}
	if in.RemoveVolumes && len(vols) > 0 {
		p.Warnings = append(p.Warnings, "Docker removes only the volume object; the data directory below the volume base stays on the host until you delete it")
	}
	return p
}

// Health implements Type.
func (Container) Health(in HealthInput) (Health, error) {
	if _, err := decodeContainer(in.Spec); err != nil {
		return Health{}, err
	}
	return observationHealth(in.Observed), nil
}

// observationHealth is the generic verdict: unknown without observation,
// healthy when every desired task runs, degraded when some run.
func observationHealth(o Observation) Health {
	switch {
	case !o.Known:
		return Health{State: HealthUnknown, Reasons: []string{"no observation of the service"}}
	case o.Desired > 0 && o.Running >= o.Desired:
		return Health{State: HealthHealthy}
	case o.Running > 0:
		return Health{State: HealthDegraded, Reasons: []string{fmt.Sprintf("%d of %d tasks run", o.Running, o.Desired)}}
	}
	r := fmt.Sprintf("0 of %d tasks run", o.Desired)
	if o.Reason != "" {
		r += ": " + o.Reason
	}
	return Health{State: HealthUnhealthy, Reasons: []string{r}}
}
