package elasticsearch

import (
	"strconv"

	deploymentsv1alpha1 "github.com/elastic/stack-operators/pkg/apis/deployments/v1alpha1"
	"github.com/elastic/stack-operators/pkg/controller/stack/common"
	"github.com/mitchellh/hashstructure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// HTTPPort used by Elasticsearch for the REST API
	HTTPPort = 9200
	// TransportPort used by Elasticsearch for the Transport protocol
	TransportPort = 9300

	// defaultImageRepositoryAndName is the default image name without a tag
	defaultImageRepositoryAndName string = "docker.elastic.co/elasticsearch/elasticsearch"

	// defaultTerminationGracePeriodSeconds is the termination grace period for the Elasticsearch containers
	defaultTerminationGracePeriodSeconds int64 = 120
	// defaultInitContainerPrivileged determines if the init container should be privileged
	defaultInitContainerPrivileged bool = true
	// defaultInitContainerRunAsUser is the user id the init container should run as
	defaultInitContainerRunAsUser int64 = 0
)

var (
	// defaultContainerPorts are the default Elasticsearch port mappings
	defaultContainerPorts = []corev1.ContainerPort{
		{Name: "http", ContainerPort: HTTPPort, Protocol: corev1.ProtocolTCP},
		{Name: "transport", ContainerPort: TransportPort, Protocol: corev1.ProtocolTCP},
	}
)

// NewPod constructs a pod from the Stack definition.
func NewPod(s deploymentsv1alpha1.Stack) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      NewNodeName(s.Name),
			Namespace: s.Namespace,
			Labels:    NewLabels(s, true),
		},
		Spec: NewPodSpec(
			BuildNewPodSpecParams(s),
		),
	}
}

// BuildNewPodSpecParams creates a NewPodSpecParams from a Stack definition.
func BuildNewPodSpecParams(s deploymentsv1alpha1.Stack) NewPodSpecParams {
	return NewPodSpecParams{
		Version:                        s.Spec.Version,
		CustomImageName:                s.Spec.Elasticsearch.Image,
		ClusterName:                    s.Name,
		DiscoveryZenMinimumMasterNodes: ComputeMinimumMasterNodes(int(s.Spec.Elasticsearch.NodeCount)),
		DiscoveryServiceName:           DiscoveryServiceName(s.Name),
		SetVMMaxMapCount:               s.Spec.Elasticsearch.SetVMMaxMapCount,
	}
}

// NewPodSpecParams is used to build resources associated with an Elasticsearch Cluster
type NewPodSpecParams struct {
	// Version is the Elasticsearch version
	Version string
	// CustomImageName is the custom image used, leave empty for the default
	CustomImageName string
	// ClusterName is the name of the Elasticsearch cluster
	ClusterName string
	// DiscoveryServiceName is the name of the Service that should be used for discovery.
	DiscoveryServiceName string
	// DiscoveryZenMinimumMasterNodes is the setting for minimum master node in Zen Discovery
	DiscoveryZenMinimumMasterNodes int `hash:"ignore"`

	// SetVMMaxMapCount indicates whether a init container should be used to ensure that the `vm.max_map_count`
	// is set according to https://www.elastic.co/guide/en/elasticsearch/reference/current/vm-max-map-count.html.
	// Setting this to true requires the kubelet to allow running privileged containers.
	SetVMMaxMapCount bool
}

// Hash computes a unique hash with the current NewPodSpecParams
func (params NewPodSpecParams) Hash() string {
	hash, _ := hashstructure.Hash(params, nil)
	return strconv.FormatUint(hash, 10)
}

// NewPodSpec creates a new PodSpec for an Elasticsearch instance in this cluster.
func NewPodSpec(p NewPodSpecParams) corev1.PodSpec {
	// TODO: validate version?
	imageName := common.Concat(defaultImageRepositoryAndName, ":", p.Version)
	if p.CustomImageName != "" {
		imageName = p.CustomImageName
	}

	terminationGracePeriodSeconds := defaultTerminationGracePeriodSeconds

	// TODO: quota support
	volume := NewDefaultEmptyDirVolume()

	// TODO: Security Context, Optional init container
	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Env: []corev1.EnvVar{
				{Name: "node.name", Value: "", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.name"},
				}},
				{Name: "discovery.zen.ping.unicast.hosts", Value: p.DiscoveryServiceName},
				{Name: "cluster.name", Value: p.ClusterName},
				{Name: "discovery.zen.minimum_master_nodes", Value: strconv.Itoa(p.DiscoveryZenMinimumMasterNodes)},
				{Name: "network.host", Value: "0.0.0.0"},
				{Name: "path.data", Value: volume.DataPath()},
				{Name: "path.logs", Value: volume.LogsPath()},

				// TODO: the JVM options are hardcoded, but should be configurable
				{Name: "ES_JAVA_OPTS", Value: "-Xms1g -Xmx1g"},

				// TODO: dedicated node types support
				{Name: "node.master", Value: "true"},
				{Name: "node.data", Value: "true"},
				{Name: "node.ingest", Value: "true"},
			},
			Image:           imageName,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Name:            "elasticsearch",
			Ports:           defaultContainerPorts,
			// TODO: Hardcoded resource limits and requests
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("800m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			},
			ReadinessProbe: &corev1.Probe{
				FailureThreshold:    3,
				InitialDelaySeconds: 10,
				PeriodSeconds:       10,
				SuccessThreshold:    3,
				TimeoutSeconds:      5,
				Handler: corev1.Handler{
					Exec: &corev1.ExecAction{
						Command: []string{
							"sh",
							"-c",
							defaultReadinessProbeScript,
						},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{volume.VolumeMount()},
		}},
		TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
		Volumes:                       []corev1.Volume{volume.Volume()},
	}

	if !p.SetVMMaxMapCount {
		return podSpec
	}

	initContainerPrivileged := defaultInitContainerPrivileged
	initContainerRunAsUser := defaultInitContainerRunAsUser
	initContainerConfigureSysCtl := corev1.Container{
		Image:           imageName,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Name:            "configure-sysctl",
		SecurityContext: &corev1.SecurityContext{
			Privileged: &initContainerPrivileged,
			RunAsUser:  &initContainerRunAsUser,
		},
		Command: []string{"sysctl", "-w", "vm.max_map_count=262144"},
	}

	podSpec.InitContainers = append(podSpec.InitContainers, initContainerConfigureSysCtl)

	return podSpec
}
