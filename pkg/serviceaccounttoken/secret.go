package serviceaccounttoken

import (
	"context"
	"fmt"
	"sync"
	"time"

	corecontrollers "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	"github.com/sirupsen/logrus"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/ptr"
)

const (
	// ServiceAccountSecretLabel is the label used to search for the secret belonging to a service account.
	ServiceAccountSecretLabel = "cattle.io/service-account.name"

	serviceAccountSecretAnnotation = "kubernetes.io/service-account.name"

	// LeasePrefix is the name of the lease used to manage concurrency
	LeasePrefix = "sa-token-lease-"
)

var lockMap sync.Map

func getLock(key string) *sync.Mutex {
	actual, _ := lockMap.LoadOrStore(key, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

// secretLister is an abstraction over any kind of secret lister.
// The caller can use any cache or client it has available, whether that is from norman, wrangler, or client-go,
// as long as it can wrap it in a simplified lambda with this signature.
type secretLister func(namespace string, selector labels.Selector) ([]*v1.Secret, error)

// EnsureSecretForServiceAccount gets or creates a service account token Secret for the provided Service Account.
// For k8s <1.24, the secret is automatically generated for the service account. For >=1.24, we need to generate it explicitly.
func EnsureSecretForServiceAccount(ctx context.Context, secretsCache corecontrollers.SecretCache, clientSet kubernetes.Interface, sa *v1.ServiceAccount) (*v1.Secret, error) {
	if sa == nil {
		return nil, fmt.Errorf("could not ensure secret for invalid service account")
	}

	// Lock avoids multiple calls to this func at the same time and creation of same resources multiple times
	// Mutex is a addition to the Lease, it helps sync within the pod and avoid multiple Lease waits from the same pod
	mutex := getLock(fmt.Sprintf("%v-%v", sa.Namespace, sa.Name))
	mutex.Lock()
	defer mutex.Unlock()

	// Acquire lease
	// acquireLease has a wait.Backoff until the lease is acquired, so this will be a blocking func call
	if err := acquireLease(ctx, clientSet, sa.Namespace, sa.Name); err != nil {
		return nil, fmt.Errorf("error acquiring lease: %w", err)
	}
	defer func() {
		if err := releaseLease(ctx, clientSet, sa.Namespace, sa.Name); err != nil {
			logrus.Errorf("error releasing lease: %v", err)
		}
	}()

	secretClient := clientSet.CoreV1().Secrets(sa.Namespace)
	var secretLister secretLister
	if secretsCache != nil {
		secretLister = secretsCache.List
	} else {
		secretLister = func(_ string, selector labels.Selector) ([]*v1.Secret, error) {
			secretList, err := secretClient.List(ctx, metav1.ListOptions{
				LabelSelector: selector.String(),
			})
			if err != nil {
				return nil, err
			}
			result := make([]*v1.Secret, len(secretList.Items))
			for i := range secretList.Items {
				result[i] = &secretList.Items[i]
			}
			return result, nil
		}
	}
	secret, err := ServiceAccountSecret(ctx, sa, secretLister, secretClient)
	if err != nil {
		return nil, fmt.Errorf("error looking up secret for service account [%s:%s]: %w", sa.Namespace, sa.Name, err)
	}
	if secret == nil {
		sc := SecretTemplate(sa)
		secret, err = secretClient.Create(ctx, sc, metav1.CreateOptions{})
		if err != nil {
			return nil, fmt.Errorf("error ensuring secret for service account [%s:%s]: %w", sa.Namespace, sa.Name, err)
		}
	}
	if len(secret.Data[v1.ServiceAccountTokenKey]) > 0 {
		return secret, nil
	}
	logrus.Infof("EnsureSecretForServiceAccount: waiting for secret [%s] to be populated with token", secret.Name)
	backoff := wait.Backoff{
		Duration: 2 * time.Millisecond,
		Cap:      100 * time.Millisecond,
		Steps:    50,
	}
	err = wait.ExponentialBackoff(backoff, func() (bool, error) {
		var err error
		// use the secret client, rather than the secret getter, to circumvent the cache
		secret, err = secretClient.Get(ctx, secret.Name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("error ensuring secret for service account [%s:%s]: %w", sa.Namespace, sa.Name, err)
		}
		if len(secret.Data[v1.ServiceAccountTokenKey]) > 0 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error ensuring secret for service account [%s:%s]: %w", sa.Namespace, sa.Name, err)
	}
	return secret, nil
}

// SecretTemplate generate a template of service-account-token Secret for the provided Service Account.
func SecretTemplate(sa *v1.ServiceAccount) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: serviceAccountSecretPrefix(sa),
			Namespace:    sa.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "v1",
					Kind:       "ServiceAccount",
					Name:       sa.Name,
					UID:        sa.UID,
				},
			},
			Annotations: map[string]string{
				serviceAccountSecretAnnotation: sa.Name,
			},
			Labels: map[string]string{
				ServiceAccountSecretLabel: sa.Name,
			},
		},
		Type: v1.SecretTypeServiceAccountToken,
	}

}

// serviceAccountSecretPrefix returns the prefix that will be used to generate the secret for the given service account.
func serviceAccountSecretPrefix(sa *v1.ServiceAccount) string {
	return fmt.Sprintf("%s-token-", sa.Name)
}

// ServiceAccountSecret returns the secret for the given Service Account.
// If there are more than one, it returns the first. Can return a nil secret
// and a nil error if no secret is found
func ServiceAccountSecret(ctx context.Context, sa *v1.ServiceAccount, secretLister secretLister, secretClient clientv1.SecretInterface) (*v1.Secret, error) {
	if sa == nil {
		return nil, fmt.Errorf("cannot get secret for nil service account")
	}
	secrets, err := secretLister(sa.Namespace, labels.SelectorFromSet(map[string]string{
		ServiceAccountSecretLabel: sa.Name,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not get secrets for service account: %w", err)
	}
	if len(secrets) < 1 {
		return nil, nil
	}
	var result *v1.Secret
	for _, s := range secrets {
		if isSecretForServiceAccount(s, sa) {
			if result == nil {
				result = s
			}
			continue
		}
		logrus.Warnf("EnsureSecretForServiceAccount: secret [%s:%s] is invalid for service account [%s], deleting", s.Namespace, s.Name, sa.Name)
		err = secretClient.Delete(ctx, s.Name, metav1.DeleteOptions{})
		if err != nil {
			// we don't want to return the delete failure since the success/failure of the cleanup shouldn't affect
			// the ability of the caller to use any identified, valid secret
			logrus.Errorf("unable to delete secret [%s:%s]: %v", s.Namespace, s.Name, err)
		}
	}
	return result, nil
}

func isSecretForServiceAccount(secret *v1.Secret, sa *v1.ServiceAccount) bool {
	if secret.Type != v1.SecretTypeServiceAccountToken {
		return false
	}
	annotations := secret.Annotations
	annotation := annotations[serviceAccountSecretAnnotation]
	return sa.Name == annotation
}

func acquireLease(ctx context.Context, clientSet kubernetes.Interface, namespace, name string) error {
	leaseClient := clientSet.CoordinationV1().Leases(namespace)
	leaseDuration := int32(30)
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprint(LeasePrefix + name),
			Namespace: namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("serviceaccounttoken-controller"),
			LeaseDurationSeconds: ptr.To(leaseDuration),
		},
	}
	// Wait for the Lease to be granted
	backoff := wait.Backoff{
		Duration: 500 * time.Millisecond,
		Factor:   1.0,
		Jitter:   1.0,
		Steps:    50,
	}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := leaseClient.Create(ctx, lease, metav1.CreateOptions{})
		// if create was success, meaning we got the lease
		if err == nil {
			return true, nil
		}
		// if lease already exists, another request has this lease, continue to wait
		if errors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		return fmt.Errorf("error acquiring the lease for %v: %w", name, err)
	}
	return nil
}

func releaseLease(ctx context.Context, clientSet kubernetes.Interface, namespace, name string) error {
	leaseClient := clientSet.CoordinationV1().Leases(namespace)
	err := leaseClient.Delete(ctx, fmt.Sprint(LeasePrefix+name), metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting lease: %w", err)
	}
	return nil
}
