package logic

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
	"golang.org/x/sync/singleflight"
)

const podCacheResolveTimeout = 30 * time.Second

// PodCache is an in-memory projection of Pod watch events, indexed by source
// IP. It deliberately owns the cache policy while K8sService only observes
// Kubernetes and emits PodEvent values.
type PodCache struct {
	service *k8s.K8sService

	mu       sync.RWMutex
	podsByIP map[string]k8s.K8sPod
	requests singleflight.Group
}

func NewPodCache(service *k8s.K8sService) *PodCache {
	return &PodCache{
		service:  service,
		podsByIP: make(map[string]k8s.K8sPod),
	}
}

func (c *PodCache) Start(ctx context.Context, namespace string) error {
	if c.service == nil {
		return fmt.Errorf("kubernetes service is not configured")
	}
	return c.service.WatchPods(ctx, namespace, func(event k8s.PodEvent) {
		c.apply(namespace, event)
	})
}

func (c *PodCache) Get(ctx context.Context, namespace, remoteIP string) (k8s.K8sPod, error) {
	key := namespace + ":" + remoteIP
	if pod, ok := c.get(key); ok {
		return pod, nil
	}
	if c.service == nil {
		return k8s.K8sPod{}, fmt.Errorf("kubernetes service is not configured")
	}

	ch := c.requests.DoChan(key, func() (any, error) {
		if pod, ok := c.get(key); ok {
			return pod, nil
		}

		resolveCtx, cancel := context.WithTimeout(context.Background(), podCacheResolveTimeout)
		defer cancel()
		pod, err := c.service.QueryPodByIP(resolveCtx, namespace, remoteIP)
		slog.Info("get pod by ip", "remote_ip", remoteIP, "pod", pod, "err", err)
		if err != nil {
			return k8s.K8sPod{}, err
		}
		if pod.Metadata.UID == "" || pod.Status.PodIP == "" {
			return k8s.K8sPod{}, fmt.Errorf("pod UID not found by remote ip %s", remoteIP)
		}

		return pod, nil
	})

	select {
	case result := <-ch:
		if result.Err != nil {
			return k8s.K8sPod{}, result.Err
		}
		pod, ok := result.Val.(k8s.K8sPod)
		if !ok {
			return k8s.K8sPod{}, fmt.Errorf("unexpected Pod cache value %T", result.Val)
		}
		return pod, nil
	case <-ctx.Done():
		return k8s.K8sPod{}, ctx.Err()
	}
}

func (c *PodCache) get(key string) (k8s.K8sPod, bool) {
	c.mu.RLock()
	pod := c.podsByIP[key]
	c.mu.RUnlock()
	return pod, pod.Metadata.UID != ""
}

func (c *PodCache) apply(namespace string, event k8s.PodEvent) {
	slog.Info("podcache apply", "namespace", namespace, "pod", event.Pod.Metadata.Name, "uid", event.Pod.Metadata.UID, "ip", event.Pod.Status.PodIP)

	key := namespace + ":" + event.Pod.Status.PodIP
	c.mu.Lock()
	defer c.mu.Unlock()

	switch event.Type {
	case k8s.PodAdded, k8s.PodUpdated:
		if event.Pod.Metadata.UID == "" || event.Pod.Status.PodIP == "" {
			return
		}
		c.podsByIP[key] = event.Pod
	case k8s.PodDeleted:
		if cachedPod, ok := c.podsByIP[key]; ok && cachedPod.Metadata.UID == event.Pod.Metadata.UID {
			delete(c.podsByIP, key)
		}
	}
}
