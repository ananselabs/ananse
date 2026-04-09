package injector

import (
	px "ananse/pkg/proxy"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
)

var (
	// CONFIGURATION - loaded from environment variables (set via ConfigMap)
	SidecarImage         = getEnv("SIDECAR_IMAGE", "ghcr.io/ananselabs/ananse-proxy:latest")
	InitImage            = getEnv("INIT_IMAGE", "ghcr.io/ananselabs/ananse-init:latest")
	ProxyPort            = int32(getEnvInt("PROXY_PORT", 15001))
	InboundPort          = int32(getEnvInt("INBOUND_PORT", 15006))
	ProxyUID             = int64(getEnvInt("PROXY_UID", 1337))
	ControlPlaneEndpoint = getEnv("CONTROL_PLANE_ENDPOINT", "ananse-controlplane.ananse-system.svc:50051")
	DebugMode            = getEnv("DEBUG_MODE", "false") == "true"
	mtlsEnabled          = "false"
	IgnoredNamespaces    = []string{
		metav1.NamespaceSystem, // "kube-system"
		metav1.NamespacePublic, // "kube-public"
		"cert-manager",         // Prevent deadlock with cert-manager
		"ananse-system",        // Don't inject into own controlplane
	}
)

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func StartInjection(port string) error {
	px.InitLogger() // Once at startup
	defer px.Logger.Sync()
	http.HandleFunc("/mutate", handleMutate)
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Start TLS Server
	// Note: certs must be mounted at /etc/webhook/certs via deployment
	px.Logger.Info("Ananse Injector listening on port 8443...")
	err := http.ListenAndServeTLS(port,
		"/etc/webhook/certs/tls.crt",
		"/etc/webhook/certs/tls.key",
		nil,
	)
	if err != nil {
		px.Logger.Error("Failed to start server", zap.Error(err))
		return err
	}
	return nil
}

func handleMutate(w http.ResponseWriter, r *http.Request) {
	// A. Read body
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// B. Decode AdmissionReview
	var admissionReview admissionv1.AdmissionReview
	if _, _, err := scheme.Codecs.UniversalDecoder().Decode(body, nil, &admissionReview); err != nil {
		px.Logger.Error("Failed to decode AdmissionReview", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// C. Execute Logic
	// create a responds that match the request uid
	admissionResponse := mutatePod(admissionReview.Request)
	admissionResponse.UID = admissionReview.Request.UID
	admissionReview.Response = admissionResponse

	// D. Send Response
	respBytes, err := json.Marshal(admissionReview)
	if err != nil {
		px.Logger.Error("Failed to marshal AdmissionReview", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(respBytes); err != nil {
		px.Logger.Error("Failed to write response", zap.Error(err))
		return
	}
	px.Logger.Info("Response sent")
	return
}

// -----------------------------------------------------------------------------
// Core Injection Logic
// -----------------------------------------------------------------------------

func mutatePod(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	// 1. Decode the Raw Pod from the request
	var pod corev1.Pod
	if _, _, err := scheme.Codecs.UniversalDecoder().Decode(req.Object.Raw, nil, &pod); err != nil {
		px.Logger.Error("failed to decode pod",
			zap.String("namespace", req.Namespace),
			zap.Error(err))
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: err.Error()},
		}
	}

	// 2. Decision Hierarchy: Should we inject?
	px.Logger.Info("checking injection requirement",
		zap.String("namespace", req.Namespace),
		zap.String("pod", pod.Name),
		zap.Bool("hostNetwork", pod.Spec.HostNetwork),
		zap.Any("annotations", pod.Annotations))

	if !injectRequired(&pod, req.Namespace) {
		px.Logger.Info("injection NOT required, skipping",
			zap.String("namespace", req.Namespace),
			zap.String("pod", pod.Name))
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	px.Logger.Info("injection REQUIRED, proceeding",
		zap.String("namespace", req.Namespace),
		zap.String("pod", pod.Name))

	// 3. STRUCT DIFFING STRATEGY
	//    a. Keep the original JSON
	//    b. Make a DeepCopy of the Pod struct
	//    c. Modify the struct using standard Go (append, set fields)
	//    d. Marshal the modified struct
	//    e. Calculate the JSON Patch between Original vs Modified

	modifiedPod := pod.DeepCopy()
	injectSidecar(modifiedPod)

	// 4. Generate the Patch
	patchBytes, err := createPatch(req.Object.Raw, modifiedPod)
	if err != nil {
		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result:  &metav1.Status{Message: err.Error()},
		}
	}

	px.Logger.Info("sidecar injection",
		zap.String("namespace", req.Namespace),
		zap.String("pod", pod.Name),
		zap.String("uid", string(req.UID)))

	return &admissionv1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *admissionv1.PatchType {
			pt := admissionv1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// -----------------------------------------------------------------------------
// Decision Logic (The "Brain")
// -----------------------------------------------------------------------------

func injectRequired(pod *corev1.Pod, ns string) bool {
	// Rule 1: HostNetwork -> SKIP
	// Safety first: Don't mess with the Node's network
	if pod.Spec.HostNetwork {
		return false
	}

	// Rule 2: Ignored Namespaces -> SKIP
	// Note: webhook namespaceSelector should filter these, but double-check
	for _, ignore := range IgnoredNamespaces {
		if ns == ignore {
			return false
		}
	}

	// Rule 3: Explicit Pod Annotation -> OVERRIDE
	// Pod annotation takes precedence for opt-out
	ann, ok := pod.Annotations["sidecar.ananse.io/inject"]
	if ok {
		return strings.ToLower(ann) == "true"
	}

	// Rule 4: Default -> INJECT
	// If we reach here, the namespace has ananse.io/inject=enabled label
	// (enforced by webhook namespaceSelector), so inject by default
	return true
}

// -----------------------------------------------------------------------------
// Modification Logic (The "Surgeon")
// -----------------------------------------------------------------------------

func injectSidecar(pod *corev1.Pod) {
	if pod.Annotations["sidecar.ananse.io/mtls"] == "true" {
		mtlsEnabled = "true"
	}

	// A. Init Container (Sets up iptables)
	initContainer := corev1.Container{
		Name:            "ananse-init",
		Image:           InitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:    ptr(int64(0)), // Root (required for iptables)
			RunAsNonRoot: ptr(false),    // Override pod-level runAsNonRoot
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
				Drop: []corev1.Capability{"ALL"}, // Drop everything else
			},
		},
		Env: []corev1.EnvVar{
			{Name: "PROXY_PORT", Value: fmt.Sprintf("%d", ProxyPort)},
			{Name: "INBOUND_PORT", Value: fmt.Sprintf("%d", InboundPort)},
			{Name: "PROXY_UID", Value: func() string {
				if DebugMode {
					return "0"
				}
				return fmt.Sprintf("%d", ProxyUID)
			}()},
		},
	}

	// B. Sidecar Container (The Go Proxy)
	sidecarContainer := corev1.Container{
		Name:            "ananse-proxy",
		Image:           SidecarImage,
		ImagePullPolicy: corev1.PullIfNotPresent, // PullIfNotPresent for Kind local dev
		// Don't override Command/Args - let the image's CMD run (includes Delve in debug images)
		Ports: []corev1.ContainerPort{
			{Name: "outbound", ContainerPort: 15001, Protocol: corev1.ProtocolTCP},
			{Name: "inbound", ContainerPort: 15006, Protocol: corev1.ProtocolTCP},
		},
		SecurityContext: func() *corev1.SecurityContext {

			if DebugMode {
				// Debug mode: minimal restrictions for Delve
				return &corev1.SecurityContext{
					RunAsUser:                ptr(int64(0)), // root
					RunAsNonRoot:             ptr(false),
					AllowPrivilegeEscalation: ptr(true),
					ReadOnlyRootFilesystem:   ptr(false),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeUnconfined,
					},
				}
			}
			// Production mode: locked down
			return &corev1.SecurityContext{
				RunAsUser:                &ProxyUID,
				RunAsNonRoot:             ptr(true),
				AllowPrivilegeEscalation: ptr(false),
				ReadOnlyRootFilesystem:   ptr(true),
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
				},
			}
		}(),

		Env: []corev1.EnvVar{
			{Name: "ANANSE_MODE", Value: "sidecar"},
			{Name: "ANANSE_TRACING_ENABLED", Value: getEnv("TRACING_ENABLED", "false")},
			{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "otel-collector.monitoring.svc.cluster.local:4317"},
			{Name: "ANANSE_MTLS_ENABLED", Value: mtlsEnabled},
			// Tell Go runtime to GC aggressively before hitting the container limit.
			// Without this, GOGC=100 allows heap to reach 2×live_heap, which can
			// exceed the cgroup limit and trigger OOMKill before GC runs.
			{Name: "GOMEMLIMIT", Value: "480MiB"},
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}},
			{Name: "SERVICE_NAME", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"},
			}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			}},
			{Name: "CONTROL_PLANE_ENDPOINT", Value: ControlPlaneEndpoint},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("20Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}

	// Add cert volume mount if mTLS enabled
	if mtlsEnabled == "true" {
		sidecarContainer.VolumeMounts = []corev1.VolumeMount{
			{
				Name:      "ananse-mesh-certs",
				MountPath: "/etc/ananse/certs",
				ReadOnly:  true,
			},
		}
	}

	// Only add probes in production mode (debug mode waits for debugger)
	if !DebugMode {
		sidecarContainer.LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(15006)},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
		}
		sidecarContainer.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(15006)},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       5,
		}
	}

	// C. Probe rewriting disabled - sidecar doesn't implement health proxy yet
	// TODO: Implement /app-health/ proxy in sidecar, then re-enable
	// probeEnvVars := rewriteProbes(pod)
	// sidecarContainer.Env = append(sidecarContainer.Env, probeEnvVars...)

	// D. Append containers (Go handles nil slices automatically!)
	// NOTE: Must append sidecar AFTER adding probe env vars since Go copies structs
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)
	pod.Spec.Containers = append(pod.Spec.Containers, sidecarContainer)

	// E. Add mesh cert volume if mTLS enabled
	if mtlsEnabled == "true" {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: "ananse-mesh-certs",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: "ananse-mesh-certs",
				},
			},
		})
	}

	// F. Add Annotation to track injection status
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations["sidecar.ananse.io/status"] = "injected"
}

// probeConfig stores original probe configuration for sidecar to use
type probeConfig struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}

// rewriteProbes rewrites HTTP probes to go through the sidecar's admin port.
// This allows strict mTLS without breaking Kubernetes health checks.
// - Stores original config in annotations for sidecar to read
// - Returns env vars for sidecar to know probe configs
// - Rewrites HTTP probes; passes through TCP, exec, gRPC probes unchanged
func rewriteProbes(pod *corev1.Pod) []corev1.EnvVar {
	const sidecarAdminPort = 15021
	var envVars []corev1.EnvVar

	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]

		// Skip the sidecar itself
		if container.Name == "ananse-proxy" || container.Name == "ananse-init" {
			continue
		}

		// Rewrite liveness probe (HTTP only)
		if container.LivenessProbe != nil && container.LivenessProbe.HTTPGet != nil {
			originalPort := getProbePort(container.LivenessProbe.HTTPGet.Port, container)
			originalPath := container.LivenessProbe.HTTPGet.Path
			if originalPath == "" {
				originalPath = "/"
			}

			// Store original in annotation
			storeProbeAnnotation(pod, container.Name, "liveness", probeConfig{
				Port: originalPort,
				Path: originalPath,
			})

			// Add env vars for sidecar
			envVars = append(envVars,
				corev1.EnvVar{Name: "ANANSE_LIVENESS_PORT", Value: fmt.Sprintf("%d", originalPort)},
				corev1.EnvVar{Name: "ANANSE_LIVENESS_PATH", Value: originalPath},
			)

			// Rewrite to sidecar
			container.LivenessProbe.HTTPGet.Port = intstr.FromInt(sidecarAdminPort)
			container.LivenessProbe.HTTPGet.Path = fmt.Sprintf("/app-health/%s/livez", container.Name)
		}

		// Rewrite readiness probe (HTTP only)
		if container.ReadinessProbe != nil && container.ReadinessProbe.HTTPGet != nil {
			originalPort := getProbePort(container.ReadinessProbe.HTTPGet.Port, container)
			originalPath := container.ReadinessProbe.HTTPGet.Path
			if originalPath == "" {
				originalPath = "/"
			}

			// Store original in annotation
			storeProbeAnnotation(pod, container.Name, "readiness", probeConfig{
				Port: originalPort,
				Path: originalPath,
			})

			// Add env vars for sidecar
			envVars = append(envVars,
				corev1.EnvVar{Name: "ANANSE_READINESS_PORT", Value: fmt.Sprintf("%d", originalPort)},
				corev1.EnvVar{Name: "ANANSE_READINESS_PATH", Value: originalPath},
			)

			// Rewrite to sidecar
			container.ReadinessProbe.HTTPGet.Port = intstr.FromInt(sidecarAdminPort)
			container.ReadinessProbe.HTTPGet.Path = fmt.Sprintf("/app-health/%s/readyz", container.Name)
		}

		// Rewrite startup probe (HTTP only)
		if container.StartupProbe != nil && container.StartupProbe.HTTPGet != nil {
			originalPort := getProbePort(container.StartupProbe.HTTPGet.Port, container)
			originalPath := container.StartupProbe.HTTPGet.Path
			if originalPath == "" {
				originalPath = "/"
			}

			// Store original in annotation
			storeProbeAnnotation(pod, container.Name, "startup", probeConfig{
				Port: originalPort,
				Path: originalPath,
			})

			// Add env vars for sidecar
			envVars = append(envVars,
				corev1.EnvVar{Name: "ANANSE_STARTUP_PORT", Value: fmt.Sprintf("%d", originalPort)},
				corev1.EnvVar{Name: "ANANSE_STARTUP_PATH", Value: originalPath},
			)

			// Rewrite to sidecar
			container.StartupProbe.HTTPGet.Port = intstr.FromInt(sidecarAdminPort)
			container.StartupProbe.HTTPGet.Path = fmt.Sprintf("/app-health/%s/startupz", container.Name)
		}

		// TCP, exec, gRPC probes: pass through unchanged
		// (kubelet will probe directly, which is fine for non-HTTP checks)
	}

	return envVars
}

// storeProbeAnnotation saves original probe config to pod annotation
func storeProbeAnnotation(pod *corev1.Pod, containerName, probeType string, config probeConfig) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	configJSON, err := json.Marshal(config)
	if err != nil {
		px.Logger.Error("failed to marshal probe config", zap.Error(err))
		return
	}

	key := fmt.Sprintf("ananse.io/original-%s-probe-%s", probeType, containerName)
	pod.Annotations[key] = string(configJSON)
}

// getProbePort resolves the probe port to an integer
func getProbePort(port intstr.IntOrString, container *corev1.Container) int {
	if port.Type == intstr.Int {
		return port.IntValue()
	}

	// Named port - look up in container ports
	for _, p := range container.Ports {
		if p.Name == port.StrVal {
			return int(p.ContainerPort)
		}
	}

	// Fallback: try to parse as int
	if i, err := strconv.Atoi(port.StrVal); err == nil {
		return i
	}

	// Default to 8080 if we can't resolve
	return 8080
}

// -----------------------------------------------------------------------------
// Patch Generator
// -----------------------------------------------------------------------------

func createPatch(originalJSON []byte, modifiedPod *corev1.Pod) ([]byte, error) {
	// 1. Marshal the modified struct back to JSON
	modifiedJSON, err := json.Marshal(modifiedPod)
	if err != nil {
		return nil, err
	}

	// 2. Calculate the difference (JSON Patch)
	patches, err := jsonpatch.CreatePatch(originalJSON, modifiedJSON)
	if err != nil {
		return nil, err
	}

	// 3. Return the patch as bytes
	return json.Marshal(patches)
}

func ptr[T any](v T) *T {
	return &v
}
