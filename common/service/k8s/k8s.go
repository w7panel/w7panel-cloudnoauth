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

	"k8s.io/client-go/rest"
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

type K8sService struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type AppCredential struct {
	AppGroup  string
	AppID     string
	AppSecret string
}

type k8sObjectMeta struct {
	Name            string              `json:"name"`
	Labels          map[string]string   `json:"labels"`
	Annotations     map[string]string   `json:"annotations"`
	OwnerReferences []k8sOwnerReference `json:"ownerReferences"`
}

type k8sOwnerReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type k8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
}

type k8sReplicaSet struct {
	Metadata k8sObjectMeta `json:"metadata"`
}

type k8sDeployment struct {
	Metadata k8sObjectMeta `json:"metadata"`
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
	return &K8sService{
		BaseURL: strings.TrimRight(config.Host, "/"),
		HTTPClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}, nil
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

// ResolveAppCredentialForPod resolves the AppGroup owning the current Pod and
// returns that AppGroup's application credential. The Pod may carry the owner
// group metadata itself; otherwise the lookup follows Pod -> ReplicaSet ->
// Deployment and reads it from the Deployment.
func (s *K8sService) ResolveAppCredentialForPod(ctx context.Context, namespace, podName string) (AppCredential, error) {
	namespace = resolveNamespace(namespace)
	podName = strings.TrimSpace(podName)
	if podName == "" {
		return AppCredential{}, errors.New("pod name is required")
	}

	pod, err := s.QueryPod(ctx, namespace, podName)
	if err != nil {
		return AppCredential{}, fmt.Errorf("query current pod %s: %w", podName, err)
	}
	groupName, err := s.ResolveAppGroupParentName(ctx, namespace, pod)
	if err != nil {
		return AppCredential{}, err
	}
	group, err := s.QueryAppGroup(ctx, namespace, groupName)
	if err != nil {
		return AppCredential{}, fmt.Errorf("%w: %s: %v", ErrAppGroupNotFound, groupName, err)
	}

	appID := group.Spec.AppCredentials.AppID
	appSecret := group.Spec.AppCredentials.AppSecret
	if appID == "" || appSecret == "" {
		return AppCredential{}, fmt.Errorf("%w: appid or appsecret not found in appgroup %s", ErrAppCredentialNotFound, groupName)
	}

	slog.Info("k8s resolve app credential succeeded",
		"namespace", namespace,
		"appgroup", groupName,
		"appid", appID,
	)
	return AppCredential{AppGroup: groupName, AppID: appID, AppSecret: appSecret}, nil
}

func (s *K8sService) ResolveAppGroupParentName(ctx context.Context, namespace string, pod k8sPod) (string, error) {
	if parentName := resolveAppGroupParentNameFromMetadata(pod.Metadata); parentName != "" {
		return parentName, nil
	}

	replicaSetName := ownerReferenceName(pod.Metadata.OwnerReferences, "ReplicaSet")
	if replicaSetName == "" {
		return "", fmt.Errorf("%w: replicaset owner not found in pod %s", ErrAppGroupParentNotFound, pod.Metadata.Name)
	}
	replicaSet, err := s.QueryReplicaSet(ctx, namespace, replicaSetName)
	if err != nil {
		return "", err
	}
	deploymentName := ownerReferenceName(replicaSet.Metadata.OwnerReferences, "Deployment")
	if deploymentName == "" {
		return "", fmt.Errorf("%w: deployment owner not found in replicaset %s", ErrAppGroupParentNotFound, replicaSet.Metadata.Name)
	}
	deployment, err := s.QueryDeployment(ctx, namespace, deploymentName)
	if err != nil {
		return "", err
	}
	if parentName := resolveAppGroupParentNameFromMetadata(deployment.Metadata); parentName != "" {
		return parentName, nil
	}
	return "", fmt.Errorf("%w: owner group metadata not found in deployment %s for pod %s", ErrAppGroupParentNotFound, deployment.Metadata.Name, pod.Metadata.Name)
}

func (s *K8sService) QueryPod(ctx context.Context, namespace, name string) (k8sPod, error) {
	return queryK8sObject[k8sPod](ctx, s, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", url.PathEscape(resolveNamespace(namespace)), url.PathEscape(name)), name, "pod", func(value k8sPod) string { return value.Metadata.Name })
}

func (s *K8sService) QueryReplicaSet(ctx context.Context, namespace, name string) (k8sReplicaSet, error) {
	return queryK8sObject[k8sReplicaSet](ctx, s, fmt.Sprintf("/apis/apps/v1/namespaces/%s/replicasets/%s", url.PathEscape(resolveNamespace(namespace)), url.PathEscape(name)), name, "replicaset", func(value k8sReplicaSet) string { return value.Metadata.Name })
}

func (s *K8sService) QueryDeployment(ctx context.Context, namespace, name string) (k8sDeployment, error) {
	return queryK8sObject[k8sDeployment](ctx, s, fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", url.PathEscape(resolveNamespace(namespace)), url.PathEscape(name)), name, "deployment", func(value k8sDeployment) string { return value.Metadata.Name })
}

func queryK8sObject[T any](ctx context.Context, service *K8sService, path, expectedName, kind string, objectName func(T) string) (T, error) {
	var value T
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, service.baseURL()+path, nil)
	if err != nil {
		return value, err
	}
	resp, err := service.doPanelReq(req)
	if err != nil {
		return value, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return value, err
	}
	if err = json.Unmarshal(body, &value); err != nil {
		return value, err
	}
	if objectName(value) == "" {
		return value, fmt.Errorf("%s not found by name %s", kind, expectedName)
	}
	return value, nil
}

func (s *K8sService) QueryAppGroup(ctx context.Context, namespace, name string) (appGroup, error) {
	namespace = resolveNamespace(namespace)
	name = strings.TrimSpace(name)
	if name == "" {
		return appGroup{}, errors.New("appgroup name is required")
	}

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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return appGroup{}, err
	}
	group := appGroup{}
	if err := json.Unmarshal(body, &group); err != nil {
		return appGroup{}, err
	}
	if group.Metadata.Name == "" {
		return appGroup{}, fmt.Errorf("appgroup not found by name %s", name)
	}
	return group, nil
}

func (s *K8sService) doPanelReq(req *http.Request) (*http.Response, error) {
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	startedAt := time.Now()
	resp, err := s.httpClient().Do(req)
	if err != nil {
		slog.Warn("k8s http request failed", "method", req.Method, "url", req.URL.String(), "duration", time.Since(startedAt), "error", err)
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("failed to request panel, status: %d, response: %s", resp.StatusCode, string(body))
	}
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
	for _, values := range []map[string]string{metadata.Labels, metadata.Annotations} {
		for _, key := range []string{"w7.cc/owner-group-name", "w7.cc/parent-group-name", "w7.cc/group-name"} {
			if value := strings.TrimSpace(values[key]); value != "" {
				return value
			}
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
