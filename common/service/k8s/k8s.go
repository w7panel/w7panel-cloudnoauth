package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ErrAppCredentialNotFound  = errors.New("app credential annotation not found")
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
}

type AppCredential struct {
	RemoteIP  string
	PodName   string
	AppGroup  string
	AppID     string
	AppSecret string
}

type k8sList[T any] struct {
	Items []T `json:"items"`
}

type k8sObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

type k8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Status   struct {
		PodIP string `json:"podIP"`
	} `json:"status"`
}

type appGroup struct {
	Metadata k8sObjectMeta `json:"metadata"`
}

func NewK8sService(k8sConfig string) (*K8sService, error) {
	config, err := makeK8sConfig(k8sConfig)
	if err != nil {
		return nil, err
	}
	config.Timeout = 10 * time.Second

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

func (s *K8sService) ResolveAppCredential(ctx context.Context, namespace string, remoteIP string) (AppCredential, error) {
	pod, err := s.QueryPodByIP(ctx, namespace, remoteIP)
	if err != nil {
		return AppCredential{}, err
	}

	parentName, err := resolveAppGroupParentName(pod)
	if err != nil {
		return AppCredential{}, err
	}

	group, err := s.QueryAppGroupByParent(ctx, namespace, parentName)
	if err != nil {
		return AppCredential{}, err
	}

	appID := firstAnnotation(group.Metadata.Annotations, []string{"w7.cc/appid", "w7.cc/app-id", "appid", "app_id"})
	appSecret := firstAnnotation(group.Metadata.Annotations, []string{"w7.cc/appsecret", "w7.cc/app-secret", "appsecret", "app_secret"})
	if appID == "" || appSecret == "" {
		return AppCredential{}, fmt.Errorf("%w: appid or appsecret annotation not found in appgroup %s", ErrAppCredentialNotFound, group.Metadata.Name)
	}

	return AppCredential{
		RemoteIP:  remoteIP,
		PodName:   pod.Metadata.Name,
		AppGroup:  group.Metadata.Name,
		AppID:     appID,
		AppSecret: appSecret,
	}, nil
}

func (s *K8sService) QueryPodByIP(ctx context.Context, namespace string, remoteIP string) (k8sPod, error) {
	var pods k8sList[k8sPod]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/api/v1/namespaces/%s/pods?fieldSelector=%s",
		s.baseURL(),
		url.PathEscape(resolveNamespace(namespace)),
		url.QueryEscape("status.podIP="+remoteIP),
	), nil)
	if err != nil {
		return k8sPod{}, err
	}

	resp, err := s.doPanelReq(req)
	if err != nil {
		return k8sPod{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return k8sPod{}, err
	}
	if err = json.Unmarshal(respBody, &pods); err != nil {
		return k8sPod{}, err
	}

	for _, pod := range pods.Items {
		if pod.Status.PodIP == remoteIP {
			return pod, nil
		}
	}

	return k8sPod{}, fmt.Errorf("pod not found by remote ip %s", remoteIP)
}

func (s *K8sService) QueryAppGroupByParent(ctx context.Context, namespace string, parentName string) (appGroup, error) {
	labelSelector := fmt.Sprintf("w7.cc/parent=%s", parentName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/apis/w7panel.w7.com/v1alpha1/namespaces/%s/appgroups?labelSelector=%s",
		s.baseURL(),
		url.PathEscape(resolveNamespace(namespace)),
		url.QueryEscape(labelSelector),
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

	groups := &k8sList[appGroup]{}
	if err = json.Unmarshal(respBody, groups); err != nil {
		return appGroup{}, err
	}
	if len(groups.Items) > 0 {
		return groups.Items[0], nil
	}

	return appGroup{}, fmt.Errorf("%w: parent %s", ErrAppGroupNotFound, parentName)
}

func (s *K8sService) QueryAppGroup(ctx context.Context, namespace string, name string) (appGroup, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(
		"%s/apis/w7panel.w7.com/v1alpha1/namespaces/%s/appgroups/%s",
		s.baseURL(),
		url.PathEscape(resolveNamespace(namespace)),
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
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("failed to request panel, status: %d, response: %s", resp.StatusCode, string(respBody))
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

func resolveAppGroupParentName(pod k8sPod) (string, error) {
	for _, key := range []string{"w7.cc/group-name", "w7.cc/name", "app.kubernetes.io/instance", "app"} {
		if value := pod.Metadata.Labels[key]; value != "" {
			return value, nil
		}
	}

	return "", fmt.Errorf("%w: appgroup parent label not found in pod %s", ErrAppGroupParentNotFound, pod.Metadata.Name)
}

func firstAnnotation(annotations map[string]string, keys []string) string {
	for _, key := range keys {
		if value := annotations[key]; value != "" {
			return value
		}
	}
	return ""
}
