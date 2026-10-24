// Package k8s contains a typed, client-go-free manifest subset for previews.
// These types do not claim the completeness of the Kubernetes API schemas.
package k8s

import "encoding/json"

const LabelInstallation = "dbcm.installation"

type Metadata struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
}
type Object interface {
	Header() (string, string, Metadata)
}
type Manifest[T any] struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       T        `json:"spec"`
}

func (m Manifest[T]) Header() (string, string, Metadata) { return m.APIVersion, m.Kind, m.Metadata }

type Selector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}
type WorkloadSpec struct {
	Replicas    int         `json:"replicas"`
	Selector    Selector    `json:"selector"`
	Template    PodTemplate `json:"template"`
	ServiceName string      `json:"serviceName,omitempty"`
}
type PodTemplate struct {
	Metadata Metadata `json:"metadata"`
	Spec     PodSpec  `json:"spec"`
}
type PodSpec struct {
	Containers                    []Container       `json:"containers"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
	AutomountServiceAccountToken  bool              `json:"automountServiceAccountToken"`
	TerminationGracePeriodSeconds int64             `json:"terminationGracePeriodSeconds"`
}
type Container struct {
	Name         string               `json:"name"`
	Image        string               `json:"image"`
	Command      []string             `json:"command,omitempty"`
	Args         []string             `json:"args,omitempty"`
	Env          []EnvVar             `json:"env,omitempty"`
	VolumeMounts []VolumeMount        `json:"volumeMounts,omitempty"`
	Resources    ResourceRequirements `json:"resources"`
}
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}
type Volume struct {
	Name                  string        `json:"name"`
	PersistentVolumeClaim *ClaimSource  `json:"persistentVolumeClaim,omitempty"`
	Secret                *SecretSource `json:"secret,omitempty"`
	ConfigMap             *ConfigSource `json:"configMap,omitempty"`
}
type ClaimSource struct {
	ClaimName string `json:"claimName"`
}
type SecretSource struct {
	SecretName string    `json:"secretName"`
	Items      []KeyPath `json:"items"`
}
type ConfigSource struct {
	Name  string    `json:"name"`
	Items []KeyPath `json:"items"`
}
type KeyPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}
type ServiceSpec struct {
	Selector  map[string]string `json:"selector"`
	Ports     []ServicePort     `json:"ports"`
	ClusterIP string            `json:"clusterIP,omitempty"`
}
type ServicePort struct {
	Name       string `json:"name"`
	Port       int    `json:"port"`
	TargetPort int    `json:"targetPort"`
	Protocol   string `json:"protocol"`
}
type PVCSpec struct {
	AccessModes      []string             `json:"accessModes"`
	StorageClassName string               `json:"storageClassName"`
	VolumeName       string               `json:"volumeName"`
	Resources        ResourceRequirements `json:"resources"`
}
type PVSpec struct {
	Capacity                      map[string]string `json:"capacity"`
	AccessModes                   []string          `json:"accessModes"`
	StorageClassName              string            `json:"storageClassName"`
	PersistentVolumeReclaimPolicy string            `json:"persistentVolumeReclaimPolicy"`
	Local                         LocalSource       `json:"local"`
	NodeAffinity                  NodeAffinity      `json:"nodeAffinity"`
	ClaimRef                      ClaimRef          `json:"claimRef"`
}
type ClaimRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}
type LocalSource struct {
	Path string `json:"path"`
}
type NodeAffinity struct {
	Required NodeSelector `json:"required"`
}
type NodeSelector struct {
	NodeSelectorTerms []NodeSelectorTerm `json:"nodeSelectorTerms"`
}
type NodeSelectorTerm struct {
	MatchExpressions []Requirement `json:"matchExpressions"`
}
type Requirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
}
type ConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Data       map[string]string `json:"data"`
}

func (m ConfigMap) Header() (string, string, Metadata) { return m.APIVersion, m.Kind, m.Metadata }

type IngressRouteSpec struct {
	EntryPoints []string `json:"entryPoints"`
	Routes      []Route  `json:"routes"`
}
type Route struct {
	Kind     string         `json:"kind"`
	Match    string         `json:"match"`
	Services []RouteService `json:"services"`
}
type RouteService struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// RawObject is used only at the REST boundary; renderers use typed specs.
type RawObject struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Spec       json.RawMessage   `json:"spec,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
}

func (m RawObject) Header() (string, string, Metadata) { return m.APIVersion, m.Kind, m.Metadata }
