package helm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"time"

	catalog "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	"github.com/rancher/rancher/pkg/catalogv2"
	"github.com/rancher/rancher/pkg/catalogv2/git"
	helmhttp "github.com/rancher/rancher/pkg/catalogv2/http"
	catalogcontrollers "github.com/rancher/rancher/pkg/generated/controllers/catalog.cattle.io/v1"
	namespaces "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/condition"
	corev1controllers "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	name2 "github.com/rancher/wrangler/pkg/name"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	maxSize = 100_000
)

var (
	interval = 5 * time.Minute
)

type repoHandler struct {
	// secrets is a cache for Kubernetes secrets used to store Helm chart repository credentials and other sensitive data.
	secrets corev1controllers.SecretCache
	// clusterRepos is a controller interface client for ClusterRepo resources in Rancher. ClusterRepo resources represent Helm chart repositories current state at the cluster scope.
	clusterRepos catalogcontrollers.ClusterRepoController
	// configMaps is a client for interacting with Kubernetes ConfigMap resources. ConfigMaps in the context of Helm repositories can store Helm chart index files.
	configMaps corev1controllers.ConfigMapClient
	// configMapCache is a cache for Kubernetes ConfigMap resources, providing a way to quickly lookup ConfigMap resources in memory.
	configMapCache corev1controllers.ConfigMapCache
	apply          apply.Apply
}

// Register Callbacks

// RegisterRepos function is responsible for registring the handler of repositories in Rancher.
// It sets up a new handler with configuration maps, secrets and cluster repositories and then
// registers the cluster repository status handler.
func RegisterRepos(ctx context.Context,
	apply apply.Apply,
	secrets corev1controllers.SecretCache,
	clusterRepos catalogcontrollers.ClusterRepoController,
	configMap corev1controllers.ConfigMapController,
	configMapCache corev1controllers.ConfigMapCache) {
	h := &repoHandler{
		secrets:        secrets,
		clusterRepos:   clusterRepos,
		configMaps:     configMap,
		configMapCache: configMapCache,
		apply:          apply.WithCacheTypes(configMap).WithStrictCaching().WithSetOwnerReference(false, false),
	}

	catalogcontrollers.RegisterClusterRepoStatusHandler(ctx, clusterRepos,
		condition.Cond(catalog.RepoDownloaded), "helm-clusterrepo-download", h.ClusterRepoDownloadStatusHandler)

}

// RegisterReposForFollowers function is responsible for registering the handler for repositories for follower nodes in Rancher.
// It sets up a new handler with secrets and cluster repositories and then registers the cluster
// repository status handler for followers.
func RegisterReposForFollowers(ctx context.Context,
	secrets corev1controllers.SecretCache,
	clusterRepos catalogcontrollers.ClusterRepoController) {
	h := &repoHandler{
		secrets:      secrets,
		clusterRepos: clusterRepos,
	}

	catalogcontrollers.RegisterClusterRepoStatusHandler(ctx, clusterRepos,
		condition.Cond(catalog.FollowerRepoDownloaded), "helm-clusterrepo-ensure", h.ClusterRepoDownloadEnsureStatusHandler)

}

// Callbacks with system logic

// ClusterRepoDownloadEnsureStatusHandler method ensures that the repository is always up-to-date
// with clusterRepo.Status and Spec
func (r *repoHandler) ClusterRepoDownloadEnsureStatusHandler(repo *catalog.ClusterRepo, status catalog.RepoStatus) (catalog.RepoStatus, error) {
	r.clusterRepos.EnqueueAfter(repo.Name, interval)
	return r.ensure(&repo.Spec, status, &repo.ObjectMeta)
}

// ClusterRepoDownloadStatusHandler is responsible for creating/update of the GitHub folder
// or downloading the index file from the http URL and then creating helm index file and storing it in a configmap.
func (r *repoHandler) ClusterRepoDownloadStatusHandler(repo *catalog.ClusterRepo, status catalog.RepoStatus) (catalog.RepoStatus, error) {
	err := r.ensureIndexConfigMap(repo, &status)
	if err != nil {
		return status, err
	}
	if !shouldRefresh(&repo.Spec, &status) {
		r.clusterRepos.EnqueueAfter(repo.Name, interval)
		return status, nil
	}

	return r.download(&repo.Spec, status, &repo.ObjectMeta, metav1.OwnerReference{
		APIVersion: catalog.SchemeGroupVersion.Group + "/" + catalog.SchemeGroupVersion.Version,
		Kind:       "ClusterRepo",
		Name:       repo.Name,
		UID:        repo.UID,
	})
}

func toOwnerObject(namespace string, owner metav1.OwnerReference) runtime.Object {
	return &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{
			Kind:       owner.Kind,
			APIVersion: owner.APIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner.Name,
			Namespace: namespace,
			UID:       owner.UID,
		},
	}
}

// The ensure method makes sure that a repo exists and is ready based on the provided RepoSpec
// and RepoStatus. The repo's observed generation is set to the metadata's generation, and various
// checks are made to determine whether the repo exists and is ready.
func (r *repoHandler) ensure(repoSpec *catalog.RepoSpec, status catalog.RepoStatus, metadata *metav1.ObjectMeta) (catalog.RepoStatus, error) {
	// related ClusterRepo Status is not updated by download handler yet
	if status.Branch == "" || status.Branch != repoSpec.GitBranch {
		return status, nil
	}

	status.ObservedGeneration = metadata.Generation
	// secret for private or secured repositories
	secret, err := catalogv2.GetSecret(r.secrets, repoSpec, metadata.Namespace)
	if err != nil {
		return status, err
	}

	repo, err := git.BuildRepoConfig(secret, metadata.Namespace, metadata.Name, status.URL, repoSpec.InsecureSkipTLSverify, repoSpec.CABundle)
	if err != nil {
		return status, err
	}

	return status, repo.Ensure(status.Branch)
}

func (r *repoHandler) createOrUpdateMap(namespace, name string, index *repo.IndexFile, owner metav1.OwnerReference) (*corev1.ConfigMap, error) {
	// do this before we normalize the namespace
	ownerObject := toOwnerObject(namespace, owner)

	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	if err := json.NewEncoder(gz).Encode(index); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}

	if namespace == "" {
		namespace = namespaces.System
	}

	var (
		objs  []runtime.Object
		bytes = buf.Bytes()
		left  []byte
		i     = 0
		size  = len(bytes)
	)

	for {
		if len(bytes) > maxSize {
			left = bytes[maxSize:]
			bytes = bytes[:maxSize]
		}

		next := ""
		if len(left) > 0 {
			next = name2.SafeConcatName(owner.Name, fmt.Sprint(i+1), string(owner.UID))
		}

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name2.SafeConcatName(owner.Name, fmt.Sprint(i), string(owner.UID)),
				Namespace:       namespace,
				OwnerReferences: []metav1.OwnerReference{owner},
				Annotations: map[string]string{
					"catalog.cattle.io/next": next,
					// Size ensure the resource version should update even if this is the head of a multipart chunk
					"catalog.cattle.io/size": fmt.Sprint(size),
				},
			},
			BinaryData: map[string][]byte{
				"content": bytes,
			},
		}

		objs = append(objs, cm)
		if len(left) == 0 {
			break
		}

		i++
		bytes = left
		left = nil
	}

	return objs[0].(*corev1.ConfigMap), r.apply.WithOwner(ownerObject).ApplyObjects(objs...)
}

func (r *repoHandler) download(repoSpec *catalog.RepoSpec, status catalog.RepoStatus, metadata *metav1.ObjectMeta, owner metav1.OwnerReference) (catalog.RepoStatus, error) {
	var (
		index  *repo.IndexFile
		commit string
		err    error
	)

	status.ObservedGeneration = metadata.Generation

	secret, err := catalogv2.GetSecret(r.secrets, repoSpec, metadata.Namespace)
	if err != nil {
		return status, err
	}

	downloadTime := metav1.Now()
	// Decide if we need to perform a download operation
	if repoSpec.GitRepo != "" {
		repo, err := git.BuildRepoConfig(secret, metadata.Namespace, metadata.Name, repoSpec.GitRepo, repoSpec.InsecureSkipTLSverify, repoSpec.CABundle)
		if err != nil {
			return status, err
		}
		// We need a download operation
		// if we don't have a Index ConfigMap name, that means, we have not cloned the repository yet.
		// if we have one, we just need to update it
		if status.IndexConfigMapName == "" {
			commit, err = repo.Head(repoSpec.GitBranch)
			if err != nil {
				return status, err
			}
			status.URL = repoSpec.GitRepo
			status.Branch = repoSpec.GitBranch
		} else {
			commit, err = repo.CheckUpdate(repoSpec.GitBranch, settings.SystemCatalog.Get())
			if err != nil {
				return status, err
			}
			status.URL = repoSpec.GitRepo
			status.Branch = repoSpec.GitBranch
			if status.Commit == commit {
				status.DownloadTime = downloadTime
				return status, nil
			}
		}
		// regardless of which download operation took place, build or get the new index
		index, err = git.BuildOrGetIndex(metadata.Namespace, metadata.Name, repoSpec.GitRepo)
		if err != nil || index == nil {
			return status, err
		}
	} else if repoSpec.URL != "" {
		status.URL = repoSpec.URL
		status.Branch = ""
		index, err = helmhttp.DownloadIndex(secret, repoSpec.URL, repoSpec.CABundle, repoSpec.InsecureSkipTLSverify, repoSpec.DisableSameOriginCheck)
	} else {
		return status, nil
	}
	if err != nil || index == nil {
		return status, err
	}

	index.SortEntries()

	name := status.IndexConfigMapName
	if name == "" {
		name = owner.Name
	}

	cm, err := r.createOrUpdateMap(metadata.Namespace, name, index, owner)
	if err != nil {
		return status, err
	}

	status.IndexConfigMapName = cm.Name
	status.IndexConfigMapNamespace = cm.Namespace
	status.IndexConfigMapResourceVersion = cm.ResourceVersion
	status.DownloadTime = downloadTime
	status.Commit = commit
	return status, nil
}

func (r *repoHandler) ensureIndexConfigMap(repo *catalog.ClusterRepo, status *catalog.RepoStatus) error {
	// Charts from the clusterRepo will be unavailable if the IndexConfigMap recorded in the status does not exist.
	// By resetting the value of IndexConfigMapName, IndexConfigMapNamespace, IndexConfigMapResourceVersion to "",
	// the method shouldRefresh will return true and trigger the rebuild of the IndexConfigMap and accordingly update the status.
	if repo.Spec.GitRepo != "" && status.IndexConfigMapName != "" {
		_, err := r.configMapCache.Get(status.IndexConfigMapNamespace, status.IndexConfigMapName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				status.IndexConfigMapName = ""
				status.IndexConfigMapNamespace = ""
				status.IndexConfigMapResourceVersion = ""
				return nil
			}
			return err
		}
	}
	return nil
}

func shouldRefresh(spec *catalog.RepoSpec, status *catalog.RepoStatus) bool {
	// status has an older branch then target branch
	if spec.GitRepo != "" && status.Branch != spec.GitBranch {
		return true
	}
	// repository URL changed for http(s) URL to an index generated by Helm
	if spec.URL != "" && spec.URL != status.URL {
		return true
	}
	// repository URL changed for Git repository containing Helm chart or cluster template definitions
	if spec.GitRepo != "" && spec.GitRepo != status.URL {
		return true
	}
	// configMap to be updated or created (holds chart versions)
	if status.IndexConfigMapName == "" {
		return true
	}
	// forced update requested by user (refresh button)
	if spec.ForceUpdate != nil && spec.ForceUpdate.After(status.DownloadTime.Time) && spec.ForceUpdate.Time.Before(time.Now()) {
		return true
	}
	refreshTime := time.Now().Add(-interval)
	return refreshTime.After(status.DownloadTime.Time)
}
