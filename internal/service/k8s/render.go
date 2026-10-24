package k8s

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

func shaName(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:16] }

var ErrDisabled = errors.New("kubernetes groundwork is disabled")
var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// LocalBinding is explicit operator input, never inferred from a Swarm ID.
// The path must eventually be provisioned and identity-checked by an agent.
type LocalBinding struct {
	Node  string
	Path  string
	Bytes int64
}
type Renderer struct {
	Enabled        bool
	InstallationID string
	Namespace      string
	Local          map[string]LocalBinding // managed volume name -> binding
}

var _ service.Orchestrator = Renderer{}

// Preview retains typed objects alongside the common render contract.
type Preview struct {
	Plan    service.RenderedPlan
	Objects []Object
}

// YAML exports JSON-style YAML documents. JSON is a YAML 1.2 subset;
// strings are always quoted and document boundaries are explicit.
func (p Preview) YAML() (string, error) {
	var b strings.Builder
	for _, o := range p.Objects {
		v, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			return "", err
		}
		b.WriteString("---\n")
		b.Write(v)
		b.WriteByte('\n')
	}
	return b.String(), nil
}
func (r Renderer) Render(p service.Plan) (service.RenderedPlan, error) {
	v, e := r.Preview(p)
	return v.Plan, e
}
func (r Renderer) metadata(name, ns string, p service.Plan) Metadata {
	return Metadata{Name: name, Namespace: ns, Labels: map[string]string{LabelInstallation: r.InstallationID, service.LabelCluster: p.ServiceID, service.LabelServiceType: p.Type, service.LabelServiceName: p.Name}, Annotations: map[string]string{"dbcm.io/groundwork": "true", "dbcm.io/platform": "kubernetes"}}
}

// Preview renders create plans only. Unsupported semantics are retained as
// blockers rather than silently inheriting Docker behavior.
func (r Renderer) Preview(p service.Plan) (Preview, error) {
	if !r.Enabled {
		return Preview{}, ErrDisabled
	}
	if r.InstallationID == "" || !dnsName.MatchString(r.Namespace) || p.ServiceID == "" {
		return Preview{}, errors.New("k8s: installation, service identity and DNS namespace required")
	}
	v := Preview{Plan: service.RenderedPlan{Platform: service.PlatformKubernetes, Source: p, Blockers: slices.Clone(p.Blockers), RequiresApproval: true}}
	block := func(s string) { v.Plan.Blockers = append(v.Plan.Blockers, s) }
	block("W10 groundwork: execution is disabled; no real Kubernetes evidence")
	if p.Kind != service.PlanCreate {
		block("only create previews are supported; deletion/update requires an inventoried Kubernetes plan")
		return v.finish()
	}
	switch p.Type {
	case service.TypeContainer, service.TypeTraefik, service.TypeGalera, "sftp", "ftp", "s3", "nfs":
	default:
		block("service type has no Kubernetes capability declaration")
	}
	if p.Type == service.TypeGalera || p.Delegated() {
		block("Galera bootstrap and safe_to_bootstrap require fenced observation, quorum, identity checks and explicit guided recovery; StatefulSet cannot replace them")
	}
	if p.Delegated() {
		// Render steady-state node shapes for review, never bootstrap commands.
		specs := map[string]service.ServiceSpec{}
		for _, step := range p.Galera.Steps {
			if step.Spec != nil {
				raw, _ := json.Marshal(step.Spec)
				var spec service.ServiceSpec
				if err := json.Unmarshal(raw, &spec); err != nil {
					return Preview{}, err
				}
				spec.Args = slices.DeleteFunc(spec.Args, func(s string) bool { return s == "--wsrep-new-cluster" })
				specs[spec.Name] = spec
			}
		}
		names := []string{}
		for name := range specs {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			r.workload(&v, p, specs[name], true, block)
		}
	} else {
		for _, s := range p.Steps {
			if s.Spec != nil {
				r.workload(&v, p, *s.Spec, len(s.Spec.Mounts) > 0, block)
			}
		}
	}
	// Config artifacts stay non-secret, using a fixed data key for safe mounts.
	for i, a := range p.Artifacts {
		if a.Name == "swarm-service.json" || strings.HasSuffix(a.Name, ".json") {
			continue
		}
		name := fmt.Sprintf("dbcm-%s-config-%d", p.Name, i)
		v.Objects = append(v.Objects, ConfigMap{APIVersion: "v1", Kind: "ConfigMap", Metadata: r.metadata(name, r.Namespace, p), Data: map[string]string{"content": a.Content}})
		block("generated ConfigMap " + name + " requires explicit workload target mapping")
	}
	return v.finish()
}
func (v Preview) finish() (Preview, error) {
	slices.Sort(v.Plan.Blockers)
	v.Plan.Blockers = slices.Compact(v.Plan.Blockers)
	slices.SortFunc(v.Objects, func(a, b Object) int {
		av, ak, am := a.Header()
		bv, bk, bm := b.Header()
		return strings.Compare(av+"/"+ak+"/"+am.Namespace+"/"+am.Name, bv+"/"+bk+"/"+bm.Namespace+"/"+bm.Name)
	})
	seen := map[string]bool{}
	for _, o := range v.Objects {
		av, k, m := o.Header()
		key := av + "/" + k + "/" + m.Namespace + "/" + m.Name
		if seen[key] {
			return Preview{}, fmt.Errorf("k8s: duplicate object %s", key)
		}
		seen[key] = true
	}
	y, err := v.YAML()
	if err != nil {
		return Preview{}, err
	}
	v.Plan.Artifacts = []service.Artifact{{Name: "kubernetes.yaml", MediaType: "application/yaml", Content: y}}
	return v, nil
}
func quantities(a service.ResourceAmount) map[string]string {
	m := map[string]string{}
	if a.MilliCPU > 0 {
		m["cpu"] = fmt.Sprintf("%dm", a.MilliCPU)
	}
	if a.MemoryBytes > 0 {
		m["memory"] = strconv.FormatInt(a.MemoryBytes, 10)
	}
	return m
}
func (r Renderer) workload(v *Preview, p service.Plan, s service.ServiceSpec, stateful bool, block func(string)) {
	if !dnsName.MatchString(s.Name) {
		block("workload name is not a Kubernetes DNS label")
		return
	}
	meta := r.metadata(s.Name, r.Namespace, p)
	if installation := s.Labels[LabelInstallation]; installation != "" && installation != r.InstallationID {
		block("source workload belongs to another installation")
	}
	if node := s.Labels[service.LabelNode]; node != "" {
		meta.Labels[service.LabelNode] = node
	}
	selector := map[string]string{LabelInstallation: r.InstallationID, service.LabelCluster: p.ServiceID, "dbcm.workload": s.Name}
	meta.Labels["dbcm.workload"] = s.Name
	pod := PodSpec{NodeSelector: map[string]string{}, TerminationGracePeriodSeconds: 30}
	if s.StopGracePeriod > 0 {
		pod.TerminationGracePeriodSeconds = int64(s.StopGracePeriod.Seconds())
	}
	c := Container{Name: "service", Image: s.Image, Command: s.Command, Args: s.Args, Resources: ResourceRequirements{Limits: quantities(s.Resources.Limits), Requests: quantities(s.Resources.Reservations)}}
	for _, e := range s.Env {
		k, val, ok := strings.Cut(e, "=")
		if !ok {
			block("malformed environment entry")
			continue
		}
		c.Env = append(c.Env, EnvVar{Name: k, Value: val})
	}
	if len(s.Networks) > 0 {
		block("Swarm overlay networks require explicit Kubernetes NetworkPolicy and DNS mapping")
	}
	if s.User != "" {
		block("container user requires explicit Kubernetes securityContext mapping")
	}
	if s.RestartCondition != "" && s.RestartCondition != service.RestartAny {
		block("Swarm restart condition is not a Deployment restart policy")
	}
	if s.UpdateConfig != nil || s.RollbackConfig != nil {
		block("Swarm rolling update/rollback semantics need Kubernetes health and rollout gates")
	}
	for _, constraint := range s.Constraints {
		if strings.HasPrefix(constraint, "node.labels.") && strings.Contains(constraint, "==") {
			k, val, _ := strings.Cut(strings.TrimPrefix(constraint, "node.labels."), "==")
			pod.NodeSelector[strings.TrimSpace(k)] = strings.TrimSpace(val)
		} else {
			block("Swarm placement constraint requires explicit Kubernetes mapping: " + constraint)
		}
	}
	for i, m := range s.Mounts {
		name := fmt.Sprintf("data-%d", i)
		if m.Type != "volume" || !dnsName.MatchString(m.Source) {
			block("only named managed local volumes can be previewed")
			continue
		}
		bind, ok := r.Local[m.Source]
		if !ok || !dnsName.MatchString(bind.Node) || !path.IsAbs(bind.Path) || path.Clean(bind.Path) != bind.Path || bind.Path == "/" || bind.Bytes <= 0 {
			block("local storage pinning requires explicit node, absolute path and positive capacity for " + m.Source)
			continue
		}
		if existing := pod.NodeSelector["kubernetes.io/hostname"]; existing != "" && existing != bind.Node {
			block("local storage bindings conflict on node affinity")
		}
		pod.NodeSelector["kubernetes.io/hostname"] = bind.Node
		if s.Replicas != 1 {
			block("one local PV cannot be shared by multiple workload replicas")
		}
		block("local PV identity marker and returning-node safety are unverified for " + m.Source)
		pvName := fmt.Sprintf("dbcm-%x", shaName(r.InstallationID+"/"+r.Namespace+"/"+m.Source))
		pv := PVSpec{Capacity: map[string]string{"storage": strconv.FormatInt(bind.Bytes, 10)}, AccessModes: []string{"ReadWriteOnce"}, StorageClassName: "", PersistentVolumeReclaimPolicy: "Retain", Local: LocalSource{Path: bind.Path}, ClaimRef: ClaimRef{Name: m.Source, Namespace: r.Namespace}, NodeAffinity: NodeAffinity{Required: NodeSelector{NodeSelectorTerms: []NodeSelectorTerm{{MatchExpressions: []Requirement{{Key: "kubernetes.io/hostname", Operator: "In", Values: []string{bind.Node}}}}}}}}
		pvc := PVCSpec{AccessModes: []string{"ReadWriteOnce"}, StorageClassName: "", VolumeName: pvName, Resources: ResourceRequirements{Requests: map[string]string{"storage": strconv.FormatInt(bind.Bytes, 10)}}}
		v.Objects = append(v.Objects, Manifest[PVSpec]{"v1", "PersistentVolume", r.metadata(pvName, "", p), pv}, Manifest[PVCSpec]{"v1", "PersistentVolumeClaim", r.metadata(m.Source, r.Namespace, p), pvc})
		pod.Volumes = append(pod.Volumes, Volume{Name: name, PersistentVolumeClaim: &ClaimSource{ClaimName: m.Source}})
		c.VolumeMounts = append(c.VolumeMounts, VolumeMount{Name: name, MountPath: m.Target, ReadOnly: m.ReadOnly})
	}
	for i, secret := range s.Secrets {
		name := fmt.Sprintf("secret-%d", i)
		if !dnsName.MatchString(secret) {
			block("Secret reference requires Kubernetes DNS mapping")
			continue
		}
		pod.Volumes = append(pod.Volumes, Volume{Name: name, Secret: &SecretSource{SecretName: secret, Items: []KeyPath{{Key: "value", Path: secret}}}})
		c.VolumeMounts = append(c.VolumeMounts, VolumeMount{Name: name, MountPath: path.Join(service.SecretsDir, secret), SubPath: secret, ReadOnly: true})
	}
	for i, cfg := range s.Configs {
		name := fmt.Sprintf("config-%d", i)
		if !dnsName.MatchString(cfg.Config) {
			block("ConfigMap reference requires Kubernetes DNS mapping")
			continue
		}
		if cfg.Mode != 0 && cfg.Mode != 0444 {
			block("config mode needs explicit Kubernetes volume defaultMode")
		}
		pod.Volumes = append(pod.Volumes, Volume{Name: name, ConfigMap: &ConfigSource{Name: cfg.Config, Items: []KeyPath{{Key: "content", Path: "content"}}}})
		c.VolumeMounts = append(c.VolumeMounts, VolumeMount{Name: name, MountPath: cfg.Target, SubPath: "content", ReadOnly: true})
	}
	pod.Containers = []Container{c}
	kind := "Deployment"
	if stateful {
		kind = "StatefulSet"
	}
	ws := WorkloadSpec{Replicas: int(s.Replicas), Selector: Selector{MatchLabels: selector}, Template: PodTemplate{Metadata: Metadata{Labels: maps.Clone(meta.Labels), Annotations: maps.Clone(meta.Annotations)}, Spec: pod}}
	if stateful {
		ws.ServiceName = s.Name
	}
	v.Objects = append(v.Objects, Manifest[WorkloadSpec]{"apps/v1", kind, meta, ws})
	svc := ServiceSpec{Selector: selector}
	if stateful {
		svc.ClusterIP = "None"
	}
	for i, port := range s.Ports {
		protocol := strings.ToUpper(port.Protocol)
		if protocol == "" {
			protocol = "TCP"
		}
		svc.Ports = append(svc.Ports, ServicePort{Name: fmt.Sprintf("port-%d", i), Port: port.Target, TargetPort: port.Target, Protocol: protocol})
		if port.Published != 0 {
			block("published Swarm ports require an explicit Kubernetes exposure decision")
		}
	}
	if p.Type == service.TypeGalera && len(svc.Ports) == 0 {
		svc.Ports = []ServicePort{{Name: "database", Port: 3306, TargetPort: 3306, Protocol: "TCP"}}
	}
	if stateful || len(svc.Ports) > 0 {
		v.Objects = append(v.Objects, Manifest[ServiceSpec]{"v1", "Service", meta, svc})
	}
}

// Definition uses the existing validated service definitions and planners.
// Traefik routes are an additional typed preview; unsupported backends are blocked.
func (r Renderer) Definition(t service.Type, in service.PlanInput) (Preview, error) {
	if !r.Enabled {
		return Preview{}, ErrDisabled
	}
	if in.InstallationID != r.InstallationID {
		return Preview{}, errors.New("k8s: source installation differs from renderer")
	}
	if g, ok := t.(service.Galera); ok && g.NewUUID == nil {
		n := 0
		g.NewUUID = func() (string, error) {
			n++
			return service.VolumeIdentity(in.ServiceID, fmt.Sprintf("kubernetes-preview-%d", n)), nil
		}
		t = g
	}
	p, err := t.Plan(in)
	if err != nil {
		return Preview{}, err
	}
	v, err := r.Preview(p)
	if err != nil {
		return v, err
	}
	if t.Name() == service.TypeTraefik {
		var spec service.TraefikSpec
		if err := json.Unmarshal(in.Spec, &spec); err != nil {
			return Preview{}, err
		}
		for _, router := range spec.Routers {
			match := []string{}
			for _, h := range router.Match.Hosts {
				match = append(match, "Host(`"+h+"`)")
			}
			host := "(" + strings.Join(match, " || ") + ")"
			match = nil
			if host != "()" {
				match = append(match, host)
			}
			if router.Match.Path != "" {
				match = append(match, "Path(`"+router.Match.Path+"`)")
			}
			if router.Match.PathPrefix != "" {
				match = append(match, "PathPrefix(`"+router.Match.PathPrefix+"`)")
			}
			route := Route{Kind: "Rule", Match: strings.Join(match, " && ")}
			for _, backend := range spec.Services {
				if backend.Name != router.Service {
					continue
				}
				for _, server := range backend.Servers {
					u, e := url.Parse(server)
					if e != nil || !dnsName.MatchString(u.Hostname()) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
						v.Plan.Blockers = append(v.Plan.Blockers, "Traefik URL backend needs explicit Kubernetes Service mapping")
						continue
					}
					port := 80
					if u.Scheme != "http" {
						v.Plan.Blockers = append(v.Plan.Blockers, "Traefik backend scheme requires explicit transport mapping")
					}
					if u.Port() != "" {
						port, _ = strconv.Atoi(u.Port())
					}
					route.Services = append(route.Services, RouteService{Name: u.Hostname(), Port: port})
				}
			}
			if len(route.Services) == 0 || len(router.Middlewares) > 0 || router.TLS != nil || router.Priority != 0 {
				v.Plan.Blockers = append(v.Plan.Blockers, "Traefik route needs backend, middleware, priority and TLS capability validation")
			}
			v.Plan.Blockers = append(v.Plan.Blockers, "Traefik IngressRoute requires an installed and authorized Traefik CRD/controller")
			v.Objects = append(v.Objects, Manifest[IngressRouteSpec]{"traefik.io/v1alpha1", "IngressRoute", r.metadata("dbcm-"+in.Name+"-"+router.Name, r.Namespace, p), IngressRouteSpec{EntryPoints: router.EntryPoints, Routes: []Route{route}}})
		}
	}
	return v.finish()
}
