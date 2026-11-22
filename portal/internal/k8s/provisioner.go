package k8s

import (
	"context"
	"fmt"

	"github.com/tallnatt/kubetorrent/portal/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Provisioner interface {
	ProvisionUser(ctx context.Context, username string) error
	DeprovisionUser(ctx context.Context, username string) error
	GetUserStatus(ctx context.Context, username string) string
}

type NoopProvisioner struct{}

func (NoopProvisioner) ProvisionUser(context.Context, string) error   { return nil }
func (NoopProvisioner) DeprovisionUser(context.Context, string) error { return nil }
func (NoopProvisioner) GetUserStatus(context.Context, string) string  { return "local-noop" }

type KubeProvisioner struct {
	client kubernetes.Interface
	cfg    *config.Config
}

func NewProvisioner(cfg *config.Config) (Provisioner, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return NoopProvisioner{}, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &KubeProvisioner{client: cs, cfg: cfg}, nil
}

func NewFromClient(cs kubernetes.Interface, cfg *config.Config) *KubeProvisioner {
	return &KubeProvisioner{client: cs, cfg: cfg}
}

func ResourceName(username string) string {
	return "kubetorrent-client-" + username
}

func (p *KubeProvisioner) ProvisionUser(ctx context.Context, username string) error {
	name := ResourceName(username)
	labels := map[string]string{
		"app.kubernetes.io/name":      name,
		"app.kubernetes.io/component": "client",
		"app.kubernetes.io/part-of":   "kubetorrent",
		"kubetorrent.io/user":         username,
	}

	replicas := int32(1)
	hostPathType := corev1.HostPathDirectoryOrCreate

	gatewayEnv := []corev1.EnvVar{
		{Name: "GATEWAY", Value: p.cfg.GatewayHost},
		{Name: "VXLAN_ID", Value: p.cfg.VXLANID},
		{Name: "VXLAN_IP_NETWORK", Value: p.cfg.VXLANNetwork},
		{Name: "NOT_ROUTED_TO_GATEWAY_CIDRS", Value: p.cfg.NotRoutedCIDRs},
		{Name: "DNS_LOCAL_CIDRS", Value: "cluster.local"},
	}

	initScript := fmt.Sprintf(`mkdir -p /config/qBittorrent "/downloads/%s"
conf="/config/qBittorrent/qBittorrent.conf"
if [ ! -f "$conf" ]; then
  cat > "$conf" <<'EOF'
[BitTorrent]
Session\DefaultSavePath=/downloads
Session\Port=6881
Session\TempPath=/downloads/.incomplete

[Preferences]
WebUI\Port=8080
WebUI\LocalHostAuth=false
WebUI\AuthSubnetWhitelistEnabled=true
WebUI\AuthSubnetWhitelist=0.0.0.0/0
EOF
fi`, username)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app.kubernetes.io/name": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: p.cfg.ClientServiceAccount,
					InitContainers: []corev1.Container{
						{
							Name:    "init-qbittorrent-config",
							Image:   "busybox:1.37",
							Command: []string{"sh", "-c", initScript},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "configs", MountPath: "/config", SubPath: username},
								{Name: "host-downloads", MountPath: "/downloads"},
							},
						},
						{
							Name:            "gateway-init",
							Image:           p.cfg.PodGatewayImage,
							Env:             gatewayEnv,
							SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "gateway-sidecar",
							Image:           p.cfg.PodGatewayImage,
							Env:             append(gatewayEnv, corev1.EnvVar{Name: "IS_SIDECAR", Value: "true"}),
							SecurityContext: &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}}},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("50m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
						},
						{
							Name:  "qbittorrent",
							Image: p.cfg.QBittorrentImage,
							Env: []corev1.EnvVar{
								{Name: "PUID", Value: "1000"},
								{Name: "PGID", Value: "1000"},
								{Name: "TZ", Value: "UTC"},
								{Name: "WEBUI_PORT", Value: "8080"},
								{Name: "TORRENTING_PORT", Value: "6881"},
							},
							Ports: []corev1.ContainerPort{
								{Name: "webui", ContainerPort: 8080},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "configs", MountPath: "/config", SubPath: username},
								{Name: "host-downloads", MountPath: "/downloads", SubPath: username},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       10,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("150Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "configs",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.cfg.ConfigsPVCName,
								},
							},
						},
						{
							Name: "host-downloads",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: p.cfg.HostDownloadsPath,
									Type: &hostPathType,
								},
							},
						},
					},
				},
			},
		},
	}

	_, err := p.client.AppsV1().Deployments(p.cfg.Namespace).Create(ctx, dep, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment %s: %w", name, err)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.cfg.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app.kubernetes.io/name": name},
			Ports: []corev1.ServicePort{
				{
					Name:       "webui",
					Port:       8080,
					TargetPort: intstr.FromInt32(8080),
				},
			},
		},
	}

	_, err = p.client.CoreV1().Services(p.cfg.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service %s: %w", name, err)
	}

	return nil
}

func (p *KubeProvisioner) DeprovisionUser(ctx context.Context, username string) error {
	name := ResourceName(username)
	if err := p.client.AppsV1().Deployments(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete deployment %s: %w", name, err)
	}
	if err := p.client.CoreV1().Services(p.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	return nil
}

func (p *KubeProvisioner) GetUserStatus(ctx context.Context, username string) string {
	name := ResourceName(username)
	dep, err := p.client.AppsV1().Deployments(p.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "missing"
		}
		return "unknown"
	}
	return fmt.Sprintf("%d/%d ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
}
