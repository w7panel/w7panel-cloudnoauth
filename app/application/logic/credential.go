package logic

import (
	"context"
	"fmt"

	cache "github.com/patrickmn/go-cache"
	"github.com/w7panel/w7panel-appid-proxy/common/service/k8s"
)

type Credential struct {
	K8sService *k8s.K8sService
	Namespace  string
	Cache      *cache.Cache
}

type credentialCacheItem struct {
	Credential k8s.AppCredential
	Err        error
}

func (logic *Credential) ResolveByRemoteIP(ctx context.Context, remoteIP string) (k8s.AppCredential, error) {
	cacheKey := fmt.Sprintf("%s:%s", logic.Namespace, remoteIP)
	if credential, err, ok := logic.getCache(cacheKey); ok {
		return credential, err
	}

	credential, err := logic.K8sService.ResolveAppCredential(ctx, logic.Namespace, remoteIP)
	if err != nil {
		if k8s.IsSkippableCredentialError(err) {
			logic.setCache(cacheKey, credential, err)
		}
		return k8s.AppCredential{}, err
	}

	logic.setCache(cacheKey, credential, nil)
	return credential, nil
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
