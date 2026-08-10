package logic

import (
	"context"
	"fmt"
	"time"

	cache "github.com/patrickmn/go-cache"
	"github.com/w7panel/w7panel-cloudnoauth/common/service/k8s"
	"golang.org/x/sync/singleflight"
)

const credentialResolveTimeout = 5 * time.Second

type Credential struct {
	K8sService       *k8s.K8sService
	Namespace        string
	PodName          string
	Cache            *cache.Cache
	NegativeCacheTTL time.Duration
	requests         singleflight.Group
}

type credentialCacheItem struct {
	Credential k8s.AppCredential
	Err        error
}

func (logic *Credential) Resolve(ctx context.Context) (k8s.AppCredential, error) {
	if logic.PodName == "" {
		return k8s.AppCredential{}, fmt.Errorf("pod name is not configured")
	}
	if logic.K8sService == nil {
		return k8s.AppCredential{}, fmt.Errorf("kubernetes service is not configured")
	}

	cacheKey := fmt.Sprintf("%s:pod:%s", logic.Namespace, logic.PodName)
	if credential, err, ok := logic.getCache(cacheKey); ok {
		return credential, err
	}

	ch := logic.requests.DoChan(cacheKey, func() (any, error) {
		if credential, err, ok := logic.getCache(cacheKey); ok {
			return credential, err
		}

		resolveCtx, cancel := context.WithTimeout(context.Background(), credentialResolveTimeout)
		defer cancel()

		credential, err := logic.K8sService.ResolveAppCredentialForPod(resolveCtx, logic.Namespace, logic.PodName)
		if err != nil {
			if k8s.IsSkippableCredentialError(err) {
				logic.setNegativeCache(cacheKey, credential, err)
			}
			return k8s.AppCredential{}, err
		}

		logic.setCache(cacheKey, credential, nil)
		return credential, nil
	})

	select {
	case result := <-ch:
		if result.Err != nil {
			return k8s.AppCredential{}, result.Err
		}
		credential, ok := result.Val.(k8s.AppCredential)
		if !ok {
			return k8s.AppCredential{}, fmt.Errorf("unexpected credential cache value type %T", result.Val)
		}
		return credential, nil
	case <-ctx.Done():
		return k8s.AppCredential{}, ctx.Err()
	}
}

func (logic *Credential) getCache(key string) (k8s.AppCredential, error, bool) {
	if logic.Cache == nil {
		return k8s.AppCredential{}, nil, false
	}

	value, ok := logic.Cache.Get(key)
	if !ok {
		return k8s.AppCredential{}, nil, false
	}

	item, ok := value.(credentialCacheItem)
	if !ok {
		logic.Cache.Delete(key)
		return k8s.AppCredential{}, nil, false
	}

	return item.Credential, item.Err, true
}

func (logic *Credential) setCache(key string, credential k8s.AppCredential, err error) {
	if logic.Cache == nil {
		return
	}

	logic.Cache.SetDefault(key, credentialCacheItem{
		Credential: credential,
		Err:        err,
	})
}

func (logic *Credential) setNegativeCache(key string, credential k8s.AppCredential, err error) {
	if logic.Cache == nil {
		return
	}

	ttl := logic.NegativeCacheTTL
	if ttl <= 0 {
		ttl = cache.DefaultExpiration
	}
	logic.Cache.Set(key, credentialCacheItem{
		Credential: credential,
		Err:        err,
	}, ttl)
}
