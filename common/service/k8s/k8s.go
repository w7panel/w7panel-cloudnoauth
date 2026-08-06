package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	ErrAppGroupParentNotFound = errors.New("appgroup parent not found")
	ErrAppGroupNotFound       = errors.New("appgroup not found")
	ErrAppCredentialNotFound  = errors.New("app credential not found")
)

const (
	k8sMaxIdleConns        = 100
	k8sMaxIdleConnsPerHost = 50
	k8sMaxConnsPerHost     = 100
)

func IsSkippableCredentialError(err error) bool {
	return errors.Is(err, ErrAppGroupParentNotFound) ||
		errors.Is(err, ErrAppGroupNotFound) ||
		errors.Is(err, ErrAppCredentialNotFound)
}

type ApiError struct {
	ErrorMsg string `json:"error"`
	Code     int    `json:"code"`
}

func (ve ApiError) Error() string {
	return ve.ErrorMsg
}

type K8sService struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
	clientset  kubernetes.Interface
}

type AppCredential struct {
	AppGroup  string
	AppID     string
	AppSecret string
}

type k8sList[T any] struct {
	Items []T `json:"items"`
}

type k8sObjectMeta struct {
	Name            string              `json:"name"`
	UID             string              `json:"uid"`
	Namespace       string              `json:"namespace"`
	Labels          map[string]string   `json:"labels"`
	Annotations     map[string]string   `json:"annotations"`
	OwnerReferences []k8sOwnerReference `json:"ownerReferences"`
}

type k8sOwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// K8sPod is the subset of Pod fields used when resolving application credentials.
// Metadata.UID uniquely identifies a Pod instance, including across Pod name or
// IP reuse.
type K8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Status   struct {
		PodIP string `json:"podIP"`
	} `json:"status"`
}

type PodEventType string

const (
	PodAdded   PodEventType = "added"
	PodUpdated PodEventType = "updated"
	PodDeleted PodEventType = "deleted"
)

// PodEvent is emitted by WatchPods. Consumers decide how Pod lifecycle
// changes affect their own state, such as an IP-to-Pod cache.
type PodEvent struct {
	Type PodEventType
	Pod  K8sPod
}

type appGroup struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Spec     struct {
		AppCredentials struct {
			AppID     string `json:"appid"`
			AppSecret string `json:"appSecret"`
		} `json:"appCredentials"`
	} `json:"spec"`
}

type k8sReplicaSet struct {
	Metadata k8sObjectMeta `json:"metadata"`
}

type k8sDeployment struct {
	Metadata k8sObjectMeta `json:"metadata"`
}

func NewK8sService(k8sConfig string) (*K8sService, error) {
	config, err := makeK8sConfig(k8sConfig)
	if err != nil {
		return nil, err
	}
	config.Timeout = 10 * time.Second
	existingWrapTransport := config.WrapTransport
	config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		rt = tuneK8sTransport(rt)
		if existingWrapTransport != nil {
			return existingWrapTransport(rt)
		}
		return rt
	}

	transport, err := rest.TransportFor(config)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &K8sService{
		BaseURL:   strings.TrimRight(config.Host, "/"),
		clientset: clientset,
		HTTPClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}, nil
}

// WatchPods starts a namespace-scoped Pod informer and emits lifecycle events
// to handler. It owns Kubernetes observation only; consumers own any cache or
// other reaction to those events.
func (s *K8sService) WatchPods(ctx context.Context, namespace string, handler func(PodEvent)) error {
	if s.clientset == nil {
		return errors.New("kubernetes clientset is not configured")
	}
	namespace = resolveNamespace(namespace)
	factory := informers.NewSharedInformerFactoryWithOptions(s.clientset, 0, informers.WithNamespace(namespace))
	informer := factory.Core().V1().Pods().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { emitPodEvent(handler, PodAdded, obj) },
		UpdateFunc: func(oldObj, newObj any) {
			emitPodEvent(handler, PodDeleted, oldObj)
			emitPodEvent(handler, PodUpdated, newObj)
		},
		DeleteFunc: func(obj any) { emitPodEvent(handler, PodDeleted, obj) },
	})
	factory.Start(ctx.Done())
	syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
		return fmt.Errorf("synchronize pod watcher for namespace %s", namespace)
	}
	return nil
}

func emitPodEvent(handler func(PodEvent), eventType PodEventType, obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			return
		}
	}
	if pod.Status.PodIP == "" {
		return
	}
	cached := K8sPod{Metadata: k8sObjectMeta{Name: pod.Name, UID: string(pod.UID), Namespace: pod.Namespace, Labels: pod.Labels, Annotations: pod.Annotations}}
	cached.Status.PodIP = pod.Status.PodIP
	handler(PodEvent{Type: eventType, Pod: cached})
}

func makeK8sConfig(k8sConfig string) (*rest.Config, error) {
	if strings.TrimSpace(k8sConfig) != "" {
		return clientcmd.RESTConfigFromKubeConfig([]byte(k8sConfig))
	}
	return rest.InClusterConfig()
}

func tuneK8sTransport(rt http.RoundTripper) http.RoundTripper {
	transport, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}

	cloned := transport.Clone()
	if cloned.MaxIdleConns == 0 || cloned.MaxIdleConns < k8sMaxIdleConns {
		cloned.MaxIdleConns = k8sMaxIdleConns
	}
	if cloned.MaxIdleConnsPerHost == 0 || cloned.MaxIdleConnsPerHost < k8sMaxIdleConnsPerHost {
		cloned.MaxIdleConnsPerHost = k8sMaxIdleConnsPerHost
	}
	if cloned.MaxConnsPerHost == 0 || cloned.MaxConnsPerHost > k8sMaxConnsPerHost {
		cloned.MaxConnsPerHost = k8sMaxConnsPerHost
	}
	return cloned
}

// ResolveAppCredentialForPod resolves credentials for a Pod that has already
// been identified from the request source IP.
func (s *K8sService) ResolveAppCredentialForPod(ctx context.Context, namespace string, pod K8sPod) (AppCredential, error) {
	namespace = resolveNamespace(namespace)
	parentName, err := s.ResolveAppGroupParentName(ctx, namespace, pod)
	if err != nil {
		slog.Warn("k8s resolve appgroup parent failed",
			"namespace", namespace,
			"pod", pod.Metadata.Name,
			"error", err,
		)
		return AppCredential{}, err
	}

	group, err := s.QueryAppGroup(ctx, namespace, parentName)
	if err != nil {
		err = fmt.Errorf("%w: parent %s: %v", ErrAppGroupNotFound, parentName, err)
		slog.Warn("k8s resolve appgroup by name failed",
			"namespace", namespace,
			"pod", pod.Metadata.Name,
			"appgroup", parentName,
			"error", err,
		)
		return AppCredential{}, err
	}

	appID, appSecret := resolveAppCredentialFromAppGroup(group)
	if appID == "" || appSecret == "" {
		slog.Warn("k8s app credential annotation missing",
			"namespace", namespace,
			"pod", pod.Metadata.Name,
			"appgroup", group.Metadata.Name,
			"has_appid", appID != "",
			"has_appsecret", appSecret != "",
		)
		return AppCredential{}, fmt.Errorf("%w: appid or appsecret not found in appgroup %s", ErrAppCredentialNotFound, group.Metadata.Name)
	}

	slog.Info("k8s resolve app credential succeeded",
		"namespace", namespace,
		"pod", pod.Metadata.Name,
		"appgroup", group.Metadata.Name,
		"appid", appID,
	)

	return AppCredential{
		AppGroup:  group.Metadata.Name,
		AppID:     appID,
		AppSecret: appSecret,
	}, nil
}

func (s *K8sService) ResolveAppGroupParentName(ctx context.Context, namespace string, pod K8sPod) (string, error) {
	if parentName := resolveAppGroupParentNameFromMetadata(pod.Metadata); parentName != "" {
		slog.Info("k8s appgroup parent resolved from pod metadata",
			"namespace", namespace,
			"pod", pod.Metadata.Name,
			"parent", parentName,
		)
		return parentName, nil
	}

	deployment, err := s.QueryDeploymentByPod(ctx, namespace, pod)
	if err != nil {
		return "", err
	}
	if parentName := resolveAppGroupParentNameFromMetadata(deployment.Metadata); parentName != "" {
		slog.Info("k8s appgroup parent resolved from deployment metadata",
			"namespace", namespace,
			"pod", pod.Metadata.Name,
			"deployment", deployment.Metadata.Name,
			"parent", parentName,
		)
		return parentName, nil
	}

	return "", fmt.Errorf("%w: appgroup parent annotation not found in deployment %s for pod %s", ErrAppGroupParentNotFound, deployment.Metadata.Name, pod.Metadata.Name)
}

func (s *K8sService) QueryDeploymentByPod(ctx context.Context, namespace string, pod K8sPod) (k8sDeployment, error) {
	replicaSetName := ownerReferenceName(pod.Metadata.OwnerReferences, "ReplicaSet")
	if replicaSetName == "" {
		return k8sDeployment{}, fmt.Errorf("%w: replicaset owner not found in pod %s", ErrAppGroupParentNotFound, pod.Metadata.Name)
	}

	replicaSet, err := s.QueryReplicaSet(ctx, namespace, replicaSetName)
	if err != nil {
		return k8sDeployment{}, err
	}

	deploymentName := ownerReferenceName(replicaSet.Metadata.OwnerReferences, "Deployment")
	if deploymentName == "" {
		return k8sDeployment{}, fmt.Errorf("%w: deployment owner not found in replicaset %s", ErrAppGroupParentNotFound, replicaSet.Metadata.Name)
	}

	return s.QueryDeployment(ctx, namespace, deploymentName)
}

func (s *K8sService) QueryReplicaSet(ctx context.Context, namespace string, name string) (k8sReplicaSet, error) {
	namespace = resolveNamespace(namespace)
	slog.Debug("k8s query replicaset by name",
		"namespace", namespace,
		"replicaset", name,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/apis/apps/v1/namespaces/%s/replicasets/%s",
		s.baseURL(),
		url.PathEscape(namespace),
		url.PathEscape(name),
	), nil)
	if err != nil {
		return k8sReplicaSet{}, err
	}

	resp, err := s.doPanelReq(req)
	if err != nil {
		return k8sReplicaSet{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return k8sReplicaSet{}, err
	}

	replicaSet := &k8sReplicaSet{}
	if err = json.Unmarshal(respBody, replicaSet); err != nil {
		return k8sReplicaSet{}, err
	}
	if replicaSet.Metadata.Name == "" {
		return k8sReplicaSet{}, fmt.Errorf("replicaset not found by name %s", name)
	}

	return *replicaSet, nil
}

func (s *K8sService) QueryDeployment(ctx context.Context, namespace string, name string) (k8sDeployment, error) {
	namespace = resolveNamespace(namespace)
	slog.Debug("k8s query deployment by name",
		"namespace", namespace,
		"deployment", name,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/apis/apps/v1/namespaces/%s/deployments/%s",
		s.baseURL(),
		url.PathEscape(namespace),
		url.PathEscape(name),
	), nil)
	if err != nil {
		return k8sDeployment{}, err
	}

	resp, err := s.doPanelReq(req)
	if err != nil {
		return k8sDeployment{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return k8sDeployment{}, err
	}

	deployment := &k8sDeployment{}
	if err = json.Unmarshal(respBody, deployment); err != nil {
		return k8sDeployment{}, err
	}
	if deployment.Metadata.Name == "" {
		return k8sDeployment{}, fmt.Errorf("deployment not found by name %s", name)
	}

	return *deployment, nil
}

func (s *K8sService) QueryPodByIP(ctx context.Context, namespace string, remoteIP string) (K8sPod, error) {
	namespace = resolveNamespace(namespace)
	var pods k8sList[K8sPod]
	slog.Debug("k8s query pod by ip",
		"namespace", namespace,
		"remote_ip", remoteIP,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/api/v1/namespaces/%s/pods?fieldSelector=%s",
		s.baseURL(),
		url.PathEscape(namespace),
		url.QueryEscape("status.podIP="+remoteIP),
	), nil)
	if err != nil {
		return K8sPod{}, err
	}

	resp, err := s.doPanelReq(req)
	if err != nil {
		return K8sPod{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return K8sPod{}, err
	}
	if err = json.Unmarshal(respBody, &pods); err != nil {
		return K8sPod{}, err
	}

	for _, pod := range pods.Items {
		if pod.Status.PodIP == remoteIP {
			slog.Info("k8s pod matched by ip",
				"namespace", namespace,
				"remote_ip", remoteIP,
				"pod", pod.Metadata.Name,
			)
			return pod, nil
		}
	}

	return K8sPod{}, fmt.Errorf("pod not found by remote ip %s", remoteIP)
}

func (s *K8sService) QueryAppGroup(ctx context.Context, namespace string, name string) (appGroup, error) {
	namespace = resolveNamespace(namespace)
	slog.Debug("k8s query appgroup by name",
		"namespace", namespace,
		"appgroup", name,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/apis/w7panel.w7.com/v1alpha1/namespaces/%s/appgroups/%s",
		s.baseURL(),
		url.PathEscape(namespace),
		url.PathEscape(name),
	), nil)
	if err != nil {
		return appGroup{}, err
	}

	resp, err := s.doPanelReq(req)
	if err != nil {
		return appGroup{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return appGroup{}, err
	}

	group := &appGroup{}
	if err = json.Unmarshal(respBody, group); err != nil {
		return appGroup{}, err
	}
	if group.Metadata.Name == "" {
		return appGroup{}, fmt.Errorf("appgroup not found by parent %s", name)
	}

	return *group, nil
}

func (s *K8sService) doPanelReq(req *http.Request) (*http.Response, error) {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	startedAt := time.Now()
	slog.Debug("k8s http request started",
		"method", req.Method,
		"url", req.URL.String(),
	)
	resp, err := s.httpClient().Do(req)
	if err != nil {
		slog.Warn("k8s http request failed",
			"method", req.Method,
			"url", req.URL.String(),
			"duration", time.Since(startedAt),
			"error", err,
		)
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		slog.Warn("k8s http request returned non-success",
			"method", req.Method,
			"url", req.URL.String(),
			"status", resp.StatusCode,
			"duration", time.Since(startedAt),
			"response", string(respBody),
		)
		return nil, fmt.Errorf("failed to request panel, status: %d, response: %s", resp.StatusCode, string(respBody))
	}

	slog.Debug("k8s http request succeeded",
		"method", req.Method,
		"url", req.URL.String(),
		"status", resp.StatusCode,
		"duration", time.Since(startedAt),
	)
	return resp, nil
}

func (s *K8sService) baseURL() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return "https://kubernetes.default.svc"
}

func (s *K8sService) httpClient() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func resolveNamespace(namespace string) string {
	if namespace != "" {
		return namespace
	}
	return "default"
}

func resolveAppGroupParentNameFromMetadata(metadata k8sObjectMeta) string {
	for _, key := range []string{"w7.cc/owner-group-name", "w7.cc/parent-group-name", "w7.cc/group-name"} {
		if value := metadata.Labels[key]; value != "" {
			return value
		}
	}
	for _, key := range []string{"w7.cc/owner-group-name", "w7.cc/parent-group-name", "w7.cc/group-name"} {
		if value := metadata.Annotations[key]; value != "" {
			return value
		}
	}
	return ""
}

func ownerReferenceName(ownerReferences []k8sOwnerReference, kind string) string {
	for _, ownerReference := range ownerReferences {
		if strings.EqualFold(ownerReference.Kind, kind) && ownerReference.Name != "" {
			return ownerReference.Name
		}
	}
	return ""
}

func resolveAppCredentialFromAppGroup(group appGroup) (string, string) {
	appID := group.Spec.AppCredentials.AppID
	appSecret := group.Spec.AppCredentials.AppSecret
	if appID != "" || appSecret != "" {
		return appID, appSecret
	}

	return "", ""
}
