package kubernetes

import (
	"context"
	"sync"
	"time"

	"github.com/rancher/dynamiclistener"
	"github.com/rancher/wrangler-api/pkg/generated/controllers/core"
	v1controller "github.com/rancher/wrangler-api/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/start"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CoreGetter func() *core.Factory

func New(ctx context.Context, core CoreGetter, namespace, name string, backing dynamiclistener.TLSStorage) dynamiclistener.TLSStorage {
	storage := &storage{
		name:      name,
		namespace: namespace,
		storage:   backing,
		ctx:       ctx,
	}

	// lazy init
	go func() {
		for {
			core := core()
			if core != nil {
				storage.init(core.Core().V1().Secret())
				start.All(ctx, 5, core)
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()

	return storage
}

type storage struct {
	sync.Mutex

	namespace, name string
	storage         dynamiclistener.TLSStorage
	secrets         v1controller.SecretClient
	ctx             context.Context
}

func (s *storage) init(secrets v1controller.SecretController) {
	s.Lock()
	defer s.Unlock()

	secrets.OnChange(s.ctx, "tls-storage", func(key string, secret *v1.Secret) (*v1.Secret, error) {
		if secret == nil {
			return nil, nil
		}
		if secret.Namespace == s.namespace && secret.Name == s.name {
			if err := s.Update(secret); err != nil {
				return nil, err
			}
		}

		return secret, nil
	})
	s.secrets = secrets

	secret, err := s.storage.Get()
	if err == nil && secret != nil {
		s.saveInK8s(secret)
	}
}

func (s *storage) Get() (*v1.Secret, error) {
	s.Lock()
	defer s.Unlock()

	return s.storage.Get()
}

func (s *storage) targetSecret() (*v1.Secret, error) {
	existingSecret, err := s.secrets.Get(s.namespace, s.name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      s.name,
				Namespace: s.namespace,
			},
		}, nil
	}
	return existingSecret, err
}

func (s *storage) saveInK8s(secret *v1.Secret) (*v1.Secret, error) {
	if s.secrets == nil {
		return secret, nil
	}

	targetSecret, err := s.targetSecret()
	if err != nil {
		return nil, err
	}

	if equality.Semantic.DeepEqual(targetSecret.Annotations, secret.Annotations) &&
		equality.Semantic.DeepEqual(targetSecret.Data, secret.Data) {
		return secret, nil
	}

	targetSecret.Annotations = secret.Annotations
	targetSecret.Type = v1.SecretTypeTLS
	targetSecret.Data = secret.Data

	if targetSecret.UID == "" {
		logrus.Infof("Creating new TLS secret for %v (count: %d)", targetSecret.Name, len(targetSecret.Data)-1)
		return s.secrets.Create(targetSecret)
	} else {
		logrus.Infof("Updating TLS secret for %v (count: %d)", targetSecret.Name, len(targetSecret.Data)-1)
		return s.secrets.Update(targetSecret)
	}
}

func (s *storage) Update(secret *v1.Secret) (err error) {
	s.Lock()
	defer s.Unlock()

	for i := 0; i < 3; i++ {
		secret, err = s.saveInK8s(secret)
		if errors.IsConflict(err) {
			continue
		} else if err != nil {
			return err
		}
		break
	}

	if err != nil {
		return err
	}

	// update underlying storage
	return s.storage.Update(secret)
}
