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
	SidecarImage         = getEnv("SIDECAR_IMAGE", "anthony4m/ananse-proxy:v1")
	InitImage            = getEnv("INIT_IMAGE", "anthony4m/ananse-init:v1")
	ProxyPort            = int32(getEnvInt("PROXY_PORT", 15001))
	InboundPort          = int32(getEnvInt("INBOUND_PORT", 15006))
	ProxyUID             = int64(getEnvInt("PROXY_UID", 1337))
	ControlPlaneEndpoint = getEnv("CONTROL_PLANE_ENDPOINT", "ananse-controlplane.ananse-system.svc:50051")
	DebugMode            = getEnv("DEBUG_MODE", "false") == "true"
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
	if !injectRequired(&pod, req.Namespace) {
		// If no injection needed, allow the pod as-is (no patch)
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

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
	// Safety first: Dont mess with the Node's network
	if pod.Spec.HostNetwork {
		return false
	}

	// Rule 2: Ignored Namespaces -> SKIP
	for _, ignore := range IgnoredNamespaces {
		if ns == ignore {
			return false
		}
	}

	// Rule 3: Explicit Annotation -> CHECK
	// "true" = Yes, "false" = No
	ann, ok := pod.Annotations["sidecar.ananse.io/inject"]
	if ok {
		return strings.ToLower(ann) == "true"
	}

	// Rule 4: Default behavior -> SKIP (Opt-in Model)
	return false
}

// -----------------------------------------------------------------------------
// Modification Logic (The "Surgeon")
// -----------------------------------------------------------------------------

func injectSidecar(pod *corev1.Pod) {
	// A. Init Container (Sets up iptables)
	initContainer := corev1.Container{
		Name:            "ananse-init",
		Image:           InitImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser: ptr(int64(0)), // Root (required for iptables)
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
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
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

	// C. Append logic (Go handles nil slices automatically!)
	pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)
	pod.Spec.Containers = append(pod.Spec.Containers, sidecarContainer)

	// D. Add Annotation to track injection status
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations["sidecar.ananse.io/status"] = "injected"
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
